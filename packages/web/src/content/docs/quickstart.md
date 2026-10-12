---
title: Quickstart
description: Install the CLI, deploy a starter Agent, and run its first Session.
sidebarLabel: Quickstart
---

# Quickstart

This page takes a new TypeScript project from source to a completed Turn: you
install the CLI, connect it to your Helmr control plane, deploy a small Agent,
start it, watch the Session, send a follow-up and close it.

You need:

- A Helmr control plane with at least one active worker. See
  [Self-hosting](/docs/self-hosting/overview).
- Docker with Buildx. `helmr deploy` builds your project in Helmr's Linux
  builder through Docker.
- Node.js 22 or newer and a package manager. The starter project uses Bun.

## Install the CLI

```sh
curl -fsSL https://helmr.dev/install | bash
```

The installer selects the latest stable release for your OS and architecture.
To install a specific release, pass its tag:

```sh
curl -fsSL https://helmr.dev/install | bash -s -- --version "$HELMR_RELEASE_TAG"
```

Nix users can install the package instead:

```sh
nix profile install github:helmrdotdev/helmr#helmr
```

## Configure the CLI

The CLI talks to one control plane. Sign in with your control plane's origin:

```sh
helmr login https://helmr.example.com
```

`helmr login` prints a verification URL and code, opens your browser, and saves
the login for that origin once you approve it. Pass `--no-browser` to only print
the URL. Later commands use the saved login. You can also set the origin once
instead of passing it:

```sh
export HELMR_API_URL=https://helmr.example.com
helmr login
```

Check the active credentials:

```sh
helmr whoami
```

With a saved login, commands that act on an Environment take `--project` and
`--env` (short forms `-p` and `-e`), which accept the public Project and Environment IDs.

For automation, set `HELMR_API_KEY` to an
[Environment API key](/docs/reference/api/authentication) and `HELMR_API_URL` to
your control plane's origin. An API key already belongs to one Project
Environment, so commands reject `--project` and `--env` when it is set. Project
and Environment management (`helmr project`, `helmr env`) requires a saved
login. The origin must use HTTPS except on a loopback address.

The rest of the documentation assumes the CLI is configured as described here.

## Create a Project

```sh
helmr project create hello --name "Hello Helmr" \
  --history-retention-mode duration --history-retention-seconds 2592000
```

A new Project has two Environments, `staging` and `production`. This page uses
`staging`. The retention flags choose how long Session history is kept after a
Session ends; see [Projects](/docs/concepts/projects).

## Create the starter project

```sh
helmr init --dir ./hello-helmr
cd ./hello-helmr
bun install
```

`helmr init` creates `helmr.config.ts`, `.helmrignore`, `package.json`,
`tsconfig.json` and a starter Agent in `agents/hello.ts`. Helmr discovers
exported definitions in the directories that `helmr.config.ts` lists, `agents`
by default.

Replace `agents/hello.ts` with an Agent that uses its input:

```ts
import { agent, template, image } from "@helmr/sdk"
import { writeFile } from "node:fs/promises"

export const helloComputer = template({
  id: "hello",
  image: image("hello").from("node:24-bookworm-slim").workdir("/app"),
  resources: { cpu: 1, memory: "1GiB" },
})

export const hello = agent({
  id: "hello",
  template: helloComputer,
  maxTurnDuration: "5m",
  async turn(turn) {
    const name = turn.input.map(part => part.text).join("").trim()
    if (!name) throw new Error("Send a name")
    const greeting = `Hello ${name}`
    await turn.output.write("Writing your greeting")
    await writeFile("/workspace/greeting.txt", `${greeting}\n`, { flush: true })
    await turn.respond(greeting)
    return { greeting, path: "/workspace/greeting.txt" }
  },
})
```

The `template()` definition is the Linux environment the Agent runs in. The
`turn` handler runs once per input: it writes progress output, writes a file on
the Computer's disk, stages a response for people, and returns a JSON result for
programs. See [Agents](/docs/concepts/agents) and
[Computers](/docs/concepts/computers).

## Deploy

```sh
helmr deploy . --project hello --env staging
```

The CLI builds the project in Helmr's builder, uploads the result, and makes the
new Deployment current in `staging`. See [Deploy](/docs/guides/deploy).

## Start the Agent

```sh
helmr session create hello --project hello --env staging \
  --text "Ada" --idempotency-key quickstart:hello:1
```

The command prints a `session_id` and a `turn_id`. Helmr creates a Computer from
the Agent's definition and runs the first Turn on it. The receipt means the
input was accepted, not that the Turn has finished. If the command fails with an
uncertain result, run it again with the same idempotency key and input; it
returns the same Session and Turn.

## Watch the Session

Use the IDs from the receipt:

```sh
helmr session events SESSION_ID --project hello --env staging
helmr session turn wait TURN_ID --session SESSION_ID --timeout 10m \
  --project hello --env staging
```

`session events` prints the Session's event records, including the Turn's
progress output. `session turn wait` waits until the Turn settles or the timeout
passes. A timeout ends only the wait, not the Turn. A `completed` Turn shows the
returned result and the staged response, similar to:

```text
status: settled
turn_status: completed
result: {"greeting":"Hello Ada","path":"/workspace/greeting.txt"}
response: [{"type":"text","text":"Hello Ada"}]
```

## Send a follow-up

```sh
helmr session enqueue SESSION_ID --project hello --env staging \
  --text "Grace" --idempotency-key quickstart:hello:2
```

The follow-up becomes the Session's next Turn and runs on the same Computer, so
it overwrites the greeting file. Wait for it with the new `turn_id`.

## Close the Session

```sh
helmr session close SESSION_ID --project hello --env staging
```

Close finishes work that was already accepted and rejects new input. The
Computer remains until it is deleted, explicitly or after it has been unused
for its retention period.

## Next steps

- [Start and observe a Session](/docs/guides/start-and-observe-a-session) from
  the CLI or the TypeScript SDK.
- [Build a durable Agent](/docs/guides/build-a-durable-agent) that keeps state
  across Turns.
- [How Helmr works](/docs/concepts/how-helmr-works).
