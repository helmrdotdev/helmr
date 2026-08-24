---
title: Deploy a project
description: Build, upload, verify, and promote a Helmr project with the CLI.
---

# Deploy a project

From a directory containing `helmr.config.ts`, `package.json`, and declaration
source, run:

```sh
helmr deploy . --project agents --env production
```

Saved-login commands require `--project` and `--env`. An environment API key is
already scoped and rejects those flags.

The CLI packages the selected source, invokes the official digest-pinned Linux
builder, uploads the resulting content-addressed bundle, asks the Control Plane
to verify it, and promotes the finalized Deployment. Build dependencies and
lifecycle scripts run only inside the isolated builder, never in the Control
Plane or execution Worker.

Helmr respects the project's `packageManager` and lockfile when present. npm,
pnpm, Bun, Yarn, and a custom `--install-command` are producer choices rather
than server acceptance criteria. A frozen lockfile is the recommended
reproducible path, but the durable Deployment identity is the completed bundle
and its artifact digests—not the package-manager name or version.

To separate build and deploy:

```sh
helmr build . --output .helmr/deployment-bundle
helmr deploy --bundle .helmr/deployment-bundle --project agents --env staging
```

Use `--skip-promotion` to finalize without making the Deployment current. The
Control Plane validates the bundle closure, sizes, formats, architecture, and
runtime contract; it does not rebuild the project.

Do not copy secrets into source or build outputs. Pass private dependency
credentials through the local or CI build environment, and put runtime
credentials in Helmr Secrets.
