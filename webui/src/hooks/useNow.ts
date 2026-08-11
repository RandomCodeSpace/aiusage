import { useEffect, useRef, useState } from 'react';
import type { MetaResponse } from '../api/contract';

const TICK_MS = 1000;

/**
 * The scene's idea of now, anchored to the SERVER clock.
 *
 * The right edge of the trace is a claim about the present, and a browser
 * whose clock is minutes off would move the now line away from the data it is
 * supposed to mark. The daemon's now_unix supplies the offset; the local
 * clock supplies the ticking.
 */
export function useNow(meta: MetaResponse | undefined): number {
  const skew = useRef(0);
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!meta) return;
    skew.current = meta.now_unix * 1000 - Date.now();
    setNow(Date.now() + skew.current);
  }, [meta]);

  useEffect(() => {
    const timer = window.setInterval(() => {
      setNow(Date.now() + skew.current);
    }, TICK_MS);
    return () => {
      window.clearInterval(timer);
    };
  }, []);

  return now;
}
