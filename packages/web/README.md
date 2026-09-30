# Web

Public marketing site and documentation for `helmr.dev`.

## Local development

```sh
bun run --cwd packages/web dev
```

The site is static by default and builds to `packages/web/dist`.

## Build and deploy

Run from the repository root in the pinned development environment:

```sh
nix develop
bun install --frozen-lockfile --ignore-scripts
bun run check:web
```

`build:web` runs Astro and its output checks, then packages the static files
with the Cloudflare Vite plugin. The deployable artifact lives in
`packages/web/.cloudflare/output/v0/`. `check:web` also validates that artifact with a
deployment dry run, without credentials or deployment API requests. CLI telemetry
is disabled for the dry run. Source CI runs this check.

The web package owns its Cloudflare dependencies, `cloudflare.config.ts`, and
`vite.config.ts`. The Cloudflare configuration sets the Worker name, domain, and
asset serving policy. Vite packages Astro's output without adding a Worker script.
The root scripts delegate to this package so both build stages and deployment run
from `packages/web`. Its existing Astro type check also checks both configuration
files.
The Cloudflare CLI runs on Node.js; Bun manages dependencies and runs the scripts.
The CLI and Vite plugin use pinned beta releases, so update and validate them
together.

To publish the site, run `bun run deploy:web`. It builds and deploys the
same artifact with `cf deploy --prebuilt --mode production`. Authenticate using
`cf auth login` for local use, or `CLOUDFLARE_API_TOKEN` and
`CLOUDFLARE_ACCOUNT_ID` for automation. Cloudflare CLI credentials are separate
from Wrangler credentials.
