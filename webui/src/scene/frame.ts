import type { TraceSession } from './sessions';
import type { VisibleTotals } from './totals';
import { EMPTY_TOTALS } from './totals';
import type { View } from './viewport';

/** What the DOM tooltip needs: a screen anchor and what is under it. */
export interface HoverState {
  clientX: number;
  clientY: number;
  session: TraceSession | null;
  instantMs: number;
}

/** One painted frame, published to the DOM chrome that reports on the scene. */
export interface SceneFrame {
  view: View;
  totals: VisibleTotals;
  nowMs: number;
  /** Canvas-local x of the now line, or null when it is off screen. */
  nowX: number | null;
  pxPerHour: number;
  hover: HoverState | null;
  follow: boolean;
}

export const EMPTY_FRAME: SceneFrame = {
  view: { t0: 0, t1: 0 },
  totals: EMPTY_TOTALS,
  nowMs: 0,
  nowX: null,
  pxPerHour: 0,
  hover: null,
  follow: true,
};

/**
 * The scene repaints on every pan frame; React must not. Components that
 * report on the scene subscribe here instead, so a drag re-renders four small
 * nodes rather than the tree.
 */
export class FrameStore {
  private frame: SceneFrame = EMPTY_FRAME;
  private readonly listeners = new Set<() => void>();

  subscribe = (listener: () => void): (() => void) => {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  };

  getSnapshot = (): SceneFrame => this.frame;

  publish = (frame: SceneFrame): void => {
    this.frame = frame;
    for (const listener of this.listeners) listener();
  };
}
