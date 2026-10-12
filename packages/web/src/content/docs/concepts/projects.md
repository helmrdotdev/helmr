---
title: Projects
description: How Projects, Environments, Deployments and promotion decide which code runs.
---

# Projects

A Project groups one product or service inside an Organization. It contains one
or more Environments, and each Environment receives Deployments of the
Project's code.

A Project has a permanent, readable ID such as `agents`, returned as `project`
by the API. Each Environment has its own permanent ID within the Project,
returned as `environment`. The Console, CLI and API use these same values.
Display names can change without changing IDs or breaking references.
Creation trims and lowercases Project and Environment IDs. Use the resulting ID
returned by the API or copied from the Console verbatim; lookups are case-sensitive.

## Environments

An Environment is an independent scope within a Project. Each Environment has
its own:

- current Deployment;
- [Secrets](/docs/concepts/secrets);
- [Computers](/docs/concepts/computers), [Sessions](/docs/concepts/sessions)
  and [Turns](/docs/concepts/turns);
- [triggers](/docs/concepts/triggers);
- API keys.

Use separate Environments, such as `development` and `production`, when the
same source needs different promoted versions, credentials, history or access.

An API key belongs to exactly one Environment, so API-key requests need no
Project or Environment argument. A CLI user signed in with `helmr login`
selects the Project and Environment on each command with `--project` and
`--env`. See [Authentication](/docs/reference/api/authentication).

```sh
helmr project create agents --name "Agents" --history-retention-mode until_environment_deletion
helmr env create development --project agents --history-retention-mode until_environment_deletion
```

## Deployments

A Deployment is an immutable, content-addressed bundle of your Project's code.
The Helmr CLI builds it on your machine or in CI: it installs dependencies,
evaluates `helmr.config.ts`, compiles your Agents and Computers, builds the
Computer images and records every Agent, Template definition and trigger. The
control plane verifies and registers the finished bundle without rebuilding
it.

```sh
helmr deploy --project agents --env production
```

`helmr deploy` builds, uploads and promotes in one step. `helmr build` produces
a bundle that `helmr deploy --bundle` can upload later. See
[Deploy](/docs/guides/deploy).

## Promotion

Promotion makes one Deployment current in an Environment. New starts, trigger
fires and definition reads use the current Deployment unless a read names
another one explicitly.

`helmr deploy` promotes by default. With `--skip-promotion` it finalizes the
Deployment without making it current, so you can inspect it first and promote
it later:

```sh
helmr deploy --project agents --env production --skip-promotion
helmr deployment promote DEPLOYMENT --project agents --env production
```

Any Deployment registered in the Environment can be promoted, including an
earlier one, which is how you roll back.

## What promotion changes

Promotion changes what **new** work runs. It does not change work that already
exists:

- **Existing Sessions stay on their Deployment.** A Session is pinned to the
  Deployment that was current when it was created. Later Turns in that
  Session, including Turns added by starting with the same Session key, keep
  running the old code. To move a conversation to new code, start a new
  Session.
- **Existing Computers are not rewritten** by promotion.
- **Triggers are replaced.** Promotion reconciles the Environment's active
  cron triggers with the ones declared in the new Deployment. See
  [Triggers](/docs/concepts/triggers).
- **Computer refresh settings are replaced** with the ones declared in the new
  Deployment's Template definitions.

The Deployment itself is never modified.

## Related

- [Deploy](/docs/guides/deploy)
- [`helmr deploy`](/docs/reference/cli/deploy),
  [`helmr deployment`](/docs/reference/cli/deployment) and
  [`helmr project`](/docs/reference/cli/project)
- [Configuration](/docs/reference/config)
