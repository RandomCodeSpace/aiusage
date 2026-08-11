import type { ApiClient } from './client';
import type {
  Bucket,
  DataSource,
  EventRow,
  EventsQuery,
  EventsResponse,
  Facet,
  FacetsResponse,
  GroupDimension,
  LiveFrame,
  MetaResponse,
  RangeQuery,
  SummaryQuery,
  SummaryResponse,
} from './contract';
import { EVENTS_PAGE_LIMIT } from './contract';
import { bucketKey } from './buckets';

/**
 * A deterministic stand-in for the daemon, shaped by the same contract.
 *
 * It exists so the surface can be built, reviewed and demonstrated with no
 * daemon and no ledger, and so a reviewer sees the SAME scene on every load -
 * the prototype's mulberry32, same seed, same sessions. It aggregates
 * server-side, exactly as the real endpoint must: summary() folds rows into
 * buckets before returning, and never returns rows.
 */

const HOUR_S = 3600;
const DAY_S = 24 * HOUR_S;
const CONTRACT_VERSION = 1;

/**
 * Joins the group-by values into a map key. NUL cannot occur inside a
 * dimension value, so no pair of distinct key tuples can collide on it. It
 * is spelled as an escape because a literal NUL byte in the source makes the
 * file binary to git, grep and every review tool.
 */
const KEY_SEPARATOR = '\u0000';

/**
 * The mock walks its generated rows for every question, which is what the
 * server calls the ledger path: exact bounds, real distinct session counts.
 * It has no rollup and never claims one.
 */
const MOCK_SOURCE: DataSource = 'ledger';

/** The prototype's PRNG, unchanged, so the generated world is identical. */
function mulberry32(seed: number): () => number {
  let a = seed;
  return () => {
    a |= 0;
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

interface ToolProfile {
  models: string[];
  provider: string;
  /** Tokens per hour of session wall time. */
  rate: number;
  perDay: [number, number];
  /** Session duration in minutes. */
  durationMinutes: [number, number];
  /** USD per million tokens, used to stamp a plausible cost. */
  usdPerMillion: number;
}

const TOOLS: Record<string, ToolProfile> = {
  'claude-code': {
    models: ['claude-opus-4-7', 'claude-sonnet-4-6', 'claude-haiku-4-5'],
    provider: 'anthropic',
    rate: 480_000,
    perDay: [3, 6],
    durationMinutes: [25, 200],
    usdPerMillion: 4.2,
  },
  codex: {
    models: ['gpt-5-codex', 'gpt-5-codex-mini', 'gpt-5-nano'],
    provider: 'openai',
    rate: 130_000,
    perDay: [6, 12],
    durationMinutes: [8, 90],
    usdPerMillion: 2.8,
  },
  opencode: {
    models: ['gemini-3-pro', 'qwen3-coder-480b', 'deepseek-v4'],
    provider: 'google',
    rate: 90_000,
    perDay: [1, 4],
    durationMinutes: [12, 70],
    usdPerMillion: 1.9,
  },
};

const PROJECTS = ['aiusage', 'oss-code', 'unified-agent-manager', 'buzz', 'graphify', 'brain-map'];

interface MockSession {
  id: string;
  tool: string;
  model: string;
  provider: string;
  project: string;
  startUnix: number;
  durationSeconds: number;
  tokens: number;
  eventCount: number;
  costMicroUSD: number;
  live: boolean;
}

interface MockWorld {
  nowUnix: number;
  bootUnix: number;
  watermark: number;
  lastCycleUnix: number;
  sessions: MockSession[];
  rows: EventRow[];
  liveSession: MockSession;
  nextSeq: number;
}

const rnd = mulberry32(0xa105a9e | 0);

function pick<T>(values: readonly T[]): T {
  return values[Math.floor(rnd() * values.length)] as T;
}

function splitTokens(total: number, share: number): number {
  return Math.max(0, Math.round(total * share));
}

/**
 * Explodes a session into ledger rows. Real sources emit one row per API
 * request; the mock emits a handful spread across the session so the folded
 * span matches the session and the 1000-row cap is reachable, which is what
 * makes the truncated flag testable rather than theoretical.
 */
function rowsForSession(session: MockSession, startSeq: number): EventRow[] {
  const count = Math.max(2, Math.min(8, Math.round(2 + rnd() * 6)));
  const rows: EventRow[] = [];
  const profile = TOOLS[session.tool];
  const share = 1 / count;
  for (let i = 0; i < count; i++) {
    const at =
      session.startUnix +
      Math.round((session.durationSeconds * i) / Math.max(1, count - 1 === 0 ? 1 : count - 1));
    const tokens = Math.round(session.tokens * share);
    const input = splitTokens(tokens, 0.18);
    const output = splitTokens(tokens, 0.12);
    const cacheCreation = splitTokens(tokens, 0.14);
    const cacheRead = Math.max(0, tokens - input - output - cacheCreation);
    const reasoning = splitTokens(output, 0.35);
    // One row in twenty is left unpriced, mirroring the pre-v3 rows the real
    // ledger carries: null is the only "unknown", a stamped 0 would lie.
    const unpriced = rnd() < 0.05;
    rows.push({
      seq: startSeq + i,
      tool: session.tool,
      model: session.model,
      provider: session.provider,
      service_tier: 'standard',
      session_id: session.id,
      project: session.project,
      event_time_unix: at,
      observed_time_unix: at + 30,
      input,
      output,
      cache_creation: cacheCreation,
      cache_read: cacheRead,
      reasoning,
      total: tokens,
      cost_micro_usd: unpriced
        ? null
        : Math.round((tokens / 1e6) * (profile?.usdPerMillion ?? 2) * 1e6),
      price_source: unpriced ? '' : 'litellm-2026-08-01',
      kind: 'usage',
    });
  }
  return rows;
}

function buildWorld(): MockWorld {
  const nowUnix = Math.floor(Date.now() / 1000);
  const sessions: MockSession[] = [];

  for (const [tool, profile] of Object.entries(TOOLS)) {
    for (let day = 13; day >= 0; day--) {
      const dayStart = nowUnix - day * DAY_S;
      const weekday = new Date(dayStart * 1000).getDay();
      const weight = weekday === 0 || weekday === 6 ? 0.35 : 1;
      const perDay = profile.perDay[0] + rnd() * (profile.perDay[1] - profile.perDay[0]);
      const count = Math.round(perDay * weight);
      for (let i = 0; i < count; i++) {
        const late = rnd() < 0.22;
        const hour = late ? 22 + rnd() * 4 : 9 + rnd() * 12;
        const midnight = dayStart - new Date(dayStart * 1000).getHours() * HOUR_S;
        const startUnix = Math.round(midnight + hour * HOUR_S + rnd() * 40 * 60);
        const minutes =
          profile.durationMinutes[0] +
          rnd() * (profile.durationMinutes[1] - profile.durationMinutes[0]);
        const durationSeconds = Math.round(minutes * 60);
        if (startUnix + durationSeconds > nowUnix) continue;
        const tokens = Math.round(profile.rate * (minutes / 60) * (0.4 + rnd() * 1.4));
        sessions.push({
          id: `ses_${Math.floor(rnd() * 0xffffffff).toString(16).padStart(8, '0')}`,
          tool,
          model: pick(profile.models),
          provider: profile.provider,
          project: pick(PROJECTS),
          startUnix,
          durationSeconds,
          tokens,
          eventCount: Math.max(4, Math.round(tokens / 26_000)),
          costMicroUSD: Math.round((tokens / 1e6) * profile.usdPerMillion * 1e6),
          live: false,
        });
      }
    }
  }

  const liveSession: MockSession = {
    id: 'ses_live0826d5ed',
    tool: 'claude-code',
    model: 'claude-opus-4-7',
    provider: 'anthropic',
    project: 'aiusage',
    startUnix: nowUnix - 47 * 60,
    durationSeconds: 47 * 60,
    tokens: 3_240_000,
    eventCount: 1841,
    costMicroUSD: 12_420_000,
    live: true,
  };
  sessions.push(liveSession);
  sessions.sort((a, b) => a.startUnix - b.startUnix);

  let nextSeq = 1;
  const rows: EventRow[] = [];
  for (const session of sessions) {
    const produced = rowsForSession(session, nextSeq);
    nextSeq += produced.length;
    rows.push(...produced);
  }
  rows.sort((a, b) => a.event_time_unix - b.event_time_unix || a.seq - b.seq);

  return {
    nowUnix,
    bootUnix: nowUnix - 5 * 60,
    watermark: nowUnix,
    lastCycleUnix: nowUnix,
    sessions,
    rows,
    liveSession,
    nextSeq,
  };
}

/**
 * One collection cycle lands: the running session grows, now advances, and a
 * frame goes out. Sample cadence is 8s so the live edge is visible in a demo;
 * the real daemon is minutes, so production is far quieter.
 */
function advance(world: MockWorld, deltaSeconds: number): void {
  const events = 3 + Math.floor(rnd() * 14);
  const tokens = Math.round(events * 21_000 * (0.6 + rnd()));
  world.nowUnix += deltaSeconds;
  world.watermark = world.nowUnix;
  world.lastCycleUnix = world.nowUnix;
  world.liveSession.durationSeconds += deltaSeconds;
  world.liveSession.tokens += tokens;
  world.liveSession.eventCount += events;
  world.liveSession.costMicroUSD += Math.round((tokens / 1e6) * 4.2 * 1e6);

  const input = splitTokens(tokens, 0.18);
  const output = splitTokens(tokens, 0.12);
  const cacheCreation = splitTokens(tokens, 0.14);
  world.rows.push({
    seq: world.nextSeq++,
    tool: world.liveSession.tool,
    model: world.liveSession.model,
    provider: world.liveSession.provider,
    service_tier: 'standard',
    session_id: world.liveSession.id,
    project: world.liveSession.project,
    event_time_unix: world.nowUnix,
    observed_time_unix: world.nowUnix,
    input,
    output,
    cache_creation: cacheCreation,
    cache_read: Math.max(0, tokens - input - output - cacheCreation),
    reasoning: splitTokens(output, 0.35),
    total: tokens,
    cost_micro_usd: Math.round((tokens / 1e6) * 4.2 * 1e6),
    price_source: 'litellm-2026-08-01',
    kind: 'usage',
  });
}

function emptyBucket(keys: Record<string, string>, ordered: string[]): Bucket {
  return {
    keys,
    ordered_keys: ordered,
    events: 0,
    sessions: 0,
    input: 0,
    output: 0,
    cache_creation: 0,
    cache_read: 0,
    reasoning: 0,
    total: 0,
    cost_micro_usd: 0,
    unpriced_events: 0,
  };
}

function accumulate(bucket: Bucket, row: EventRow, sessionIds: Set<string>): void {
  bucket.events += 1;
  bucket.input += row.input;
  bucket.output += row.output;
  bucket.cache_creation += row.cache_creation;
  bucket.cache_read += row.cache_read;
  bucket.reasoning += row.reasoning;
  bucket.total += row.total;
  if (row.cost_micro_usd === null) bucket.unpriced_events += 1;
  else bucket.cost_micro_usd += row.cost_micro_usd;
  if (row.session_id) sessionIds.add(row.session_id);
}

function dimensionValue(row: EventRow, dimension: GroupDimension): string {
  switch (dimension) {
    case 'tool':
      return row.tool;
    case 'model':
      return row.model;
    case 'provider':
      return row.provider;
    case 'project':
      return row.project;
    case 'session':
      return row.session_id;
    default:
      return bucketKey(row.event_time_unix, dimension);
  }
}

function matchesRange(row: EventRow, query: RangeQuery): boolean {
  if (row.event_time_unix < query.since || row.event_time_unix >= query.until) return false;
  if (query.tools?.length && !query.tools.includes(row.tool)) return false;
  if (query.models?.length && !query.models.includes(row.model)) return false;
  if (query.projects?.length && !query.projects.includes(row.project)) return false;
  if (query.sessions?.length && !query.sessions.includes(row.session_id)) return false;
  return true;
}

function summarize(world: MockWorld, query: SummaryQuery): SummaryResponse {
  const buckets = new Map<string, { bucket: Bucket; sessions: Set<string> }>();
  const totals = emptyBucket({}, []);
  const totalSessions = new Set<string>();

  for (const row of world.rows) {
    if (!matchesRange(row, query)) continue;
    accumulate(totals, row, totalSessions);

    const keys: Record<string, string> = {};
    for (const dimension of query.group_by) keys[dimension] = dimensionValue(row, dimension);
    const id = query.group_by.map((dimension) => keys[dimension]).join(KEY_SEPARATOR);
    let entry = buckets.get(id);
    if (!entry) {
      entry = { bucket: emptyBucket(keys, [...query.group_by]), sessions: new Set() };
      buckets.set(id, entry);
    }
    accumulate(entry.bucket, row, entry.sessions);
  }

  totals.sessions = totalSessions.size;
  const out: Bucket[] = [];
  for (const entry of buckets.values()) {
    entry.bucket.sessions = entry.sessions.size;
    out.push(entry.bucket);
  }
  // The store returns buckets in key order; lexical order on the formatted
  // keys is the same order for every time dimension by construction.
  out.sort((a, b) =>
    a.ordered_keys
      .map((k) => a.keys[k])
      .join(KEY_SEPARATOR)
      .localeCompare(b.ordered_keys.map((k) => b.keys[k]).join(KEY_SEPARATOR)),
  );

  return {
    group_by: [...query.group_by],
    buckets: out,
    totals,
    since: query.since,
    until: query.until,
    source: MOCK_SOURCE,
  };
}

function facetsOf(world: MockWorld, query: RangeQuery, field: keyof EventRow): Facet[] {
  const counts = new Map<string, Facet>();
  for (const row of world.rows) {
    if (!matchesRange(row, query)) continue;
    const value = String(row[field]);
    const facet = counts.get(value) ?? { value, events: 0, total: 0 };
    facet.events += 1;
    facet.total += row.total;
    counts.set(value, facet);
  }
  return [...counts.values()].sort((a, b) => b.total - a.total);
}

/** Deterministic latency so loading states are exercised, not skipped. */
function settle<T>(value: T): Promise<T> {
  return new Promise((resolve) => {
    setTimeout(() => {
      resolve(value);
    }, 40);
  });
}

export function createMockClient(): ApiClient {
  const world = buildWorld();
  const listeners = new Set<(frame: LiveFrame) => void>();
  let timer: number | undefined;

  const startTicking = (): void => {
    if (timer !== undefined) return;
    timer = window.setInterval(() => {
      advance(world, 8);
      const frame: LiveFrame = { watermark: world.watermark, cycle_at: world.lastCycleUnix };
      for (const listener of listeners) listener(frame);
    }, 8000);
  };

  const stopTicking = (): void => {
    if (timer === undefined) return;
    window.clearInterval(timer);
    timer = undefined;
  };

  return {
    meta: () => {
      const meta: MetaResponse = {
        contract_version: CONTRACT_VERSION,
        server_version: '0.0.0-mock',
        now_unix: world.nowUnix,
        watermark: world.watermark,
        daemon: {
          running: true,
          pid: 1202140,
          uptime_seconds: world.nowUnix - world.bootUnix,
          last_cycle_unix: world.lastCycleUnix,
          interval_seconds: 300,
        },
        database: {
          size_bytes: 243_269_632,
          events: world.rows.length,
          schema_version: 5,
          earliest_event_unix: world.rows[0]?.event_time_unix ?? world.nowUnix,
          latest_event_unix: world.rows[world.rows.length - 1]?.event_time_unix ?? world.nowUnix,
        },
        resources: { cpu: 0.14, memory: 0.38, disk: 0.81 },
        tools: Object.keys(TOOLS),
        // The mock IS the embedded UI as far as the page can tell: it is only
        // ever reached from a build that is serving this bundle.
        capabilities: { embedded_ui: true },
      };
      return settle(meta);
    },

    summary: (query) => settle(summarize(world, query)),

    facets: (query) =>
      settle<FacetsResponse>({
        tools: facetsOf(world, query, 'tool'),
        models: facetsOf(world, query, 'model'),
        providers: facetsOf(world, query, 'provider'),
        projects: facetsOf(world, query, 'project'),
        since: query.since,
        until: query.until,
        source: MOCK_SOURCE,
      }),

    events: (query: EventsQuery) => {
      const limit = Math.min(query.limit ?? EVENTS_PAGE_LIMIT, EVENTS_PAGE_LIMIT);
      const cursorSeq = query.cursor ? Number(query.cursor.split(':')[1] ?? 0) : 0;
      const inRange = world.rows.filter((row) => matchesRange(row, query));
      const matching = inRange.filter((row) => row.seq > cursorSeq);
      const page = matching.slice(0, limit);
      const last = page[page.length - 1];
      const truncated = matching.length > page.length;
      return settle<EventsResponse>({
        rows: page,
        next_cursor: truncated && last ? `${String(last.event_time_unix)}:${String(last.seq)}` : null,
        truncated,
        limit,
        // The whole range, cursor ignored: total answers "how big is this
        // range", not "how much of it is left".
        total: inRange.length,
      });
    },

    subscribe: (onFrame, onState) => {
      listeners.add(onFrame);
      startTicking();
      onState(true);
      return () => {
        listeners.delete(onFrame);
        onState(false);
        if (listeners.size === 0) stopTicking();
      };
    },
  };
}
