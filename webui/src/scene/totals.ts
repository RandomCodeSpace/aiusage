import type { TraceSession } from './sessions';
import type { View } from './viewport';

/** What the readout card sums: exactly what is on screen, nothing else. */
export interface VisibleTotals {
  tokens: number;
  events: number;
  costUSD: number;
  sessions: number;
  /** Tokens per tool id, same clipping. */
  byTool: Record<string, number>;
  /** True when any contributing session carried an unpriced row. */
  costIncomplete: boolean;
}

export const EMPTY_TOTALS: VisibleTotals = {
  tokens: 0,
  events: 0,
  costUSD: 0,
  sessions: 0,
  byTool: {},
  costIncomplete: false,
};

/**
 * Sums the visible window, clipping each session's contribution to the part
 * of it that is actually on screen - so panning moves the numbers honestly
 * instead of flipping a session in and out whole.
 */
export function visibleTotals(sessions: readonly TraceSession[], view: View): VisibleTotals {
  const totals: VisibleTotals = {
    tokens: 0,
    events: 0,
    costUSD: 0,
    sessions: 0,
    byTool: {},
    costIncomplete: false,
  };

  for (const session of sessions) {
    if (session.endMs < view.t0 || session.startMs > view.t1) continue;
    const overlap =
      (Math.min(view.t1, session.endMs) - Math.max(view.t0, session.startMs)) / session.durationMs;
    totals.tokens += session.tokens * overlap;
    totals.events += session.events * overlap;
    totals.costUSD += session.costUSD * overlap;
    totals.byTool[session.tool] = (totals.byTool[session.tool] ?? 0) + session.tokens * overlap;
    totals.sessions += 1;
    if (session.unpriced) totals.costIncomplete = true;
  }

  return totals;
}
