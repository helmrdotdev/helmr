---
title: Deploy
description: Build, upload, verify and promote a Helmr project with the CLI.
---

# Deploy

A Deployment is one immutable build of your project in one Environment. This
guide builds and promotes one, separates build from deploy, and promotes or
rolls back an existing Deployment. For how Deployments, Environments and
promotion relate, see [Projects](/docs/concepts/projects).

You need the CLI configured (see [Quickstart](/docs/quickstart)), Docker with
Buildx, and Node.js 22 or newer.

## Deploy a project

From a directory containing `helmr.config.ts`, `package.json` and your Agent
source, run:

```sh
helmr deploy . --project demo --env production
```

With a saved login, `--project` and `--env` are required. With an Environment API
key, omit them; the key already selects the Environment.

The CLI:

1. evaluates `helmr.config.ts` on your machine, so install the packages it
   imports first;
2. builds the project in Helmr's digest-pinned Linux builder through Docker:
   installs dependencies, compiles your definitions and builds each Computer
   image;
3. uploads the content-addressed result, skipping objects the control plane
   already has;
4. asks the control plane to verify and finalize the Deployment;
5. promotes it, making it the Environment's current Deployment.

Progress goes to stderr and the result to stdout; `--json` writes progress as JSON
lines. Dependency installation and lifecycle scripts run only inside the builder,
never on the control plane or a worker. The control plane verifies the uploaded
build; it does not rebuild it.

## Dependencies and the build environment

Helmr follows the project's `packageManager` field and lockfile. npm, pnpm, Bun
and Yarn all work, and `build.installCommand` replaces the inferred install
command. A frozen lockfile gives reproducible builds.

The builder is a Debian environment with a native toolchain, so ordinary
lifecycle scripts and native modules work. When dependencies need more
operating-system packages, add them with `build.builder`. To give the install
step a private registry token, list the name of an environment variable in
`build.secrets`; it is mounted at `/run/secrets/NAME` during installation only.
See [Configuration](/docs/reference/config).

Do not copy credentials into source or build outputs. Credentials that Agents and
Computers use at runtime belong in [Secrets](/docs/guides/manage-secrets).

## Build and deploy separately

```sh
helmr build . --output .helmr/deployment-bundle
helmr deploy --bundle .helmr/deployment-bundle --project demo --env staging
```

`helmr build` writes the verified bundle locally (default
`.helmr/deployment-bundle`). `helmr deploy --bundle` uploads that bundle through
the same verification path. This lets CI build once and deploy the same bundle
to several Environments.

## Deploy without promoting

```sh
helmr deploy . --project demo --env production --skip-promotion
```

The Deployment is finalized but does not become current. Inspect it, then promote
it when ready:

```sh
helmr deployment list --project demo --env production
helmr deployment get DEPLOYMENT_ID --project demo --env production --json
helmr deployment promote DEPLOYMENT_ID --project demo --env production
```

`deployment promote` also rolls back: promote an earlier Deployment from the list
to make it current again.

## What promotion changes

- New Sessions and new Computers use the current Deployment.
- Existing Sessions stay on the Deployment they started with, including when you
  add Turns to them or start the Agent again with the same session key.
- Existing Computers keep their disk; see
  [Prepare a Computer](/docs/guides/prepare-a-computer#roll-out-a-change).
- Cron triggers declared in the new Deployment replace the previous ones; see
  [Triggers](/docs/concepts/triggers).

If a deploy finalization fails with an uncertain result, rerun it with the same
`--idempotency-key`. Without one, the CLI generates a new key for each run.
