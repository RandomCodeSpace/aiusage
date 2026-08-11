import { useSyncExternalStore } from 'react';
import type { FrameStore, SceneFrame } from '../scene/frame';

/** Subscribes a chrome component to the scene's published frames. */
export function useSceneFrame(frames: FrameStore): SceneFrame {
  return useSyncExternalStore(frames.subscribe, frames.getSnapshot, frames.getSnapshot);
}
