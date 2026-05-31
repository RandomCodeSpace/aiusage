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
