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

/** Same origin in every deployment: the daemon serves the page and the API. */
const API_BASE = '/api';

const RECONNECT_MIN_MS = 500;
const RECONNECT_MAX_MS = 30_000;

class HttpError extends Error {
  readonly status: number;
  constructor(status: number, statusText: string, path: string) {
    super(`${path}: ${String(status)} ${statusText}`);
    this.name = 'HttpError';
    this.status = status;
  }
}

function appendList(params: URLSearchParams, name: string, values: string[] | undefined): void {
  for (const value of values ?? []) params.append(name, value);
}

function rangeParams(query: RangeQuery): URLSearchParams {
  const params = new URLSearchParams();
  params.set('since', String(query.since));
  params.set('until', String(query.until));
  appendList(params, 'tool', query.tools);
  appendList(params, 'model', query.models);
  appendList(params, 'project', query.projects);
  appendList(params, 'session', query.sessions);
  return params;
}

async function getJSON<T>(path: string, params?: URLSearchParams, signal?: AbortSignal): Promise<T> {
  const qs = params?.toString();
  const url = qs ? `${API_BASE}${path}?${qs}` : `${API_BASE}${path}`;
  const response = await fetch(url, {
    signal: signal ?? null,
    headers: { accept: 'application/json' },
    credentials: 'same-origin',
    cache: 'no-store',
  });
  if (!response.ok) throw new HttpError(response.status, response.statusText, path);
  return (await response.json()) as T;
}

function liveSocketURL(): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${protocol}//${window.location.host}${API_BASE}/ws`;
}

/**
 * Reconnect with backoff is mandatory, not defensive: the daemon re-executes
 * itself into a replaced binary and drops every connection without a close
 * frame.
 */
function subscribeLive(
  onFrame: (frame: LiveFrame) => void,
  onState: (up: boolean) => void,
): () => void {
  let socket: WebSocket | null = null;
  let timer: number | undefined;
  let delay = RECONNECT_MIN_MS;
  let closed = false;

  const connect = (): void => {
    if (closed) return;
    const next = new WebSocket(liveSocketURL());
    socket = next;

    next.addEventListener('open', () => {
      delay = RECONNECT_MIN_MS;
      onState(true);
    });
    next.addEventListener('message', (event: MessageEvent<unknown>) => {
      if (typeof event.data !== 'string') return;
      try {
        onFrame(JSON.parse(event.data) as LiveFrame);
      } catch {
        // A frame we cannot decode is a contract problem, not a reason to
        // tear down the socket; /api/meta polling is what catches that.
      }
    });
    const retry = (): void => {
      onState(false);
      if (closed) return;
      timer = window.setTimeout(connect, delay);
      delay = Math.min(RECONNECT_MAX_MS, delay * 2);
    };
    next.addEventListener('close', retry);
    next.addEventListener('error', () => {
      next.close();
    });
  };

  connect();

  return () => {
    closed = true;
    if (timer !== undefined) window.clearTimeout(timer);
    socket?.close();
  };
}

export function createHttpClient(): ApiClient {
  return {
    meta: (signal) => getJSON<MetaResponse>('/meta', undefined, signal),

    summary: (query: SummaryQuery, signal) => {
      const params = rangeParams(query);
      for (const dimension of query.group_by) params.append('group_by', dimension);
      return getJSON<SummaryResponse>('/summary', params, signal);
    },

    facets: (query: RangeQuery, signal) =>
      getJSON<FacetsResponse>('/facets', rangeParams(query), signal),

    events: (query: EventsQuery, signal) => {
      const params = rangeParams(query);
      if (query.cursor) params.set('cursor', query.cursor);
      if (query.limit !== undefined) params.set('limit', String(query.limit));
      return getJSON<EventsResponse>('/events', params, signal);
    },

    subscribe: subscribeLive,
  };
}
