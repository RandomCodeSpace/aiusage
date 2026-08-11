import { useCallback, useMemo, useRef, useState } from 'react';
import type { ApiClient } from './api/client';
import { EVENTS_PAGE_LIMIT, microUsdToUsd } from './api/contract';
import { useContractGuard, useEvents, useFacets, useLive, useMeta, useSummary } from './api/queries';
import { CommandStrip } from './components/CommandStrip';
import { Inspector } from './components/Inspector';
import { NowLine } from './components/NowLine';
import { Readout } from './components/Readout';
import { StatusBar } from './components/StatusBar';
import { Tooltip } from './components/Tooltip';
import { useNow } from './hooks/useNow';
import { useReducedMotion } from './hooks/useReducedMotion';
import { useSettledRange } from './hooks/useSettledRange';
import { FrameStore } from './scene/frame';
import { foldSessions } from './scene/sessions';
import type { TraceSession } from './scene/sessions';
import { TraceScene } from './scene/TraceScene';
import type { SceneHandle } from './scene/TraceScene';

/** Sessions are refolded on this grain, not on every clock tick. */
const LIVE_GRAIN_MS = 30_000;

export interface AppProps {
  /**
   * Built once at boot (main.tsx) and stable for the life of the tree. It is
   * an effect dependency of the live subscription, so a client whose identity
   * changed per render would tear down and reopen the WebSocket every render.
   */
  client: ApiClient;
}

export function App({ client }: AppProps) {
  const frames = useMemo(() => new FrameStore(), []);
  const sceneRef = useRef<SceneHandle | null>(null);
  const reducedMotion = useReducedMotion();

  const metaQuery = useMeta(client);
  useContractGuard(metaQuery.data);
  const live = useLive(client);
  const nowMs = useNow(metaQuery.data);

  const range = useSettledRange(frames);
  const enabled = range !== null;
  const rangeQuery = range ?? { since: 0, until: 0 };

  const eventsQuery = useEvents(
    client,
    { since: rangeQuery.since, until: rangeQuery.until, limit: EVENTS_PAGE_LIMIT },
    enabled,
  );
  const facetsQuery = useFacets(client, rangeQuery, enabled);

  const truncated = eventsQuery.data?.truncated ?? false;
  const eventsTotal = eventsQuery.data?.total ?? null;
  // Only worth asking when the capped event page is known to understate: the
  // readout is otherwise summing exactly what it draws.
  const summaryQuery = useSummary(
    client,
    { since: rangeQuery.since, until: rangeQuery.until, group_by: ['tool'] },
    enabled && truncated,
  );

  const rows = eventsQuery.data?.rows;
  const liveGrain = Math.floor(nowMs / LIVE_GRAIN_MS);
  const sessions = useMemo<TraceSession[]>(
    () => foldSessions(rows ?? [], liveGrain * LIVE_GRAIN_MS),
    [rows, liveGrain],
  );

  // Lanes come from the facets of the visible range, heaviest first, so the
  // scene shows the tools that are actually there instead of every tool id
  // the ledger has ever held.
  const tools = useMemo<string[]>(() => {
    const fromFacets = facetsQuery.data?.tools.map((facet) => facet.value) ?? [];
    if (fromFacets.length > 0) return fromFacets;
    return metaQuery.data?.tools ?? [];
  }, [facetsQuery.data, metaQuery.data]);

  const rangeTotal = useMemo(() => {
    const totals = summaryQuery.data?.totals;
    if (!truncated || !totals) return null;
    return {
      tokens: totals.total,
      // The events endpoint counts the range itself, uncapped, and that count
      // is the one the truncation flag is measured against - so it is the
      // honest denominator for "showing 1000 of what". The summary's own
      // count is the fallback for a response that predates the field.
      events: eventsTotal ?? totals.events,
      costUSD: microUsdToUsd(totals.cost_micro_usd),
    };
  }, [summaryQuery.data, truncated, eventsTotal]);

  const [follow, setFollow] = useState(true);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selected = useMemo(
    () => sessions.find((session) => session.id === selectedId) ?? null,
    [sessions, selectedId],
  );

  const onSelect = useCallback((session: TraceSession | null) => {
    setSelectedId(session?.id ?? null);
  }, []);

  const onToggleFollow = useCallback(() => {
    sceneRef.current?.toggleFollow();
  }, []);

  const onZoomToSession = useCallback((session: TraceSession) => {
    sceneRef.current?.zoomToSession(session);
  }, []);

  const onCloseInspector = useCallback(() => {
    setSelectedId(null);
  }, []);

  return (
    <>
      <CommandStrip
        frames={frames}
        follow={follow}
        cycles={live.cycles}
        reducedMotion={reducedMotion}
        onToggleFollow={onToggleFollow}
      />

      <TraceScene
        sessions={sessions}
        tools={tools}
        nowMs={nowMs}
        selectedId={selectedId}
        reducedMotion={reducedMotion}
        frames={frames}
        onSelect={onSelect}
        onFollowChange={setFollow}
        handleRef={sceneRef}
      />
      <NowLine frames={frames} />

      <Readout frames={frames} tools={tools} cycles={live.cycles} rangeTotal={rangeTotal} />
      <Inspector session={selected} onClose={onCloseInspector} onZoom={onZoomToSession} />
      <Tooltip frames={frames} />

      <StatusBar meta={metaQuery.data} live={live} truncated={truncated} />
    </>
  );
}
