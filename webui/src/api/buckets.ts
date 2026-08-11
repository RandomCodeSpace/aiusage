import type { Granularity } from './contract';

/**
 * Formats a unix second into the bucket key layout the store produces.
 *
 * This exists for ONE caller: the mock adapter, which has to invent keys the
 * real server gets from SQLite
 * (`strftime(layout, event_time_unix, 'unixepoch', 'localtime')`). Nothing
 * else in the UI may derive a bucket key - keys arrive on the wire and are
 * treated as opaque, lexically-sortable labels. Re-deriving them from a Date
 * is exactly the localtime/time.Local disagreement the project already fixed
 * once on the Go side.
 */
export function bucketKey(unixSeconds: number, granularity: Granularity): string {
  const at = new Date(unixSeconds * 1000);
  const year = String(at.getFullYear()).padStart(4, '0');
  const month = String(at.getMonth() + 1).padStart(2, '0');
  const day = String(at.getDate()).padStart(2, '0');
  switch (granularity) {
    case 'hour':
      return `${year}-${month}-${day} ${String(at.getHours()).padStart(2, '0')}`;
    case 'day':
      return `${year}-${month}-${day}`;
    case 'week':
      return `${year}-${String(sundayWeekOfYear(at)).padStart(2, '0')}`;
    case 'month':
      return `${year}-${month}`;
  }
}

/** SQLite's %W: week of year, Sunday as the first day, 00-53. */
function sundayWeekOfYear(at: Date): number {
  const startOfYear = new Date(at.getFullYear(), 0, 1);
  const dayOfYear = Math.floor((at.getTime() - startOfYear.getTime()) / 86_400_000);
  return Math.floor((dayOfYear + startOfYear.getDay()) / 7);
}
