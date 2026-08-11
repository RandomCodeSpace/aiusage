import { useEffect, useRef, useState } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import type { QueryClient } from '@tanstack/react-query';
import type { ApiClient } from './client';
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

/** One place that knows how a query is addressed, so invalidation is exact. */
export const queryKeys = {
  meta: () => ['meta'] as const,
  summary: (query: SummaryQuery) => ['summary', query] as const,
  facets: (query: RangeQuery) => ['facets', query] as const,
  events: (query: EventsQuery) => ['events', query] as const,
};

/** How often /api/meta is re-read when no live frame has arrived. */
const META_POLL_MS = 60_000;

export function useMeta(client: ApiClient) {
  return useQuery<MetaResponse>({
    queryKey: queryKeys.meta(),
    queryFn: ({ signal }) => client.meta(signal),
    refetchInterval: META_POLL_MS,
    staleTime: 5_000,
  });
}

export function useSummary(client: ApiClient, query: SummaryQuery, enabled = true) {
  return useQuery<SummaryResponse>({
    queryKey: queryKeys.summary(query),
    queryFn: ({ signal }) => client.summary(query, signal),
    enabled,
    // The scene pans continuously; keeping the previous buckets on screen is
    // the difference between a live surface and a flickering one.
    placeholderData: (previous) => previous,
    staleTime: 5_000,
  });
}

export function useFacets(client: ApiClient, query: RangeQuery, enabled = true) {
  return useQuery<FacetsResponse>({
    queryKey: queryKeys.facets(query),
    queryFn: ({ signal }) => client.facets(query, signal),
    enabled,
    placeholderData: (previous) => previous,
    staleTime: 30_000,
  });
}

export function useEvents(client: ApiClient, query: EventsQuery, enabled = true) {
  return useQuery<EventsResponse>({
    queryKey: queryKeys.events(query),
    queryFn: ({ signal }) => client.events(query, signal),
    enabled,
    placeholderData: (previous) => previous,
    staleTime: 5_000,
  });
}

export interface LiveState {
  connected: boolean;
  frame: LiveFrame | null;
  /** Increments once per frame; drives the one-shot pulse animations. */
  cycles: number;
}

function invalidateData(queryClient: QueryClient): void {
  // meta is deliberately included: contract_version and the daemon gauges
  // move on the same cadence, and it is one cheap request.
  for (const key of ['meta', 'summary', 'facets', 'events']) {
    void queryClient.invalidateQueries({ queryKey: [key] });
  }
}

/**
 * Subscribes to the live channel and turns every frame into an invalidation.
 * The frame carries no data by design - the client refetches only the view it
 * is on.
 */
export function useLive(client: ApiClient): LiveState {
  const queryClient = useQueryClient();
  const [state, setState] = useState<LiveState>({ connected: false, frame: null, cycles: 0 });

  useEffect(() => {
    const unsubscribe = client.subscribe(
      (frame) => {
        setState((previous) => ({
          connected: true,
          frame,
          cycles: previous.cycles + 1,
        }));
        invalidateData(queryClient);
      },
      (connected) => {
        setState((previous) => ({ ...previous, connected }));
      },
    );
    return unsubscribe;
  }, [client, queryClient]);

  return state;
}

/**
 * A long-lived tab must never decode a newer wire with older code. The boot
 * version is captured once; any later disagreement reloads the page.
 */
export function useContractGuard(meta: MetaResponse | undefined): void {
  const boot = useRef<number | null>(null);
  useEffect(() => {
    if (!meta) return;
    if (boot.current === null) {
      boot.current = meta.contract_version;
      return;
    }
    if (boot.current !== meta.contract_version) window.location.reload();
  }, [meta]);
}
