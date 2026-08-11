import { Palette } from '../theme';
import type { DrawnBlock } from './draw';
import { drawScene, laneNormsOf } from './draw';
import type { HoverState, SceneFrame } from './frame';
import type { TraceSession } from './sessions';
import { visibleTotals } from './totals';
import type { View } from './viewport';
import {
  DOUBLE_TAP_MS,
  DOUBLE_TAP_RADIUS_PX,
  EMPTY_TAP_TIP_MS,
  FLICK_MIN_VELOCITY,
  HIT_PAD_MOUSE_PX,
  HIT_PAD_TOUCH_PX,
  HOUR_MS,
  INERTIA_DECAY_MS,
  INERTIA_MAX_FRAME_MS,
  INERTIA_STOP_VELOCITY,
  TAP_SLOP_PX,
  clampSpan,
  clampToHistory,
  fitView,
  followView,
  spanOf,
  timeToX,
  xToTime,
  zoomAround,
} from './viewport';

/** Callbacks the controller invokes. Mutated in place so listeners never rebind. */
export interface SceneHandlers {
  onFrame: (frame: SceneFrame) => void;
  onSelect: (session: TraceSession | null) => void;
  onFollowChange: (follow: boolean) => void;
}

interface DragState {
  startX: number;
  t0: number;
  t1: number;
  moved: boolean;
  lastX: number;
  lastT: number;
  velocity: number;
}

interface PinchState {
  distance0: number;
  span0: number;
  midTime: number;
}

interface InertiaState {
  velocity: number;
  at: number;
}

interface PointerSample {
  x: number;
  y: number;
}

/**
 * The whole interaction layer of the trace scene: wheel zoom anchored at the
 * cursor, one-pointer pan with flick inertia, two-pointer pinch with midpoint
 * anchoring and single-finger handoff, tap to select, double-tap to dive, a
 * keyboard map, and an invalidation-only draw loop so a still scene costs
 * nothing.
 *
 * It lives outside React on purpose: sixty pans a second are not sixty
 * renders a second.
 */
export class SceneController {
  private readonly canvas: HTMLCanvasElement;
  private readonly ctx: CanvasRenderingContext2D;
  private readonly handlers: SceneHandlers;
  private readonly palette = new Palette();

  private width = 0;
  private height = 0;
  private rect: DOMRect;

  private view: View;
  private nowMs: number;
  private follow = true;
  private reducedMotion: boolean;

  private sessions: readonly TraceSession[] = [];
  private tools: readonly string[] = [];
  private laneNorms: Record<string, number> = {};

  private blocks: DrawnBlock[] = [];
  private hoverX = -1;
  private hoverId: string | null = null;
  private hover: HoverState | null = null;
  private selectedId: string | null = null;

  private readonly pointers = new Map<number, PointerSample>();
  private drag: DragState | null = null;
  private pinch: PinchState | null = null;
  private inertia: InertiaState | null = null;
  private lastTap = { at: 0, x: 0, y: 0 };
  private tipTimer: number | undefined;

  private dirty = true;
  private rafHandle = 0;
  private readonly resizeObserver: ResizeObserver;

  constructor(canvas: HTMLCanvasElement, handlers: SceneHandlers, reducedMotion: boolean) {
    const ctx = canvas.getContext('2d');
    if (!ctx) throw new Error('scene: 2d canvas context unavailable');
    this.canvas = canvas;
    this.ctx = ctx;
    this.handlers = handlers;
    this.reducedMotion = reducedMotion;
    this.rect = canvas.getBoundingClientRect();
    this.nowMs = Date.now();
    this.view = followView(this.nowMs, 3 * 24 * HOUR_MS);

    this.resizeObserver = new ResizeObserver(() => {
      this.resize();
    });
    this.resizeObserver.observe(canvas);

    canvas.addEventListener('wheel', this.onWheel, { passive: false });
    canvas.addEventListener('pointerdown', this.onPointerDown);
    canvas.addEventListener('pointermove', this.onPointerMove);
    canvas.addEventListener('pointerup', this.onPointerUp);
    canvas.addEventListener('pointercancel', this.onPointerCancel);
    canvas.addEventListener('pointerleave', this.onPointerLeave);
    window.addEventListener('keydown', this.onKeyDown);

    this.resize();
    this.rafHandle = requestAnimationFrame(this.loop);
  }

  destroy(): void {
    cancelAnimationFrame(this.rafHandle);
    this.resizeObserver.disconnect();
    this.canvas.removeEventListener('wheel', this.onWheel);
    this.canvas.removeEventListener('pointerdown', this.onPointerDown);
    this.canvas.removeEventListener('pointermove', this.onPointerMove);
    this.canvas.removeEventListener('pointerup', this.onPointerUp);
    this.canvas.removeEventListener('pointercancel', this.onPointerCancel);
    this.canvas.removeEventListener('pointerleave', this.onPointerLeave);
    window.removeEventListener('keydown', this.onKeyDown);
    if (this.tipTimer !== undefined) window.clearTimeout(this.tipTimer);
  }

  // ------------------------------------------------------------------ inputs

  setData(sessions: readonly TraceSession[], tools: readonly string[]): void {
    this.sessions = sessions;
    this.tools = tools;
    this.laneNorms = laneNormsOf(sessions);
    if (this.hoverId && !sessions.some((s) => s.id === this.hoverId)) this.hoverId = null;
    this.invalidate();
  }

  setNow(nowMs: number): void {
    this.nowMs = nowMs;
    this.invalidate();
  }

  setSelected(id: string | null): void {
    if (this.selectedId === id) return;
    this.selectedId = id;
    this.invalidate();
  }

  setReducedMotion(reduced: boolean): void {
    this.reducedMotion = reduced;
    if (reduced) this.inertia = null;
  }

  // ----------------------------------------------------------------- commands

  setFollow(on: boolean): void {
    if (this.follow === on) return;
    this.follow = on;
    this.handlers.onFollowChange(on);
    this.invalidate();
  }

  /** The follow chip: turning it back on brings the view to the live edge. */
  toggleFollow(): void {
    if (!this.follow) this.view = followView(this.nowMs, spanOf(this.view));
    this.setFollow(!this.follow);
    this.invalidate();
  }

  zoomToSession(session: TraceSession): void {
    const pad = session.durationMs * 0.6;
    this.view = { t0: session.startMs - pad, t1: session.endMs + pad };
    this.setFollow(false);
    this.invalidate();
  }

  fit(): void {
    this.view = fitView(this.nowMs);
    this.setFollow(false);
    this.invalidate();
  }

  panByFraction(fraction: number): void {
    const span = spanOf(this.view);
    this.view = { t0: this.view.t0 + span * fraction, t1: this.view.t1 + span * fraction };
    this.setFollow(false);
    this.invalidate();
  }

  zoomByFactor(factor: number): void {
    const centre = (this.view.t0 + this.view.t1) / 2;
    const span = clampSpan(spanOf(this.view) * factor);
    this.view = { t0: centre - span / 2, t1: centre + span / 2 };
    this.setFollow(false);
    this.invalidate();
  }

  // -------------------------------------------------------------------- loop

  private invalidate(): void {
    this.dirty = true;
  }

  private resize(): void {
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    this.rect = this.canvas.getBoundingClientRect();
    this.width = this.canvas.clientWidth;
    this.height = this.canvas.clientHeight;
    this.canvas.width = Math.round(this.width * dpr);
    this.canvas.height = Math.round(this.height * dpr);
    this.ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    this.invalidate();
  }

  private readonly loop = (): void => {
    if (this.inertia) this.stepInertia();
    if (this.dirty) {
      this.dirty = false;
      this.draw();
    }
    this.rafHandle = requestAnimationFrame(this.loop);
  };

  private stepInertia(): void {
    const inertia = this.inertia;
    if (!inertia) return;
    const now = performance.now();
    const dt = Math.min(INERTIA_MAX_FRAME_MS, now - inertia.at);
    inertia.at = now;
    const shift = ((-inertia.velocity * dt) / this.width) * spanOf(this.view);
    this.view = { t0: this.view.t0 + shift, t1: this.view.t1 + shift };
    inertia.velocity *= Math.exp(-dt / INERTIA_DECAY_MS);
    if (Math.abs(inertia.velocity) < INERTIA_STOP_VELOCITY) this.inertia = null;
    this.invalidate();
  }

  private draw(): void {
    if (this.width === 0 || this.height === 0) return;
    if (this.follow) this.view = followView(this.nowMs, spanOf(this.view));

    this.blocks = drawScene({
      ctx: this.ctx,
      width: this.width,
      height: this.height,
      view: this.view,
      sessions: this.sessions,
      tools: this.tools,
      laneNorms: this.laneNorms,
      palette: this.palette,
      hoverX: this.hoverX,
      hoverId: this.hoverId,
      selectedId: this.selectedId,
    });

    this.publish();
  }

  private publish(): void {
    const nowX = timeToX(this.view, this.width, this.nowMs);
    this.handlers.onFrame({
      view: { ...this.view },
      totals: visibleTotals(this.sessions, this.view),
      nowMs: this.nowMs,
      nowX: nowX < 0 || nowX > this.width ? null : nowX,
      pxPerHour: this.width / (spanOf(this.view) / HOUR_MS),
      hover: this.hover,
      follow: this.follow,
    });
  }

  // ---------------------------------------------------------------- hit test

  private localX(clientX: number): number {
    return clientX - this.rect.left;
  }

  private localY(clientY: number): number {
    return clientY - this.rect.top;
  }

  private hitAt(clientX: number, clientY: number, pad: number): DrawnBlock | undefined {
    const x = this.localX(clientX);
    const y = this.localY(clientY);
    return this.blocks.find(
      (b) => x >= b.x - pad && x <= b.x + b.w + pad && y >= b.y - pad && y <= b.y + b.h + pad,
    );
  }

  private setHover(clientX: number, clientY: number, session: TraceSession | null): void {
    this.hover = {
      clientX,
      clientY,
      session,
      instantMs: xToTime(this.view, this.width, this.localX(clientX)),
    };
  }

  private clearHover(): void {
    this.hover = null;
    this.hoverId = null;
    this.hoverX = -1;
    if (this.tipTimer !== undefined) {
      window.clearTimeout(this.tipTimer);
      this.tipTimer = undefined;
    }
  }

  // ----------------------------------------------------------------- pointer

  private readonly onWheel = (event: WheelEvent): void => {
    event.preventDefault();
    this.inertia = null;
    const factor = Math.exp(event.deltaY * 0.0016);
    const x = this.localX(event.clientX);
    const anchor = xToTime(this.view, this.width, x);
    const span = clampSpan(spanOf(this.view) * factor);
    const fraction = (anchor - this.view.t0) / spanOf(this.view);
    this.view = clampToHistory(zoomAround(span, anchor, fraction), this.nowMs);
    this.setFollow(false);
    this.invalidate();
  };

  private readonly onPointerDown = (event: PointerEvent): void => {
    try {
      this.canvas.setPointerCapture(event.pointerId);
    } catch {
      // Pointer capture is best effort; losing it costs a smoother gesture,
      // not the gesture.
    }
    this.pointers.set(event.pointerId, { x: event.clientX, y: event.clientY });
    this.inertia = null;

    if (this.pointers.size === 2) {
      const [a, b] = [...this.pointers.values()];
      if (a && b) {
        const mid = this.localX((a.x + b.x) / 2);
        this.pinch = {
          distance0: Math.max(24, Math.abs(a.x - b.x)),
          span0: spanOf(this.view),
          midTime: xToTime(this.view, this.width, mid),
        };
      }
      this.drag = null;
    } else if (this.pointers.size === 1) {
      this.drag = {
        startX: event.clientX,
        t0: this.view.t0,
        t1: this.view.t1,
        moved: false,
        lastX: event.clientX,
        lastT: performance.now(),
        velocity: 0,
      };
    }
    this.canvas.classList.add('dragging');
  };

  private readonly onPointerMove = (event: PointerEvent): void => {
    const sample = this.pointers.get(event.pointerId);
    if (sample) {
      sample.x = event.clientX;
      sample.y = event.clientY;
    }

    if (this.pinch && this.pointers.size >= 2) {
      this.applyPinch();
      return;
    }

    if (this.drag) {
      this.applyDrag(event);
      return;
    }

    if (event.pointerType === 'mouse') {
      this.hoverX = this.localX(event.clientX);
      const hit = this.hitAt(event.clientX, event.clientY, HIT_PAD_MOUSE_PX);
      this.hoverId = hit ? hit.session.id : null;
      this.setHover(event.clientX, event.clientY, hit ? hit.session : null);
      this.canvas.style.cursor = this.hoverId ? 'pointer' : 'grab';
      this.invalidate();
    }
  };

  private applyPinch(): void {
    const pinch = this.pinch;
    if (!pinch) return;
    const [a, b] = [...this.pointers.values()];
    if (!a || !b) return;
    const distance = Math.max(24, Math.abs(a.x - b.x));
    const span = clampSpan((pinch.span0 * pinch.distance0) / distance);
    const midX = this.localX((a.x + b.x) / 2);
    this.view = zoomAround(span, pinch.midTime, midX / this.width);
    this.setFollow(false);
    this.invalidate();
  }

  private applyDrag(event: PointerEvent): void {
    const drag = this.drag;
    if (!drag) return;
    const now = performance.now();
    const dx = event.clientX - drag.startX;
    if (Math.abs(dx) > TAP_SLOP_PX) drag.moved = true;
    const shift = (-dx / this.width) * (drag.t1 - drag.t0);
    this.view = { t0: drag.t0 + shift, t1: drag.t1 + shift };
    const frameMs = Math.max(1, now - drag.lastT);
    drag.velocity = 0.8 * drag.velocity + 0.2 * ((event.clientX - drag.lastX) / frameMs);
    drag.lastX = event.clientX;
    drag.lastT = now;
    if (drag.moved) this.setFollow(false);
    this.invalidate();
  }

  private readonly onPointerUp = (event: PointerEvent): void => {
    this.pointers.delete(event.pointerId);

    if (this.pinch) {
      if (this.pointers.size < 2) {
        this.pinch = null;
        // A finger is still down: hand off to a fresh pan rather than
        // stranding the gesture until the user lifts and starts again.
        const rest = [...this.pointers.values()][0];
        if (rest) {
          this.drag = {
            startX: rest.x,
            t0: this.view.t0,
            t1: this.view.t1,
            moved: true,
            lastX: rest.x,
            lastT: performance.now(),
            velocity: 0,
          };
        }
      }
      if (this.pointers.size === 0) this.canvas.classList.remove('dragging');
      this.invalidate();
      return;
    }

    const drag = this.drag;
    if (drag?.moved) {
      if (!this.reducedMotion && Math.abs(drag.velocity) > FLICK_MIN_VELOCITY) {
        this.inertia = { velocity: drag.velocity, at: performance.now() };
      }
    } else if (drag) {
      this.handleTap(event);
    }

    this.drag = null;
    if (this.pointers.size === 0) this.canvas.classList.remove('dragging');
    this.invalidate();
  };

  private handleTap(event: PointerEvent): void {
    const touch = event.pointerType !== 'mouse';
    const now = performance.now();

    const doubleTap =
      touch &&
      now - this.lastTap.at < DOUBLE_TAP_MS &&
      Math.hypot(event.clientX - this.lastTap.x, event.clientY - this.lastTap.y) <
        DOUBLE_TAP_RADIUS_PX;

    if (doubleTap) {
      const x = this.localX(event.clientX);
      const span = clampSpan(spanOf(this.view) * 0.5);
      this.view = zoomAround(span, xToTime(this.view, this.width, x), x / this.width);
      this.setFollow(false);
      this.lastTap = { at: 0, x: 0, y: 0 };
      return;
    }

    const hit = this.hitAt(
      event.clientX,
      event.clientY,
      touch ? HIT_PAD_TOUCH_PX : HIT_PAD_MOUSE_PX,
    );
    if (hit) {
      this.selectedId = hit.session.id;
      this.handlers.onSelect(hit.session);
    } else if (touch) {
      // A tap on empty canvas names the instant, briefly. Touch has no
      // hover, so this is the only way to ask "when is this".
      this.hoverId = null;
      this.setHover(event.clientX, event.clientY, null);
      if (this.tipTimer !== undefined) window.clearTimeout(this.tipTimer);
      this.tipTimer = window.setTimeout(() => {
        this.tipTimer = undefined;
        this.hover = null;
        this.invalidate();
        this.publish();
      }, EMPTY_TAP_TIP_MS);
    } else {
      this.selectedId = null;
      this.handlers.onSelect(null);
    }
    this.lastTap = { at: now, x: event.clientX, y: event.clientY };
  }

  private readonly onPointerCancel = (event: PointerEvent): void => {
    this.pointers.delete(event.pointerId);
    this.drag = null;
    this.pinch = null;
    this.canvas.classList.remove('dragging');
    this.invalidate();
  };

  private readonly onPointerLeave = (): void => {
    if (this.tipTimer !== undefined) return;
    this.clearHover();
    this.invalidate();
  };

  // ---------------------------------------------------------------- keyboard

  private readonly onKeyDown = (event: KeyboardEvent): void => {
    switch (event.key) {
      case 'ArrowLeft':
        this.panByFraction(-0.15);
        break;
      case 'ArrowRight':
        this.panByFraction(0.15);
        break;
      case '+':
      case '=':
        this.zoomByFactor(0.7);
        break;
      case '-':
        this.zoomByFactor(1 / 0.7);
        break;
      case '0':
        this.fit();
        break;
      case 'l':
      case 'L':
        this.toggleFollow();
        break;
      case 'Escape':
        this.selectedId = null;
        this.handlers.onSelect(null);
        this.invalidate();
        break;
      default:
        return;
    }
  };
}
