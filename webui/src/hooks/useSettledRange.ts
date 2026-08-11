import { useEffect, useState } from 'react';
import type { FrameStore } from '../scene/frame';

/** Unix-second bounds, already padded and rounded, safe to use as a query key. */
export interface SettledRange {
  since: number;
  until: number;
}

/** How long the view must hold still before it is worth a request. */
const QUIET_MS = 250;

/** Fetch beyond the edges so a small pan needs no round trip. */
const PAD_FRACTION = 0.35;

/** Round to whole minutes so a one-pixel pan cannot change the query key. */
const GRAIN_S = 60;

function settle(t0: number, t1: number): SettledRange {
  const pad = (t1 - t0) * PAD_FRACTION;
  const since = Math.floor((t0 - pad) / 1000 / GRAIN_S) * GRAIN_S;
  const until = Math.ceil((t1 + pad) / 1000 / GRAIN_S) * GRAIN_S;
  return { since, until };
}

/**
 * Turns a scene that moves at pointer speed into a query key that moves at
 * human speed.
 *
 * It subscribes to the frame store directly rather than rendering on every
 * frame - the whole point of the store is that sixty pans a second are not
 * sixty renders a second, and a hook that re-rendered here would give that
 * back.
 */
export function useSettledRange(frames: FrameStore): SettledRange | null {
  const [range, setRange] = useState<SettledRange | null>(null);

  useEffect(() => {
    let timer: number | undefined;

    const apply = (): void => {
      const { view } = frames.getSnapshot();
      if (view.t1 <= view.t0) return;
      const next = settle(view.t0, view.t1);
      setRange((previous) =>
        previous && previous.since === next.since && previous.until === next.until
          ? previous
          : next,
      );
    };

    const unsubscribe = frames.subscribe(() => {
      if (timer !== undefined) window.clearTimeout(timer);
      timer = window.setTimeout(apply, QUIET_MS);
    });

    apply();

    return () => {
      if (timer !== undefined) window.clearTimeout(timer);
      unsubscribe();
    };
  }, [frames]);

  return range;
}
