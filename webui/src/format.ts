import { DAY_MS, HOUR_MS } from './scene/viewport';

/** Compact token counts: 1.2M, 340.0K, 12. */
export function formatCompact(n: number): string {
  if (n >= 1e9) return `${(n / 1e9).toFixed(2)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}K`;
  return String(Math.round(n));
}

export function formatUSD(n: number): string {
  return `$${n.toFixed(2)}`;
}

export function formatCount(n: number): string {
  return Math.round(n).toLocaleString();
}

function pad2(n: number): string {
  return String(n).padStart(2, '0');
}

export function formatClock(ms: number): string {
  const at = new Date(ms);
  return `${pad2(at.getHours())}:${pad2(at.getMinutes())}`;
}

export function formatDayMonth(ms: number): string {
  return new Date(ms).toLocaleDateString(undefined, { month: '2-digit', day: '2-digit' });
}

export function formatStamp(ms: number): string {
  return `${formatDayMonth(ms)} ${formatClock(ms)}`;
}

export function formatInstant(ms: number): string {
  return new Date(ms).toLocaleString(undefined, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  });
}

export function formatDurationMs(ms: number): string {
  const minutes = Math.round(ms / 60e3);
  if (minutes >= 60) return `${String(Math.floor(minutes / 60))}h ${String(minutes % 60)}m`;
  return `${String(minutes)}m`;
}

export function formatSpan(ms: number): string {
  const days = ms / DAY_MS;
  return days >= 2 ? `${days.toFixed(1)}d` : `${(ms / HOUR_MS).toFixed(1)}h`;
}

export function formatBytes(bytes: number): string {
  if (bytes >= 1 << 30) return `${(bytes / (1 << 30)).toFixed(1)} GB`;
  if (bytes >= 1 << 20) return `${Math.round(bytes / (1 << 20)).toString()} MB`;
  if (bytes >= 1 << 10) return `${Math.round(bytes / (1 << 10)).toString()} KB`;
  return `${String(bytes)} B`;
}

export function formatUptime(seconds: number): string {
  if (seconds >= 86_400) return `${String(Math.floor(seconds / 86_400))}d`;
  if (seconds >= 3600) return `${String(Math.floor(seconds / 3600))}h`;
  if (seconds >= 60) return `${String(Math.floor(seconds / 60))}m`;
  return `${String(Math.floor(seconds))}s`;
}
