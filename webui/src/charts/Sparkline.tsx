import { useMemo } from 'react';
import type { Bucket } from '../api/contract';
import { SeamSparkline } from './seam';
import type { SeriesPoint } from './seam';

export type BucketMetric = 'total' | 'events' | 'cost_micro_usd';

export interface SparklineProps {
  /** Time-bucketed summary rows, in the order the store returned them. */
  buckets: readonly Bucket[];
  /** The time dimension whose formatted key labels each point. */
  dimension: string;
  metric?: BucketMetric;
  color: string;
  ariaLabel: string;
  height?: number;
}

/**
 * The first subordinate chart surface: a trend line over server-aggregated
 * buckets, drawn through the seam.
 *
 * Design revision 4.1 mounts none of these - the scene is bespoke canvas and
 * owns the whole viewport - so this is exported rather than placed. It exists
 * as the working proof that the seam carries a real chart, and as the shape
 * the composition and per-tool surfaces will be built from.
 */
export function Sparkline({
  buckets,
  dimension,
  metric = 'total',
  color,
  ariaLabel,
  height,
}: SparklineProps) {
  const points = useMemo<SeriesPoint[]>(
    () =>
      buckets.map((bucket) => ({
        // The key comes off the wire already formatted by SQLite; nothing
        // here parses or re-derives it.
        key: bucket.keys[dimension] ?? '',
        value: bucket[metric],
      })),
    [buckets, dimension, metric],
  );

  return (
    <SeamSparkline
      points={points}
      color={color}
      ariaLabel={ariaLabel}
      height={height}
      className="sparkline"
    />
  );
}
