import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

/**
 * The dev deployment is reverse-proxied by Caddy (/etc/caddy/aiusage.caddy):
 * everything that is not /api/* or /ws* reaches this server, over TLS on 443,
 * under a hostname that is not localhost. Vite has to be told both facts or
 * the browser opens its HMR socket against localhost:5173 and live reload
 * silently never connects.
 */
const DEV_HOST = 'aiusage-dev.randomcodespace.dev';

/** Dev daemon: API only, separate config and database from prod (37800). */
const DEV_DAEMON = 'http://127.0.0.1:37801';

export default defineConfig({
  plugins: [react()],
  build: {
    // Embedded into the release binary by internal/web (//go:embed all:dist),
    // gitignored, never committed. Vite refuses to clear a directory outside
    // its root unless emptyOutDir says so explicitly.
    outDir: '../internal/web/dist',
    emptyOutDir: true,
    // No CDN, no remote fonts, no runtime fetches beyond the daemon: the
    // runtime is offline and a strict CSP lands later.
    assetsInlineLimit: 4096,
    sourcemap: false,
  },
  server: {
    host: '127.0.0.1',
    port: 5173,
    strictPort: true,
    allowedHosts: [DEV_HOST],
    hmr: { host: DEV_HOST, clientPort: 443, protocol: 'wss' },
    proxy: {
      // ws:true matters for /api: the live channel is /api/ws.
      '/api': { target: DEV_DAEMON, ws: true },
      '/ws': { target: DEV_DAEMON, ws: true },
    },
  },
  preview: {
    host: '127.0.0.1',
    port: 5173,
    allowedHosts: [DEV_HOST],
  },
});
