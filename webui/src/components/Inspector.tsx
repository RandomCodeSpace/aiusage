import { formatCompact, formatCount, formatDurationMs, formatUSD } from '../format';
import type { TraceSession } from '../scene/sessions';
import { toolColorVar } from '../theme';

export interface InspectorProps {
  session: TraceSession | null;
  onClose: () => void;
  onZoom: (session: TraceSession) => void;
}

function rowsFor(session: TraceSession): [string, string][] {
  const minutes = Math.max(1, Math.round(session.durationMs / 60e3));
  const rows: [string, string][] = [
    ['tool', session.tool],
    ['model', session.model],
    ['provider', session.provider || 'unknown'],
    ['project', session.project],
    ['started', new Date(session.startMs).toLocaleString()],
    ['duration', formatDurationMs(session.durationMs)],
    ['tokens', formatCount(session.tokens)],
    ['events', formatCount(session.events)],
    ['tokens / min', formatCompact(session.tokens / minutes)],
    // The stamped cost only; unpriced rows are called out rather than guessed
    // at, because a display price is a different number from a stamped one.
    ['cost', `${session.unpriced ? '>' : ''}${formatUSD(session.costUSD)}`],
  ];
  if (session.tokens > 0) {
    rows.push(['cost / 1M tok', formatUSD(session.costUSD / (session.tokens / 1e6))]);
  }
  if (session.unpriced) rows.push(['pricing', 'incomplete - unpriced rows']);
  if (session.live) rows.push(['state', 'running now']);
  return rows;
}

export function Inspector({ session, onClose, onZoom }: InspectorProps) {
  return (
    <aside className={`inspector${session ? ' on' : ''}`} aria-label="Session detail" aria-hidden={!session}>
      <header>
        <i
          className="sw"
          style={{ background: session ? `var(${toolColorVar(session.tool)})` : 'transparent' }}
        />
        <h2>{session?.id ?? 'session'}</h2>
        <button className="close" onClick={onClose} aria-label="Close">
          x
        </button>
      </header>
      <div className="body">
        {session
          ? rowsFor(session).map(([key, value]) => (
              <div className="row" key={key}>
                <span>{key}</span>
                <span>{value}</span>
              </div>
            ))
          : null}
      </div>
      <div className="act">
        <button
          disabled={!session}
          onClick={() => {
            if (session) onZoom(session);
          }}
        >
          zoom to this session
        </button>
      </div>
    </aside>
  );
}
