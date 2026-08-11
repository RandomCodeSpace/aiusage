/* eslint-disable react-refresh/only-export-components -- the seam deliberately exports types, accessors and one component together; it is the boundary, not a component module. */
import { useMemo } from 'react';
import { defineChart, lineY } from '@tanstack/charts';
import { scaleLinear } from '@tanstack/charts/scales/linear';
import { Chart } from '@tanstack/charts/react/canvas';

/**
 * The ONLY file in this app allowed to import @tanstack/charts.
 *
 * The library is pre-alpha and says so. Its breaking changes have arrived
 * through import specifiers (0.9.0), option renames (0.8.0) and callback
 * signatures (0.7.0) - never through component props - so a props-only
 * wrapper would not have caught a single one of them. This seam therefore
 * owns:
 *
 *   - every import specifier, including the renderer entry point,
 *   - the renderer choice (canvas: no SSR, no server-painted pixels),
 *   - our own point type and props, which upstream never sees,
 *   - our accessor convention.
 *
 * The eslint rule no-restricted-imports enforces the boundary; this file is
 * its single exemption. When the library breaks, this file is the blast
 * radius.
 *
 * One convention worth stating: a point's x is its ORDINAL POSITION, and its
 * key is an opaque label formatted by the store
 * (strftime over event_time_unix, in local time). The seam never re-derives a
 * bucket from a Date - that is the localtime/time.Local disagreement the Go
 * side already had to fix once.
 */

/** One point of a subordinate chart surface. Bucket key in, number out. */
export interface SeriesPoint {
  /** The store's formatted bucket key, e.g. "2026-08-11 07". Opaque. */
  key: string;
  value: number;
}

/** Our accessor convention, so no call site reaches into a datum directly. */
export const pointKey = (point: SeriesPoint): string => point.key;
export const pointValue = (point: SeriesPoint): number => point.value;

export interface SeamSparklineProps {
  points: readonly SeriesPoint[];
  /** Resolved colour, not a CSS variable: canvas cannot resolve var(). */
  color: string;
  ariaLabel: string;
  height?: number;
  className?: string;
}

const DEFAULT_HEIGHT = 34;

/**
 * A bare trend line: no axes, no legend, no tooltip. Everything a sparkline
 * would otherwise borrow from the page it sits in, the page already says.
 */
export function SeamSparkline({
  points,
  color,
  ariaLabel,
  height = DEFAULT_HEIGHT,
  className,
}: SeamSparklineProps) {
  const definition = useMemo(
    () =>
      defineChart({
        marks: [
          lineY(points, {
            x: (_point: SeriesPoint, context: { index: number }) => context.index,
            y: pointValue,
            stroke: color,
            strokeWidth: 1.4,
          }),
        ],
        // A sparkline borrows its context from the page around it: no axes,
        // no grid, no legend.
        guides: true,
        x: { scale: scaleLinear, axis: false, grid: false },
        y: { scale: scaleLinear, axis: false, grid: false },
      }),
    [points, color],
  );

  return (
    <Chart
      definition={definition}
      ariaLabel={ariaLabel}
      height={height}
      className={className}
    />
  );
}
