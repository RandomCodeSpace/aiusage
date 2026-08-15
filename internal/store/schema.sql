-- aiusage schema. Append-only, audit-friendly.
--
-- usage_events is the IMMUTABLE source of truth for all reported usage. Rows are
-- never updated or deleted (enforced by triggers). Idempotency is via the UNIQUE
-- dedup_key (collectors INSERT OR IGNORE). This is what guarantees that later
-- agent cleanup/compaction can never reduce a past interval's reported total:
-- re-polling a shrunk source only ever inserts NEW dedup keys.
--
-- aggregate_state is MUTABLE accumulator state (one row per growing-record cell)
-- used to derive positive deltas for sources whose per-record totals grow
-- between polls (hermes sessions, gemini/agy per-turn snapshots). It is NOT
-- history — the immutable history is the sequence of delta rows in usage_events.
--
-- usage_rollup is a MUTABLE DERIVED summary of usage_events. It is not
-- history either: every row is reproducible from the ledger, and it may be
-- dropped and rebuilt at any time.
--
-- activity_events is a SECOND append-only ledger, of agent ACTIVITY rather than
-- tokens: which tool was called, which skill was invoked, which hook fired. It
-- is a sibling of usage_events, never a part of it, and it stores names and
-- counts ONLY — no tool inputs and no raw payload of any kind.

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS schema_meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS usage_events (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,
  dedup_key             TEXT    NOT NULL UNIQUE,
  tool                  TEXT    NOT NULL,                 -- categorisation: which agent CLI
  model                 TEXT    NOT NULL DEFAULT '',      -- categorisation: which model
  session_id            TEXT    NOT NULL DEFAULT '',
  project               TEXT    NOT NULL DEFAULT '',      -- workspace / cwd
  event_time_unix       INTEGER NOT NULL,                 -- UTC seconds; when usage occurred
  observed_time_unix    INTEGER NOT NULL,                 -- UTC seconds; when daemon stored it
  input_tokens          INTEGER NOT NULL DEFAULT 0,
  output_tokens         INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens      INTEGER NOT NULL DEFAULT 0,       -- informational (subset of output for some providers)
  total_tokens          INTEGER NOT NULL DEFAULT 0,       -- provider-authoritative total (summed for headlines)
  request_id            TEXT    NOT NULL DEFAULT '',
  message_id            TEXT    NOT NULL DEFAULT '',
  source_path           TEXT    NOT NULL DEFAULT '',
  kind                  TEXT    NOT NULL DEFAULT 'usage', -- 'usage' | 'adjustment'
  raw                   TEXT,                             -- optional raw provider JSON (audit)
  -- v3 columns, appended in migration order so a migrated database and a fresh
  -- one carry the same column layout.
  provider              TEXT    NOT NULL DEFAULT '',      -- billing identity ('' = unknown)
  service_tier          TEXT    NOT NULL DEFAULT '',      -- provider service tier (batch/priority change the bill)
  cost_micro_usd        INTEGER,                          -- millionths of USD; NULL = unpriced, NEVER 0
  price_source          TEXT    NOT NULL DEFAULT '',      -- which price table stamped the cost
  CHECK (input_tokens >= 0 AND output_tokens >= 0 AND cache_creation_tokens >= 0
         AND cache_read_tokens >= 0 AND reasoning_tokens >= 0 AND total_tokens >= 0)
);

CREATE INDEX IF NOT EXISTS idx_events_event_time ON usage_events(event_time_unix);
CREATE INDEX IF NOT EXISTS idx_events_tool        ON usage_events(tool);
CREATE INDEX IF NOT EXISTS idx_events_model       ON usage_events(model);
CREATE INDEX IF NOT EXISTS idx_events_session     ON usage_events(session_id);
CREATE INDEX IF NOT EXISTS idx_events_tool_time   ON usage_events(tool, event_time_unix);

-- Immutability: reject any mutation of historical rows, even from a buggy path.
CREATE TRIGGER IF NOT EXISTS trg_events_no_update
BEFORE UPDATE ON usage_events
BEGIN SELECT RAISE(ABORT, 'usage_events is append-only: UPDATE forbidden'); END;

CREATE TRIGGER IF NOT EXISTS trg_events_no_delete
BEFORE DELETE ON usage_events
BEGIN SELECT RAISE(ABORT, 'usage_events is append-only: DELETE forbidden'); END;

-- Mutable per-source incremental-collection state (v2). Losing a row only
-- costs a full re-read of that source; the collector writes it in the same
-- transaction as the events it accounts for so it can never outrun the data.
CREATE TABLE IF NOT EXISTS source_checkpoints (
  tool        TEXT    NOT NULL,
  source_path TEXT    NOT NULL,
  size_bytes  INTEGER NOT NULL DEFAULT 0,
  mtime_ns    INTEGER NOT NULL DEFAULT 0,
  read_offset INTEGER NOT NULL DEFAULT 0,
  watermark   INTEGER NOT NULL DEFAULT 0,
  state       TEXT,
  PRIMARY KEY (tool, source_path)
);

-- Derived rollup (v4): the summary the reporting surfaces read instead of
-- scanning the ledger. DERIVED ONLY, never authoritative - RebuildRollup
-- reproduces every row from usage_events, and nothing may read it as history.
--
-- The bucket key is the UTC 15-MINUTE bucket the events fall in, folded to
-- local wall clock on READ, in SQL, exactly the way query.go folds
-- event_time_unix. Rolling up by local time would bake the writing machine's
-- calendar into stored data. The width is 15 minutes rather than an hour
-- because every real-world UTC offset is a whole number of quarter hours:
-- hourly keys misplace half-hour zones (Asia/Kolkata at +05:30 among them),
-- where one hour bucket straddles two local buckets and would land wholly in
-- the earlier one. Session id, provider, service tier and resolution below the
-- bucket width are deliberately absent: those queries go to the ledger.
--
-- cost_micro_usd sums ONLY the costs actually stamped on events;
-- unpriced_events counts the rows that carry none, so a partial cost can never
-- be read as a bill.
CREATE TABLE IF NOT EXISTS usage_rollup (
  bucket_start_unix     INTEGER NOT NULL,             -- UTC 15-minute bucket start, unix seconds
  tool                  TEXT    NOT NULL,
  model                 TEXT    NOT NULL DEFAULT '',
  project               TEXT    NOT NULL DEFAULT '',
  input_tokens          INTEGER NOT NULL DEFAULT 0,
  output_tokens         INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens          INTEGER NOT NULL DEFAULT 0,
  events                INTEGER NOT NULL DEFAULT 0,
  cost_micro_usd        INTEGER NOT NULL DEFAULT 0,
  unpriced_events       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (bucket_start_unix, tool, model, project)
) WITHOUT ROWID;

-- Agent activity ledger (v5): one row per observed tool call, skill invocation
-- or hook firing. Append-only on the same terms as usage_events (UNIQUE
-- dedup_key + no-UPDATE/no-DELETE triggers), and deliberately a SEPARATE table:
-- activity is not token accounting, and a call that cost nothing has no
-- business in the ledger that answers "what did this cost".
--
-- PRIVACY BY CONSTRUCTION: there is no column for a tool's input and no raw
-- column, so a command string, a file path or a prompt has nowhere to land even
-- if an adapter tried. The only value read out of a call's input anywhere in
-- this project is the skill NAME of a Skill call, which is the recorded fact.
--
-- COST ATTRIBUTION: no token columns. usage_dedup_key references the
-- usage_events row whose provider record contained this call, and calls_in_turn
-- records how many calls shared that one usage object. Tokens and cost are
-- derived on READ by joining the ledger and dividing by calls_in_turn. One
-- assistant turn commonly emits several tool_use blocks against a SINGLE usage
-- object, so copying the turn's tokens onto each row would multiply the real
-- cost by the number of calls in the turn; keeping the number where it already
-- lives makes that inflation structurally impossible.
--
-- usage_dedup_key is NOT a foreign key on purpose. A call is an observed fact
-- even when its usage row was skipped (poison row, unparseable model) or simply
-- predates activity collection, and some sources give no join at all (codex
-- records function calls and token counts in unrelated records; hooks carry no
-- usage). Those rows store '' and the read path left-joins, so a missing
-- partner contributes no cost rather than losing the call.
CREATE TABLE IF NOT EXISTS activity_events (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  dedup_key          TEXT    NOT NULL UNIQUE,
  tool               TEXT    NOT NULL,                 -- which agent CLI
  kind               TEXT    NOT NULL,                 -- 'tool' | 'skill' | 'hook'
  name               TEXT    NOT NULL,                 -- tool/skill/hook name, verbatim
  session_id         TEXT    NOT NULL DEFAULT '',
  project            TEXT    NOT NULL DEFAULT '',      -- workspace / cwd
  model              TEXT    NOT NULL DEFAULT '',      -- model of the turn, when named
  event_time_unix    INTEGER NOT NULL,                 -- UTC seconds; when the call happened
  observed_time_unix INTEGER NOT NULL,                 -- UTC seconds; when daemon stored it
  usage_dedup_key    TEXT    NOT NULL DEFAULT '',      -- join handle to usage_events.dedup_key; '' = no cost join
  message_id         TEXT    NOT NULL DEFAULT '',
  request_id         TEXT    NOT NULL DEFAULT '',
  turn_seq           INTEGER NOT NULL DEFAULT 0,       -- 0-based index of this call within its turn
  calls_in_turn      INTEGER NOT NULL DEFAULT 1,       -- calls sharing the turn's usage object
  source_path        TEXT    NOT NULL DEFAULT '',
  CHECK (kind IN ('tool','skill','hook')),
  CHECK (name <> ''),
  CHECK (turn_seq >= 0 AND calls_in_turn >= 1 AND turn_seq < calls_in_turn)
);

CREATE INDEX IF NOT EXISTS idx_activity_event_time ON activity_events(event_time_unix);
CREATE INDEX IF NOT EXISTS idx_activity_tool_name  ON activity_events(tool, name);
CREATE INDEX IF NOT EXISTS idx_activity_name_time  ON activity_events(name, event_time_unix);
CREATE INDEX IF NOT EXISTS idx_activity_kind_time  ON activity_events(kind, event_time_unix);
CREATE INDEX IF NOT EXISTS idx_activity_session    ON activity_events(session_id);
CREATE INDEX IF NOT EXISTS idx_activity_usage_key  ON activity_events(usage_dedup_key);

-- Immutability: same terms as usage_events.
CREATE TRIGGER IF NOT EXISTS trg_activity_no_update
BEFORE UPDATE ON activity_events
BEGIN SELECT RAISE(ABORT, 'activity_events is append-only: UPDATE forbidden'); END;

CREATE TRIGGER IF NOT EXISTS trg_activity_no_delete
BEFORE DELETE ON activity_events
BEGIN SELECT RAISE(ABORT, 'activity_events is append-only: DELETE forbidden'); END;

-- Mutable accumulator state: latest observed counters per growing cell.
CREATE TABLE IF NOT EXISTS aggregate_state (
  tool                  TEXT    NOT NULL,
  acc_key               TEXT    NOT NULL,                 -- AggregateSnapshot.Key
  model                 TEXT    NOT NULL DEFAULT '',
  session_id            TEXT    NOT NULL DEFAULT '',
  project               TEXT    NOT NULL DEFAULT '',
  observed_time_unix    INTEGER NOT NULL,
  input_tokens          INTEGER NOT NULL DEFAULT 0,
  output_tokens         INTEGER NOT NULL DEFAULT 0,
  cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens          INTEGER NOT NULL DEFAULT 0,
  source_path           TEXT    NOT NULL DEFAULT '',
  raw                   TEXT,
  PRIMARY KEY (tool, acc_key)
);
