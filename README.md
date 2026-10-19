# Helmr

**Build cloud agents that work your way.**

Your models, tools, and code. Helmr handles the infrastructure.

## What you can build

- **A teammate in Slack.** Mention it in a thread and it works on its own
  computer, asks the thread when it needs a decision, and remembers the
  conversation for follow-ups.
- **An issue fixer.** Point it at your repository, and it works an issue with
  Codex or Claude Code, runs your checks, and asks before running commands that
  need approval.
- **A scheduled job.** Run an agent on a cron schedule, such as a weekday
  morning report that opens a new thread in a Slack channel.

## What an agent looks like

A Pi coding agent on its own computer, using the Pi SDK directly:

```ts
import { agent, image, template } from "@helmr/sdk"
import { createAgentSession, SessionManager } from "@earendil-works/pi-coding-agent"

const computer = template({
  id: "assistant",
  image: image("assistant")
    .from("node:24-bookworm-slim")
    .run(["sh", "-c", "apt-get update && apt-get install -y git ripgrep"])
    .workdir("/workspace"),
  resources: { cpu: 2, memory: "4GiB" },
  secrets: [{
    secret: "ANTHROPIC_API_KEY",
    env: { name: "ANTHROPIC_API_KEY", mode: "protected", allowedOrigins: ["https://api.anthropic.com"] },
  }],
})

export const assistant = agent({
  id: "assistant",
  template: computer,
  async init({ session }) {
    const { session: pi } = await createAgentSession({
      cwd: "/workspace",
      sessionManager: SessionManager.continueRecent("/workspace", `/var/lib/helmr/pi/${session.id}`),
    })
    return pi
  },
  async turn(turn, { initResult: pi }) {
    turn.onCancel(() => pi.abort())
    await pi.prompt(turn.input.map(part => part.text).join(""))
    await turn.respond(pi.getLastAssistantText() ?? "")
    return null
  },
})
```

`init` starts Pi when the session's process starts, and each message becomes a
`turn`. If the process is ever rebuilt, Pi picks up its saved conversation from
the computer's disk. The computer, its tools and its secrets live next to the
agent code. `helmr init --template` has fuller starters for Pi, Claude Code,
Codex and OpenCode.

## Why Helmr

Running agents in the cloud usually means choosing between a hosted agent you
can't see into, or building hibernation, checkpoints and recovery yourself.
Helmr aims for the feel of setting up an environment and running a script: you
write it, you can see what it's doing, and Helmr handles the lifecycle.

**Keep the agent you already use.**
Pi, Claude Code, Codex and OpenCode run as themselves, with their own sessions,
tools and permission prompts. Helmr wraps one lifecycle around them (sessions,
turns, cancellation, resume) instead of asking you to rewrite your agent for a
new framework.

**The computer is code you can review.**
Image, packages, CPU and memory, and which secrets reach which origins live in
the same TypeScript file as the agent. Changes go through a pull request like
any other code, and every environment gets the same computer.

**Work survives without checkpoint code.**
A session can sit idle between messages and pick up where it left off. Files
save in the background; if a worker is lost, Helmr tells you what was lost and
continues from the last save when you resume. Completed turns are never run twice.

**You can see it and step in.**
Every session has an event history: what ran, what it is waiting on, what it
returned. When the agent needs a decision, it asks, and your team answers from
Slack, the Console or the CLI. You can also run a command on the computer
yourself.

**Keys can stay outside the computer.**
With protected secrets, the agent sees a placeholder. Helmr swaps in the real
key outside the VM, only in requests to the origins you allow.

**It runs in your AWS account.**
The control plane, workers and data stay in infrastructure you own.

## Get started

You need a Helmr control plane with at least one worker (see the
[self-hosting guide](https://helmr.dev/docs/self-hosting/overview)) and Docker
with Buildx.

Install the CLI, sign in and create a Pi agent:

```sh
curl -fsSL https://helmr.dev/install | bash
helmr login https://helmr.example.com
helmr init --template pi --dir my-agent
cd my-agent
bun install
```

Add the `ANTHROPIC_API_KEY` Secret for your environment in the Console, then
deploy:

```sh
helmr deploy --project PROJECT --env ENVIRONMENT
```

The [quickstart](https://helmr.dev/docs/quickstart) walks through the whole path,
from creating a project to running your first session.

## Build from source

Build the CLI with the pinned toolchain and generated platform entries:

```sh
nix build .#helmr
```

For local development, install workspace dependencies and use the Make targets:

```sh
nix develop -c bun install --frozen-lockfile --ignore-scripts
nix develop -c make build
```

`make build`, `make test` and `make lint` generate the JavaScript entries before
their Go consumers. Before running focused Go commands, run
`nix develop -c make platform-entries` and repeat it after TypeScript or dependency
changes. The four platform `.mjs` bundles are local build outputs, not source
files; a fresh checkout needs this preparation for direct Go builds.
Go-only `go install ...@version` is not a supported source installation path.

Nix acquires hash-verified dependencies separately from offline entry generation.
Dependency updates may require updating the dependency output hash in
`nix/packages/platform-entries.nix`; the esbuild API and native fetch versions
must agree with `package.json` and `bun.lock`. CI compares all four Nix-generated
entries with local generation byte-for-byte.

## Explore

- [Documentation](https://helmr.dev/docs)
- [How Helmr works](https://helmr.dev/docs/concepts/how-helmr-works)
- [TypeScript SDK](https://helmr.dev/docs/reference/sdk/overview)
- [REST API](https://helmr.dev/docs/reference/api/overview)

Early, active development. APIs and deployment details may change before a
stable release.

[Apache 2.0](LICENSE).
