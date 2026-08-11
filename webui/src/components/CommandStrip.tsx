import { formatSpan, formatStamp } from '../format';
import { useSceneFrame } from '../hooks/useSceneFrame';
import type { FrameStore } from '../scene/frame';
import { spanOf } from '../scene/viewport';

export interface CommandStripProps {
  frames: FrameStore;
  follow: boolean;
  /** Increments once per collection cycle; restarts the heartbeat ripple. */
  cycles: number;
  reducedMotion: boolean;
  onToggleFollow: () => void;
}

export function CommandStrip({
  frames,
  follow,
  cycles,
  reducedMotion,
  onToggleFollow,
}: CommandStripProps) {
  const frame = useSceneFrame(frames);
  const span = spanOf(frame.view);

  return (
    <header className="head">
      <div className="wordmark">
        <b>aiusage</b>
        <span>trace</span>
      </div>
      <span className="range">
        {formatStamp(frame.view.t0)} - {formatStamp(frame.view.t1)} <b>{formatSpan(span)}</b>
      </span>
      <span className="spacer" />
      <span className="hint">
        wheel / pinch zoom - drag pan - tap or click a session - double-tap in - L live - 0 fit
      </span>
      <button className="chip" aria-pressed={follow} onClick={onToggleFollow}>
        {/* Keyed on the cycle count so the ripple restarts on every frame. */}
        <i
          key={cycles}
          className={`dot${follow && !reducedMotion ? ' beat' : ''}${cycles > 0 && !reducedMotion ? ' pulse' : ''}`}
          style={{ background: follow ? 'var(--good)' : 'var(--accent)' }}
        />
        follow live
      </button>
    </header>
  );
}
