import { formatCompact, formatUSD } from '../format';
import { useSceneFrame } from '../hooks/useSceneFrame';
import type { FrameStore } from '../scene/frame';
import { toolColorVar } from '../theme';

/** Server-side totals for the fetched range, uncapped. */
export interface RangeTotal {
  tokens: number;
  /** True row count for the range, so the cap says what it is not showing. */
  events: number;
  costUSD: number;
}

export interface ReadoutProps {
  frames: FrameStore;
  tools: readonly string[];
  cycles: number;
  /**
   * Present only when the capped event page means the in-view figures are
   * known to understate. Aggregated server-side, so it covers the whole
   * fetched range rather than the pixel-exact view.
   */
  rangeTotal: RangeTotal | null;
}

/**
 * The readout sums WHAT YOU SEE: pan and the numbers follow, with each
 * session clipped to its visible overlap. It is the only place on the page
 * that answers "how much is this window", and it never disagrees with the
 * scene because it reads the same frame.
 */
export function Readout({ frames, tools, cycles, rangeTotal }: ReadoutProps) {
  const frame = useSceneFrame(frames);
  const { totals } = frame;

  return (
    <aside className="readout" aria-label="Totals for the visible range">
      <h2>In view</h2>
      {/* Keyed on the cycle count: a remount restarts the one-shot flash, so
          a landed cycle is visible without a timer in React state. */}
      <div key={cycles} className={cycles > 0 ? 'big tick' : 'big'}>
        {formatCompact(totals.tokens)}
      </div>
      <div className="unit">tokens</div>
      <div className="row">
        <span>sessions</span>
        <span>{totals.sessions}</span>
      </div>
      <div className="row">
        <span>events</span>
        <span>{formatCompact(totals.events)}</span>
      </div>
      <div className="row">
        <span>spend</span>
        {/* A window holding unpriced rows is an understatement, not a total. */}
        <span title={totals.costIncomplete ? 'unpriced rows in range' : undefined}>
          {totals.costIncomplete ? '>' : ''}
          {formatUSD(totals.costUSD)}
        </span>
      </div>
      {rangeTotal ? (
        <>
          <hr />
          <div className="row">
            <span>range (server)</span>
            <span>{formatCompact(rangeTotal.tokens)}</span>
          </div>
          <div className="row">
            <span>range events</span>
            <span>{formatCompact(rangeTotal.events)}</span>
          </div>
        </>
      ) : null}
      <hr />
      {tools.map((tool) => (
        <div className="row" key={tool}>
          <span>
            <i style={{ background: `var(${toolColorVar(tool)})` }} />
            {tool}
          </span>
          <span>{formatCompact(totals.byTool[tool] ?? 0)}</span>
        </div>
      ))}
    </aside>
  );
}
