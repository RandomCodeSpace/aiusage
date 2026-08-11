import type { EventRow } from '../api/contract';
import { microUsdToUsd } from '../api/contract';

/**
 * A session as the scene draws it: a block on a lane, spanning the time it
 * actually occupied.
 *
 * The wire has no session table - the ledger stores events, and the store's
 * only session notion is COUNT(DISTINCT session_id). So a session is folded
 * client-side from the rows of one /api/events page. Times are milliseconds
 * because every canvas and Date computation on this page is.
 */
export interface TraceSession {
  id: string;
  tool: string;
  model: string;
  provider: string;
  project: string;
  startMs: number;
  endMs: number;
  durationMs: number;
  tokens: number;
  events: number;
  costUSD: number;
  /** True when any row in the session carried no stamped cost. */
  unpriced: boolean;
  /** True when the last row is inside the live window ending at now. */
  live: boolean;
}

/** A session whose last event is this recent is still considered running. */
export const LIVE_WINDOW_MS = 10 * 60 * 1000;

/**
 * A single event has no duration; a block that thin is invisible and cannot
 * be tapped, so a lone row gets a nominal span.
 */
const MIN_SPAN_MS = 60 * 1000;

export function foldSessions(rows: readonly EventRow[], nowMs: number): TraceSession[] {
  const byId = new Map<string, TraceSession>();

  for (const row of rows) {
    const at = row.event_time_unix * 1000;
    // Rows with no session id cannot be folded into a block; they still count
    // in every bucket the server aggregates, which is where they belong.
    if (!row.session_id) continue;

    let session = byId.get(row.session_id);
    if (!session) {
      session = {
        id: row.session_id,
        tool: row.tool,
        model: row.model,
        provider: row.provider,
        project: row.project,
        startMs: at,
        endMs: at,
        durationMs: 0,
        tokens: 0,
        events: 0,
        costUSD: 0,
        unpriced: false,
        live: false,
      };
      byId.set(row.session_id, session);
    }

    session.startMs = Math.min(session.startMs, at);
    session.endMs = Math.max(session.endMs, at);
    session.tokens += row.total;
    session.events += 1;
    if (row.cost_micro_usd === null) session.unpriced = true;
    else session.costUSD += microUsdToUsd(row.cost_micro_usd);
    // Last row wins for the descriptive attributes: a session that switched
    // models mid-flight is labelled by what it ended up using.
    session.model = row.model;
    session.project = row.project;
  }

  const sessions = [...byId.values()];
  for (const session of sessions) {
    session.durationMs = Math.max(MIN_SPAN_MS, session.endMs - session.startMs);
    session.endMs = session.startMs + session.durationMs;
    session.live = session.endMs >= nowMs - LIVE_WINDOW_MS;
  }
  sessions.sort((a, b) => a.startMs - b.startMs);
  return sessions;
}
