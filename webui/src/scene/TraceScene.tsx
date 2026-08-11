import { useEffect, useImperativeHandle, useRef } from 'react';
import type { RefObject } from 'react';
import { SceneController } from './controller';
import type { SceneHandlers } from './controller';
import type { FrameStore } from './frame';
import type { TraceSession } from './sessions';

/** Commands the DOM chrome issues at the scene. */
export interface SceneHandle {
  toggleFollow: () => void;
  zoomToSession: (session: TraceSession) => void;
  fit: () => void;
}

export interface TraceSceneProps {
  sessions: readonly TraceSession[];
  /** Lane order, one per tool id. */
  tools: readonly string[];
  nowMs: number;
  selectedId: string | null;
  reducedMotion: boolean;
  frames: FrameStore;
  onSelect: (session: TraceSession | null) => void;
  onFollowChange: (follow: boolean) => void;
  handleRef: RefObject<SceneHandle | null>;
}

/**
 * The trace scene: one canvas, one controller, no React in the draw path.
 *
 * React owns what changes rarely - the data, the selection, the reduced-motion
 * preference - and pushes it in. Everything that changes at pointer speed
 * stays inside the controller and reaches the DOM through the frame store.
 */
export function TraceScene({
  sessions,
  tools,
  nowMs,
  selectedId,
  reducedMotion,
  frames,
  onSelect,
  onFollowChange,
  handleRef,
}: TraceSceneProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const controllerRef = useRef<SceneController | null>(null);

  // Handlers are read through a stable object so a re-render never re-attaches
  // pointer listeners mid-gesture.
  const handlers = useRef<SceneHandlers>({
    onFrame: frames.publish,
    onSelect,
    onFollowChange,
  });
  const initialReducedMotion = useRef(reducedMotion);

  useEffect(() => {
    handlers.current.onFrame = frames.publish;
    handlers.current.onSelect = onSelect;
    handlers.current.onFollowChange = onFollowChange;
  });

  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas) return;
    const stable: SceneHandlers = {
      onFrame: (frame) => {
        handlers.current.onFrame(frame);
      },
      onSelect: (session) => {
        handlers.current.onSelect(session);
      },
      onFollowChange: (follow) => {
        handlers.current.onFollowChange(follow);
      },
    };
    // The reduced-motion preference is a constructor argument; the effect
    // below keeps it live afterwards.
    const controller = new SceneController(canvas, stable, initialReducedMotion.current);
    controllerRef.current = controller;
    return () => {
      controller.destroy();
      controllerRef.current = null;
    };
  }, []);

  useImperativeHandle(
    handleRef,
    () => ({
      toggleFollow: () => controllerRef.current?.toggleFollow(),
      zoomToSession: (session: TraceSession) => controllerRef.current?.zoomToSession(session),
      fit: () => controllerRef.current?.fit(),
    }),
    [],
  );

  useEffect(() => {
    controllerRef.current?.setData(sessions, tools);
  }, [sessions, tools]);

  useEffect(() => {
    controllerRef.current?.setNow(nowMs);
  }, [nowMs]);

  useEffect(() => {
    controllerRef.current?.setSelected(selectedId);
  }, [selectedId]);

  useEffect(() => {
    controllerRef.current?.setReducedMotion(reducedMotion);
  }, [reducedMotion]);

  return (
    <canvas
      ref={canvasRef}
      className="scene"
      aria-label="Zoomable timeline of agent sessions per tool. Newest at the right edge."
    />
  );
}
