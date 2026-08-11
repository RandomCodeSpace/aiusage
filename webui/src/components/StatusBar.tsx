import { formatBytes, formatCount, formatUptime } from '../format';
import type { MetaResponse } from '../api/contract';
import type { LiveState } from '../api/queries';

const HOT_GAUGE = 0.75;

export interface StatusBarProps {
  meta: MetaResponse | undefined;
  live: LiveState;
  /** True when the event page hit the server cap and blocks are partial. */
  truncated: boolean;
}

function Gauge({ label, value }: { label: string; value: number }) {
  const pct = Math.max(0, Math.min(1, value));
  return (
    <>
      <span className="k">{label}</span>
      <span className={`minigauge${pct >= HOT_GAUGE ? ' hot' : ''}`}>
        <i style={{ width: `${String(Math.round(pct * 100))}%` }} />
      </span>
    </>
  );
}

function cycleLabel(meta: MetaResponse | undefined, live: LiveState): string {
  const at = live.frame?.cycle_at ?? meta?.daemon.last_cycle_unix ?? 0;
  if (at === 0) return 'none yet';
  return new Date(at * 1000).toLocaleTimeString();
}

export function StatusBar({ meta, live, truncated }: StatusBarProps) {
  return (
    <footer className="status" aria-label="Daemon status">
      <span className="cell">
        <span className="k">daemon</span>
        <span className={`v ${meta ? 'good' : 'warn'}`}>
          {meta ? `pid ${String(meta.daemon.pid)}` : 'unreachable'}
        </span>
        {meta ? <span className="k hide-sm">{formatUptime(meta.daemon.uptime_seconds)}</span> : null}
      </span>
      <span className="cell hide-sm">
        <span className="k">db</span>
        <span className="v">{meta ? formatBytes(meta.database.size_bytes) : '-'}</span>
        <span className="k">{meta ? formatCount(meta.database.events) : ''}</span>
      </span>
      <span className="cell hide-sm">
        <Gauge label="cpu" value={meta?.resources.cpu ?? 0} />
        <Gauge label="mem" value={meta?.resources.memory ?? 0} />
        <Gauge label="disk" value={meta?.resources.disk ?? 0} />
      </span>
      <span className="grow" />
      {truncated ? (
        <span className="cell">
          {/* Blocks are drawn from a capped page; the aggregate above them is
              server-side and complete either way. */}
          <span className="k">events</span>
          <span className="v warn">capped</span>
        </span>
      ) : null}
      <span className="cell">
        <span className="k">last cycle</span>
        <span className="v now">{cycleLabel(meta, live)}</span>
      </span>
      <span className="cell">
        <span className="k">ws</span>
        <span className={`v ${live.connected ? 'good' : 'warn'}`}>
          {live.connected ? 'connected' : 'reconnecting'}
        </span>
      </span>
    </footer>
  );
}
