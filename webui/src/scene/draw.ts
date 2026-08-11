import { formatCompact } from '../format';
import type { Palette } from '../theme';
import type { TraceSession } from './sessions';
import type { LaneRect, View } from './viewport';
import {
  AGG_H,
  AXIS_H,
  BLOCK_PX_PER_HOUR,
  DAY_MS,
  HOUR_MS,
  binCount,
  laneRects,
  niceTop,
  timeToX,
} from './viewport';

const MONO = '10px ui-monospace, "SF Mono", Menlo, monospace';

/** A block as it was actually painted, which is what hit testing needs. */
export interface DrawnBlock {
  x: number;
  y: number;
  w: number;
  h: number;
  session: TraceSession;
}

export interface DrawParams {
  ctx: CanvasRenderingContext2D;
  width: number;
  height: number;
  view: View;
  sessions: readonly TraceSession[];
  tools: readonly string[];
  /**
   * Per-tool tokens-per-minute norm, computed once when data changes. A norm
   * recomputed from the visible sessions would make every block breathe as
   * the scene pans.
   */
  laneNorms: Record<string, number>;
  palette: Palette;
  hoverX: number;
  hoverId: string | null;
  selectedId: string | null;
}

/** Tokens per minute of wall time, the intensity a block's height encodes. */
export function sessionRate(session: TraceSession): number {
  return session.tokens / Math.max(1, session.durationMs / 60e3);
}

/** Mean rate per tool over the whole dataset - stable while panning. */
export function laneNormsOf(sessions: readonly TraceSession[]): Record<string, number> {
  const sums: Record<string, { total: number; n: number }> = {};
  for (const session of sessions) {
    const entry = (sums[session.tool] ??= { total: 0, n: 0 });
    entry.total += sessionRate(session);
    entry.n += 1;
  }
  const norms: Record<string, number> = {};
  for (const [tool, entry] of Object.entries(sums)) {
    norms[tool] = entry.n > 0 ? entry.total / entry.n : 1;
  }
  return norms;
}

/** Sums token mass into `bins` equal time slices of the view. */
function binSessions(
  sessions: readonly TraceSession[],
  view: View,
  bins: number,
  accept: (session: TraceSession) => boolean,
): { acc: Float64Array; total: number; count: number } {
  const span = view.t1 - view.t0;
  const binWidth = span / bins;
  const acc = new Float64Array(bins);
  let total = 0;
  let count = 0;
  for (const session of sessions) {
    if (!accept(session)) continue;
    if (session.endMs < view.t0 || session.startMs > view.t1) continue;
    total += session.tokens;
    count += 1;
    const first = Math.max(0, Math.floor((session.startMs - view.t0) / binWidth));
    const last = Math.min(bins - 1, Math.floor((session.endMs - view.t0) / binWidth));
    const perBin = session.tokens / (last - first + 1);
    for (let b = first; b <= last; b++) acc[b] += perBin;
  }
  return { acc, total, count };
}

function drawWeekends(p: DrawParams): void {
  const { ctx, view, width, height } = p;
  ctx.fillStyle = 'rgba(46,52,54,0.035)';
  const start = new Date(view.t0);
  start.setHours(0, 0, 0, 0);
  for (let t = start.getTime(); t < view.t1; t += DAY_MS) {
    const weekday = new Date(t).getDay();
    if (weekday !== 0 && weekday !== 6) continue;
    const x0 = Math.max(0, timeToX(view, width, t));
    const x1 = Math.min(width, timeToX(view, width, t + DAY_MS));
    if (x1 > x0) ctx.fillRect(x0, AXIS_H, x1 - x0, height - AXIS_H);
  }
}

/** Tick step from zoom: fifteen minutes at the bottom, a week at the top. */
const TICK_STEPS: { step: number; day: boolean }[] = [
  { step: 0.25 * HOUR_MS, day: false },
  { step: HOUR_MS, day: false },
  { step: 3 * HOUR_MS, day: false },
  { step: 6 * HOUR_MS, day: false },
  { step: DAY_MS, day: true },
  { step: 7 * DAY_MS, day: true },
];

function drawAxis(p: DrawParams, pxPerHour: number): void {
  const { ctx, view, width, height, palette } = p;
  let chosen = TICK_STEPS[TICK_STEPS.length - 1];
  for (const candidate of TICK_STEPS) {
    if ((candidate.step / HOUR_MS) * pxPerHour >= 74) {
      chosen = candidate;
      break;
    }
  }
  const { step, day: dayMode } = chosen;

  ctx.textAlign = 'left';
  ctx.lineWidth = 1;

  for (let t = Math.ceil(view.t0 / step) * step; t < view.t1; t += step) {
    const x = Math.round(timeToX(view, width, t)) + 0.5;
    const at = new Date(t);
    const midnight = at.getHours() === 0 && at.getMinutes() === 0;
    ctx.strokeStyle = midnight ? palette.get('--line') : palette.get('--line-soft');
    ctx.beginPath();
    ctx.moveTo(x, AXIS_H);
    ctx.lineTo(x, height);
    ctx.stroke();
    const label =
      dayMode || midnight
        ? at.toLocaleDateString(undefined, { month: '2-digit', day: '2-digit' })
        : `${String(at.getHours()).padStart(2, '0')}:${String(at.getMinutes()).padStart(2, '0')}`;
    ctx.fillStyle = midnight ? palette.get('--ink-2') : palette.get('--ink-3');
    ctx.fillText(label, x + 4, 16);
  }
}

/**
 * The page's pulse: every tool binned together into one area, on a quantized
 * 1/2/5 scale so the SCALE readout is a number a person can hold.
 */
function drawAggregate(p: DrawParams): void {
  const { ctx, view, width, sessions, palette } = p;
  const bins = binCount(width);
  const { acc } = binSessions(sessions, view, bins, () => true);

  let max = 1;
  for (const value of acc) if (value > max) max = value;
  const top = niceTop(max);

  const y0 = AXIS_H + 6;
  const h = AGG_H - 12;
  ctx.strokeStyle = palette.get('--line-soft');
  for (const fraction of [0.5, 1]) {
    const y = Math.round(y0 + h - fraction * h) + 0.5;
    ctx.beginPath();
    ctx.moveTo(0, y);
    ctx.lineTo(width, y);
    ctx.stroke();
  }

  const amber = palette.get('--accent');
  ctx.beginPath();
  ctx.moveTo(0, y0 + h);
  for (let b = 0; b < bins; b++) {
    ctx.lineTo(((b + 0.5) / bins) * width, y0 + h - (acc[b] / top) * h);
  }
  ctx.lineTo(width, y0 + h);
  ctx.closePath();
  const gradient = ctx.createLinearGradient(0, y0, 0, y0 + h);
  gradient.addColorStop(0, `${amber}55`);
  gradient.addColorStop(1, `${amber}08`);
  ctx.fillStyle = gradient;
  ctx.fill();

  ctx.beginPath();
  for (let b = 0; b < bins; b++) {
    const x = ((b + 0.5) / bins) * width;
    const y = y0 + h - (acc[b] / top) * h;
    if (b === 0) ctx.moveTo(x, y);
    else ctx.lineTo(x, y);
  }
  ctx.strokeStyle = amber;
  ctx.lineWidth = 1.4;
  ctx.stroke();

  ctx.fillStyle = palette.get('--ink-2');
  ctx.textAlign = 'left';
  ctx.fillText('all tools', 8, y0 + 11);
  ctx.fillStyle = palette.get('--ink-3');
  ctx.fillText(`SCALE ${formatCompact(top / 2)}/div`, 8, y0 + 23);
}

function drawBlocks(p: DrawParams, lane: LaneRect, color: string, out: DrawnBlock[]): {
  total: number;
  count: number;
} {
  const { ctx, view, width, sessions, laneNorms, palette, hoverId, selectedId } = p;
  const norm = Math.max(1, laneNorms[lane.tool] ?? 1);
  let total = 0;
  let count = 0;

  for (const session of sessions) {
    if (session.tool !== lane.tool) continue;
    if (session.endMs < view.t0 || session.startMs > view.t1) continue;
    total += session.tokens;
    count += 1;

    const x = timeToX(view, width, session.startMs);
    const w = Math.max(3, timeToX(view, width, session.endMs) - x);
    // Block height encodes intensity: this session's tokens per minute
    // against the lane's own norm, floored so a quiet session is still a
    // target you can hit.
    const heightFraction = Math.min(1, 0.25 + sessionRate(session) / (norm * 2.4));
    const h = Math.max(10, (lane.h - 18) * heightFraction);
    const y = lane.y + lane.h - 6 - h;

    const selected = selectedId === session.id;
    const hovered = hoverId === session.id;
    ctx.fillStyle = color + (selected || hovered ? 'e8' : '9e');
    ctx.strokeStyle = color;
    ctx.lineWidth = selected ? 2 : 1;
    ctx.beginPath();
    ctx.roundRect(x, y, w, h, 2);
    ctx.fill();
    ctx.stroke();

    if (session.live) {
      // The running session gets an amber cap at its right edge - the only
      // place on the scene where the present is marked inside the data.
      ctx.fillStyle = palette.get('--accent');
      ctx.fillRect(x + w - 2.5, y - 3, 2.5, h + 3);
    }

    if (w > 64) {
      ctx.save();
      ctx.beginPath();
      ctx.rect(x + 3, y, w - 6, h);
      ctx.clip();
      ctx.fillStyle = '#ffffff';
      ctx.fillText(session.project, x + 6, y + Math.min(h - 4, 14));
      ctx.restore();
    }

    out.push({ x, y, w, h, session });
  }

  return { total, count };
}

function drawRibbon(p: DrawParams, lane: LaneRect, color: string): { total: number; count: number } {
  const { ctx, view, width, sessions } = p;
  const bins = binCount(width);
  const { acc, total, count } = binSessions(
    sessions,
    view,
    bins,
    (session) => session.tool === lane.tool,
  );

  let max = 1;
  for (const value of acc) if (value > max) max = value;
  const h = lane.h - 16;
  const floor = lane.y + lane.h - 6;

  ctx.beginPath();
  ctx.moveTo(0, floor);
  for (let b = 0; b < bins; b++) {
    ctx.lineTo(((b + 0.5) / bins) * width, floor - (acc[b] / max) * h);
  }
  ctx.lineTo(width, floor);
  ctx.closePath();
  ctx.fillStyle = `${color}38`;
  ctx.fill();

  ctx.beginPath();
  for (let b = 0; b < bins; b++) {
    const x = ((b + 0.5) / bins) * width;
    const y = floor - (acc[b] / max) * h;
    if (b === 0) ctx.moveTo(x, y);
    else ctx.lineTo(x, y);
  }
  ctx.strokeStyle = color;
  ctx.lineWidth = 1.3;
  ctx.stroke();

  return { total, count };
}

function drawLanes(p: DrawParams, pxPerHour: number, out: DrawnBlock[]): void {
  const { ctx, width, height, tools, palette } = p;
  const showBlocks = pxPerHour >= BLOCK_PX_PER_HOUR;

  for (const lane of laneRects(tools, height)) {
    const color = palette.tool(lane.tool);

    ctx.fillStyle = 'rgba(255,255,255,0.55)';
    ctx.fillRect(0, lane.y, width, lane.h);
    ctx.strokeStyle = palette.get('--line-soft');
    ctx.strokeRect(-1, lane.y + 0.5, width + 2, lane.h);

    const { total, count } = showBlocks
      ? drawBlocks(p, lane, color, out)
      : drawRibbon(p, lane, color);

    ctx.fillStyle = palette.get('--ink-2');
    ctx.textAlign = 'left';
    ctx.fillText(lane.tool, 8, lane.y + 14);
    ctx.fillStyle = palette.get('--ink-3');
    ctx.fillText(
      `${formatCompact(total)} tok - ${String(count)}${showBlocks ? ' sessions' : ' sessions (zoom for blocks)'}`,
      8,
      lane.y + 26,
    );
  }
}

function drawCrosshair(p: DrawParams): void {
  const { ctx, hoverX, height, palette } = p;
  if (hoverX < 0) return;
  ctx.save();
  ctx.strokeStyle = palette.get('--ink-3');
  ctx.globalAlpha = 0.5;
  ctx.setLineDash([2, 3]);
  ctx.beginPath();
  ctx.moveTo(hoverX + 0.5, AXIS_H);
  ctx.lineTo(hoverX + 0.5, height);
  ctx.stroke();
  ctx.restore();
}

/** Paints one frame and returns the blocks it actually put on screen. */
export function drawScene(p: DrawParams): DrawnBlock[] {
  const { ctx, width, height, view } = p;
  ctx.clearRect(0, 0, width, height);
  ctx.font = MONO;

  const pxPerHour = width / ((view.t1 - view.t0) / HOUR_MS);
  const blocks: DrawnBlock[] = [];

  drawWeekends(p);
  drawAxis(p, pxPerHour);
  drawAggregate(p);
  drawLanes(p, pxPerHour, blocks);
  drawCrosshair(p);

  return blocks;
}
