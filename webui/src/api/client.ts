import type {
  EventsQuery,
  EventsResponse,
  FacetsResponse,
  LiveFrame,
  MetaResponse,
  RangeQuery,
  SummaryQuery,
  SummaryResponse,
} from './contract';
import { createHttpClient } from './http';

/**
 * Every byte the page reads from the daemon goes through this interface.
 * Swapping the mock for the real server touches this module and nothing else.
 */
export interface ApiClient {
  meta(signal?: AbortSignal): Promise<MetaResponse>;
  summary(query: SummaryQuery, signal?: AbortSignal): Promise<SummaryResponse>;
  facets(query: RangeQuery, signal?: AbortSignal): Promise<FacetsResponse>;
  events(query: EventsQuery, signal?: AbortSignal): Promise<EventsResponse>;
  /** Subscribes to the live channel. Returns an unsubscribe function. */
  subscribe(onFrame: (frame: LiveFrame) => void, onState: (up: boolean) => void): () => void;
}

export type ApiMode = 'mock' | 'live';

/**
 * Which half of the contract this bundle talks to.
 *
 * The default is structural, not configured: a production build talks to the
 * daemon, a dev server talks to the mock. Nothing sets VITE_API_MODE in the
 * release path, so a default of mock meant every shipped bundle rendered
 * generated noise and never issued a request - a build flag nobody passes is
 * not a default, it is a trap.
 *
 * VITE_API_MODE overrides in both directions and is the only way to cross the
 * grain: mock pins a production build to the generator (screenshots, offline
 * review), live points the dev server at the proxied daemon.
 */
function resolveMode(): ApiMode {
  if (import.meta.env.VITE_API_MODE === 'live') return 'live';
  if (import.meta.env.VITE_API_MODE === 'mock') return 'mock';
  return import.meta.env.PROD ? 'live' : 'mock';
}

export const API_MODE: ApiMode = resolveMode();

/**
 * Builds the client for a mode. It is async because the mock is loaded through
 * a dynamic import: the generator is a few kilobytes of fake ledger that a
 * production bundle must never carry, and an import() is what keeps it in a
 * chunk of its own that live builds never fetch.
 */
export async function createApiClient(mode: ApiMode = API_MODE): Promise<ApiClient> {
  if (mode === 'live') return createHttpClient();
  const { createMockClient } = await import('./mock');
  return createMockClient();
}
