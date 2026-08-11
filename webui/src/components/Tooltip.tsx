import { formatCompact, formatDurationMs, formatInstant, formatUSD } from '../format';
import { useSceneFrame } from '../hooks/useSceneFrame';
import type { FrameStore } from '../scene/frame';

const TIP_WIDTH_PX = 200;
const CURSOR_OFFSET_PX = 14;

export interface TooltipProps {
  frames: FrameStore;
}

/**
 * Canvas cannot do text layout worth reading, so the tooltip is DOM. It shows
 * the block under the pointer - or, on an empty tap, just names the instant,
 * which is the only way a touch device can ask "when is this".
 */
export function Tooltip({ frames }: TooltipProps) {
  const { hover } = useSceneFrame(frames);
  const session = hover?.session ?? null;

  const style = hover
    ? {
        left: Math.min(hover.clientX + CURSOR_OFFSET_PX, window.innerWidth - TIP_WIDTH_PX),
        top: hover.clientY + CURSOR_OFFSET_PX,
      }
    : undefined;

  return (
    <div className={`tip${hover ? ' on' : ''}`} style={style} role="presentation">
      {hover && !session ? (
        <div className="r">
          <span>{formatInstant(hover.instantMs)}</span>
        </div>
      ) : null}
      {session ? (
        <>
          <div className="r hd">
            <span>{session.project}</span>
            <span>{session.live ? 'live' : ''}</span>
          </div>
          <div className="r">
            <span>{session.tool}</span>
            <span>{session.model}</span>
          </div>
          <div className="r">
            <span>tokens</span>
            <span>{formatCompact(session.tokens)}</span>
          </div>
          <div className="r">
            <span>duration</span>
            <span>{formatDurationMs(session.durationMs)}</span>
          </div>
          <div className="r">
            <span>cost</span>
            <span>
              {session.unpriced ? '>' : ''}
              {formatUSD(session.costUSD)}
            </span>
          </div>
        </>
      ) : null}
    </div>
  );
}
