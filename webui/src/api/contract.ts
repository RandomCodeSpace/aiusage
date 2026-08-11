/**
 * The daemon/UI data contract.
 *
 * These types are one half of a contract whose other half is the Go wire
 * package, internal/web/wire.go - they were written first, as the
 * specification the handlers were built against, and the two are kept field
 * for field in step. The mock adapter (api/mock.ts) implements the same
 * shapes, so a mock build and a live build differ only in where the bytes
 * come from. Field names are the JSON wire names, snake_case, mirroring the
 * store-layer shapes in internal/store/store.go so the server serialises
 * rather than re-models.
 *
 * Two invariants the wire carries:
 *
 *  - Summary and facets are aggregated SERVER-side. The wire carries buckets,
 *    never events, so a 360k-row ledger never crosses it.
 *  - `raw` never appears. The hostnames are public and unauthenticated, and
 *    45,615 rows predate the usage-object-only allow-list, so an endpoint
 *    returning raw would publish transcript content. There is no field for it
 *    here and there must never be one.
 */

/**
 * Bumped by the server whenever these shapes change incompatibly. The page
 * captures it at boot and reloads itself the moment a later /api/meta
 * disagrees - a long-lived tab must never decode a newer wire with older
 * code.
 */
export const BOOT_CONTRACT_VERSION_UNKNOWN = -1;

/** Hard server-side cap on GET /api/events. Rows beyond it are truncated. */
export const EVENTS_PAGE_LIMIT = 1000;

/** Grouping dimensions the store accepts (store.Filter.GroupBy). */
export type GroupDimension =
  | 'hour'
  | 'day'
  | 'week'
  | 'month'
  | 'tool'
  | 'model'
  | 'provider'
  | 'project'
  | 'session';

/** Time granularities the UI offers. Sub-hour is out of the rollup's reach. */
export type Granularity = Extract<GroupDimension, 'hour' | 'day' | 'week' | 'month'>;

/** model.EventKind. */
export type EventKind = 'usage' | 'adjustment';

/**
 * Which store answered an aggregate.
 *
 * The server routes each request to the derived rollup (15-minute UTC buckets)
 * when the rollup keeps every dimension the query names, and to the ledger
 * otherwise. The two are not interchangeable: a rollup answer snaps its bounds
 * outward to whole 15-minute buckets and reports 0 distinct sessions, because
 * the rollup has no session dimension to count. The page has to be able to say
 * which one it got.
 */
export type DataSource = 'rollup' | 'ledger';

// ---------------------------------------------------------------- GET /api/meta

export interface DaemonStatus {
  /**
   * The advisory lock's answer, which is not the same question as "is pid
   * non-zero": a daemon whose pidfile cannot be read still holds the lock, so
   * pid 0 alone must not be rendered as stopped.
   */
  running: boolean;
  pid: number;
  uptime_seconds: number;
  /** Unix seconds of the last completed collection cycle, 0 when none yet. */
  last_cycle_unix: number;
  /** Configured seconds between collection cycles. */
  interval_seconds: number;
}

export interface DatabaseStatus {
  size_bytes: number;
  events: number;
  schema_version: number;
  earliest_event_unix: number;
  latest_event_unix: number;
}

/** internal/sysmon gauges, each a 0..1 fraction. */
export interface ResourceGauges {
  cpu: number;
  memory: number;
  disk: number;
}

/**
 * What this server build can do. It exists so the page can say why a feature
 * is missing instead of appearing broken: an untagged binary serves the API
 * but carries no embedded assets.
 */
export interface Capabilities {
  embedded_ui: boolean;
}

export interface MetaResponse {
  /** The only field whose meaning is frozen across versions. */
  contract_version: number;
  server_version: string;
  /** Server clock, so the scene's now-line does not trust a skewed browser. */
  now_unix: number;
  /** Highest observed_time_unix the store has ingested. */
  watermark: number;
  daemon: DaemonStatus;
  database: DatabaseStatus;
  resources: ResourceGauges;
  /** Tool ids present in the ledger, in ledger order. Drives the lanes. */
  tools: string[];
  capabilities: Capabilities;
}

// ------------------------------------------------------- GET /api/summary

/** store.Filter, minus GroupBy which each endpoint fixes for itself. */
export interface RangeQuery {
  /** Inclusive lower bound, unix seconds. */
  since: number;
  /** Exclusive upper bound, unix seconds. */
  until: number;
  tools?: string[];
  models?: string[];
  projects?: string[];
  sessions?: string[];
}

export interface SummaryQuery extends RangeQuery {
  group_by: GroupDimension[];
}

/** store.Bucket. Token counts are int64; cost is millionths of a USD. */
export interface Bucket {
  /** Dimension name to value, e.g. {"day":"2026-05-29","tool":"codex"}. */
  keys: Record<string, string>;
  /** Dimension names in group_by order - the store's OrderedKeys. */
  ordered_keys: string[];
  events: number;
  /** COUNT(DISTINCT session_id). Distinct counts do not add across buckets. */
  sessions: number;
  input: number;
  output: number;
  cache_creation: number;
  cache_read: number;
  reasoning: number;
  total: number;
  /**
   * Sum of the costs stamped at collect time. Rows with no stamped cost
   * contribute nothing, so a bucket with unpriced_events > 0 is an
   * UNDERSTATEMENT until those are display-priced.
   */
  cost_micro_usd: number;
  unpriced_events: number;
}

export interface SummaryResponse {
  group_by: GroupDimension[];
  buckets: Bucket[];
  totals: Bucket;
  /**
   * Echoed back so a late response can be matched to its request. When source
   * is 'rollup' these are the requested bounds snapped OUTWARD to whole UTC
   * hours, so the caller labels what it got rather than what it asked for.
   */
  since: number;
  until: number;
  /** Which store answered. 'rollup' implies snapped bounds and sessions = 0. */
  source: DataSource;
}

// -------------------------------------------------------- GET /api/facets

export interface Facet {
  value: string;
  events: number;
  total: number;
}

export interface FacetsResponse {
  tools: Facet[];
  models: Facet[];
  providers: Facet[];
  projects: Facet[];
  since: number;
  until: number;
  /** Which store answered the rollup-serviceable lists. */
  source: DataSource;
}

// -------------------------------------------------------- GET /api/events

export interface EventsQuery extends RangeQuery {
  /** Opaque keyset cursor from the previous page. */
  cursor?: string | null;
  /** Server clamps to EVENTS_PAGE_LIMIT. */
  limit?: number;
}

/**
 * One ledger row, explicitly projected. model.UsageEvent minus Raw and minus
 * the transient CacheTTL - the projection is the condition under which an
 * unauthenticated surface is survivable, not a later optimisation.
 */
export interface EventRow {
  /**
   * Keyset cursor field: monotone within (event_time_unix, seq). The store
   * has no id column today, so the server must expose rowid (or an added
   * column) for this - tracked as a precondition, not an assumption the UI
   * can paper over.
   */
  seq: number;
  tool: string;
  model: string;
  provider: string;
  service_tier: string;
  session_id: string;
  project: string;
  event_time_unix: number;
  observed_time_unix: number;
  input: number;
  output: number;
  cache_creation: number;
  cache_read: number;
  reasoning: number;
  total: number;
  /** null means unpriced. A stamped 0 would claim the request was free. */
  cost_micro_usd: number | null;
  price_source: string;
  kind: EventKind;
}

export interface EventsResponse {
  rows: EventRow[];
  /** null when this page is the last one. */
  next_cursor: string | null;
  /** True when the range holds more rows than the cap can return. */
  truncated: boolean;
  limit: number;
  /**
   * The true number of rows the range holds, counted server-side and
   * independent of the cap. It is what makes the cap honest: a capped page
   * says how much it is not showing instead of being a silent slice.
   */
  total: number;
}

// ------------------------------------------------------------- WS /api/ws

/**
 * The live channel carries a NOTIFICATION, never data: the server cannot
 * answer "what changed since X", and a cold recompute costs seconds, so
 * pushing aggregates would pay that per client per cycle. A frame becomes a
 * query invalidation and the client refetches only the view it is on.
 */
export interface LiveFrame {
  watermark: number;
  cycle_at: number;
}

// ------------------------------------------------------------------ helpers

/** Cost is stored in millionths of a USD; nothing but display divides it. */
export function microUsdToUsd(micro: number): number {
  return micro / 1e6;
}
