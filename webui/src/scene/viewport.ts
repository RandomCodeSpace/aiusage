export const HOUR_MS = 3600e3;
export const DAY_MS = 24 * HOUR_MS;

/** Zoom stops. Below two hours the axis is noise; above sixty days so is the data. */
export const MIN_SPAN_MS = 2 * HOUR_MS;
export const MAX_SPAN_MS = 60 * DAY_MS;

/** How far past now the right edge sits while following live. */
export const FOLLOW_LEAD = 0.08;

/** Below this many pixels per hour, blocks dissolve into density ribbons. */
export const BLOCK_PX_PER_HOUR = 14;

/** A pointer that moved less than this is a tap, not a pan. */
export const TAP_SLOP_PX = 4;

/** Hit padding: a fingertip is not a pixel. */
export const HIT_PAD_TOUCH_PX = 9;
export const HIT_PAD_MOUSE_PX = 2;

/** Double-tap window and radius. */
export const DOUBLE_TAP_MS = 340;
export const DOUBLE_TAP_RADIUS_PX = 28;

/** An empty tap names the instant, then gets out of the way. */
export const EMPTY_TAP_TIP_MS = 1400;

/** Flick inertia: velocity floor to start, decay constant, floor to stop. */
export const FLICK_MIN_VELOCITY = 0.06;
export const INERTIA_DECAY_MS = 240;
export const INERTIA_STOP_VELOCITY = 0.02;
export const INERTIA_MAX_FRAME_MS = 48;

/** Scene geometry, in CSS pixels. */
export const AXIS_H = 26;
export const AGG_H = 96;
export const LANE_GAP = 1;
export const LANE_MIN_H = 64;

export interface View {
  t0: number;
  t1: number;
}

export function spanOf(view: View): number {
  return view.t1 - view.t0;
}

export function timeToX(view: View, width: number, t: number): number {
  return ((t - view.t0) / (view.t1 - view.t0)) * width;
}

export function xToTime(view: View, width: number, x: number): number {
  return view.t0 + (x / width) * (view.t1 - view.t0);
}

export function clampSpan(span: number): number {
  return Math.max(MIN_SPAN_MS, Math.min(MAX_SPAN_MS, span));
}

/** Zooms to `span`, keeping the instant under `anchorFraction` of the width fixed. */
export function zoomAround(span: number, anchorTime: number, anchorFraction: number): View {
  const t0 = anchorTime - span * anchorFraction;
  return { t0, t1: t0 + span };
}

/**
 * The scene may be panned into the past without limit, but not so far into
 * the past that the right edge outruns the data by a month - a blank screen
 * with no way back is not navigation.
 */
export function clampToHistory(view: View, now: number): View {
  const span = spanOf(view);
  if (view.t1 < now - 30 * DAY_MS) {
    const t1 = now - 30 * DAY_MS;
    return { t0: t1 - span, t1 };
  }
  return view;
}

export function followView(now: number, span: number): View {
  const t1 = now + span * FOLLOW_LEAD;
  return { t0: t1 - span, t1 };
}

/** The landing range: two weeks back, a little headroom ahead. */
export function fitView(now: number): View {
  return { t0: now - 14 * DAY_MS, t1: now + 6 * HOUR_MS };
}

export interface LaneRect {
  tool: string;
  y: number;
  h: number;
}

/**
 * One lane per tool, filling what is left under the aggregate. Lanes never go
 * below LANE_MIN_H; with enough tools in the ledger the last lanes run off
 * the bottom rather than becoming unreadable slivers.
 */
export function laneRects(tools: readonly string[], height: number): LaneRect[] {
  const top = AXIS_H + AGG_H + 8;
  const each = Math.max(LANE_MIN_H, (height - top - 6) / Math.max(1, tools.length));
  return tools.map((tool, i) => ({ tool, y: top + i * (each + LANE_GAP), h: each - 10 }));
}

/** Bin count for the aggregate and the density ribbons - one bin per ~5px. */
export function binCount(width: number): number {
  return Math.min(360, Math.max(60, Math.round(width / 5)));
}

/** Quantized 1/2/5 scale top, so the SCALE readout is a round number. */
export function niceTop(max: number): number {
  const decade = Math.pow(10, Math.floor(Math.log10(max)));
  const mantissa = max / decade;
  const step = mantissa <= 1 ? 1 : mantissa <= 2 ? 2 : mantissa <= 5 ? 5 : 10;
  return step * decade;
}
