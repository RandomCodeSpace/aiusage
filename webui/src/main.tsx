import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { App } from './App';
import { createApiClient } from './api/client';
import './styles.css';

/**
 * Server state is nearly all of this page's state: every view is data fetched
 * from the daemon. The live channel invalidates; refetch-on-focus and
 * background polling would only manufacture the thundering herd the
 * WebSocket design exists to avoid.
 */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      refetchOnWindowFocus: false,
      retry: 2,
      gcTime: 5 * 60_000,
    },
  },
});

const container = document.getElementById('root');
if (!container) throw new Error('index.html is missing #root');
const root = createRoot(container);

/**
 * The API client is resolved before the first render because building it can
 * be asynchronous - the mock lives behind a dynamic import. Doing it here
 * keeps App synchronous and gives the tree one client for its whole life, so
 * no query is ever issued against a client a later render replaces.
 */
async function boot(): Promise<void> {
  const client = await createApiClient();
  root.render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <App client={client} />
      </QueryClientProvider>
    </StrictMode>,
  );
}

void boot().catch((error: unknown) => {
  // The dynamic import is the only thing here that can fail, and a chunk that
  // 404s would otherwise leave a white page indistinguishable from a hung
  // daemon. Say so instead.
  console.error('aiusage: boot failed', error);
  root.render(<p>aiusage failed to start. See the browser console.</p>);
});
