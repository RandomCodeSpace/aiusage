# aiusage web UI

The browser surface over the usage ledger: a zoomable canvas trace scene, fed
by the daemon's read-only HTTP API. React 19 + TypeScript, bundled by Vite.

## Commands

Run everything from this directory.

```sh
npm ci          # install exactly what the lockfile pins
npm run dev     # dev server on 127.0.0.1:5173, API proxied to the dev daemon
npm run typecheck
npm run lint
npm run build   # typecheck, then bundle
npm run preview # serve a built bundle locally
```

`npm run build` is `tsc --noEmit && vite build`, so a type error fails the
build before the bundler runs. CI runs typecheck and lint separately first
anyway, because a named failure beats a compound one.

## Output

`npm run build` writes to `../internal/web/dist`, outside this directory. That
path is the Go embed root: `internal/web/embed_webui.go` does
`//go:embed all:dist` behind the `webui` build tag, so a tagged binary carries
the bundle and an untagged one does not.

**The build output is never committed.** It is gitignored at the repository
root (`/internal/web/dist/`), produced fresh by the release workflow, and
planted as a placeholder file by CI jobs that only need the embed to compile.
A stale committed bundle would ship silently, which is exactly the failure a
generated directory in version control always produces.

## API mode

The bundle talks to the daemon or to a built-in mock. The default is
structural rather than configured:

| Build            | Default |
| ---------------- | ------- |
| production build | `live`  |
| dev server       | `mock`  |

`VITE_API_MODE` overrides in both directions and is the only way to cross the
grain - `VITE_API_MODE=mock npm run build` pins a production bundle to the
generator, `VITE_API_MODE=live npm run dev` points the dev server at the
proxied daemon. See `src/api/client.ts`.

The mock is loaded through a dynamic `import()`, so a bundle that never
selects it does not execute it, and its generated ledger sits in a lazy chunk
the live path never fetches.

## The two TypeScript packages

`package.json` installs TypeScript twice, under two names. This is deliberate,
it is load-bearing, and neither entry is removable:

```jsonc
"@typescript/native": "npm:typescript@7.0.2",          // the compiler
"typescript":         "npm:@typescript/typescript6@6.0.2" // the library API
```

- **`@typescript/native`** is the real `typescript` package at 7.x, the native
  (Go) compiler. It owns the `tsc` bin, so `node_modules/.bin/tsc` - and
  therefore `npm run typecheck` and `npm run build` - is TypeScript 7. It is
  aliased away from the name `typescript` precisely so it does not become the
  package other tools resolve.

- **`typescript`** is `@typescript/typescript6`, Microsoft's sanctioned
  side-by-side shim that carries the TypeScript 6 JavaScript API under the
  name `typescript`. It exists for tools that import the compiler as a
  library. Its own bin is `tsc6`, so it never competes for `tsc`.

The reason for the split is `typescript-eslint`, which declares
`"typescript": ">=4.8.4 <6.1.0"` as a peer dependency and hard-rejects 7.x. It
loads the compiler as a library - program, parser, type checker - and the 7.x
package does not offer that surface: its main export is the version string,
with the compiler API published separately under `./unstable/*`. Installing
only 7.x breaks `npm run lint`; installing only 6.x gives up the native
compiler.

So: delete neither. `typescript` looks unused because nothing in `src/`
imports it - it is resolved by name, at runtime, by `typescript-eslint`.
Removing it as dead weight breaks lint; renaming `@typescript/native` back to
`typescript` breaks it the other way, by handing the peer resolution to 7.x.

Bump the two together, and keep the alias targets right: `@typescript/native`
tracks `typescript@7`, `typescript` tracks `@typescript/typescript6@6`.

## Layout

```
src/api/       the wire contract, the HTTP client, the mock, React Query hooks
src/scene/     the canvas trace scene: viewport, frame store, draw, folding
src/charts/    the @tanstack/charts seam - the only file allowed to import it
src/components/  chrome around the scene: strip, readout, inspector, status bar
src/hooks/     clock, reduced motion, settled range, scene frame subscription
```

`src/api/contract.ts` is one half of a contract whose other half is
`internal/web/wire.go`. Field names are the JSON wire names and the two are
kept in step field for field; change one and change the other.

`@tanstack/charts` is pre-alpha and has historically broken through import
specifiers and option renames rather than through props, so a props-only
wrapper is not a sufficient seam. An ESLint rule restricts every import of it
to `src/charts/seam.tsx`.
