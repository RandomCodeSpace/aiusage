import { useSceneFrame } from '../hooks/useSceneFrame';
import type { FrameStore } from '../scene/frame';

export interface NowLineProps {
  frames: FrameStore;
}

/**
 * The right edge is alive. The line breathes in CSS (and stands still under
 * prefers-reduced-motion); its position comes from the same frame the scene
 * just painted, so it can never sit a pixel off the canvas it annotates.
 */
export function NowLine({ frames }: NowLineProps) {
  const { nowX } = useSceneFrame(frames);
  if (nowX === null) return null;
  return <div className="nowline" style={{ left: nowX }} aria-hidden="true" />;
}
