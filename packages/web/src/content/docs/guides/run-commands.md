---
title: Run commands
description: Create or find a Computer, run commands on it, and read their output.
---

# Run commands

You can run commands on a Computer directly, without an Agent Session: to set up
a repository before starting work, inspect an Agent's files, or run a job. This
guide creates or finds a Computer, runs a command and reads its output. For what
a Computer is, see [Computers](/docs/concepts/computers).

The examples use a Template definition with the ID `repo` deployed to the `demo`
Project's `staging` Environment, with the CLI configured (see
[Quickstart](/docs/quickstart)).

## Create a Computer

```sh
helmr computer create repo --project demo --env staging \
  --key repo:acme/app \
  --idempotency-key computer:acme/app
```

The command prints the new Computer's ID. `repo` is the ID from
`template({ id: "repo" })` in the current Deployment. `--key` is an optional,
immutable name you can look the Computer up by later. `--idempotency-key` makes
retries safe: repeating the command with the same key returns the same Computer.
To bind Secrets at creation, pass `--secrets-file`; see
[Manage secrets](/docs/guides/manage-secrets).

## Or use an Agent's Computer

A Session started without `--computer` gets its own Computer. Find its ID with:

```sh
helmr session get SESSION_ID --project demo --env staging
```

The output includes `computer_id`.

## Find a Computer

```sh
helmr computer get --key repo:acme/app --project demo --env staging
helmr computer get --id COMPUTER_ID --project demo --env staging --json
```

Pass exactly one of `--id` or `--key`. The JSON form includes `status`
(`available`, `deleting`, `deleted`), `residency` (for example `running` or
`cold` when hibernated) and, for an `unavailable` Computer, an `error`.

## Run a command

```sh
helmr computer exec --key repo:acme/app --project demo --env staging \
  --idempotency-key status:1 -- git -C /workspace/app status --short
```

Everything after `--` is the command and its arguments; it runs without a shell.
The command prints the command ID once Helmr has accepted the command, which can
be before a hibernated Computer has resumed. `--idempotency-key` is required.

| Flag | Meaning |
| --- | --- |
| `--cwd PATH` | Working directory. Defaults to `/workspace`. |
| `--set-env NAME=VALUE` | Add an environment variable. Repeatable. |
| `--stdin PATH` | Send a file's bytes on standard input. |
| `--timeout DURATION` | Execution limit. Defaults to `5m`, at most `15m`. |

Commands can change the Computer's disk; changes persist for later commands and
Sessions on the same Computer. Do not pass Secret values in arguments or
`--set-env`; bind them to the Computer instead.

## Wait for the outcome and read output

```sh
helmr command wait COMMAND_ID --timeout 10m --project demo --env staging
helmr command logs COMMAND_ID --follow --project demo --env staging
```

`command wait` prints `status: settled` and the outcome `kind` (`exited`,
`cancelled`, `timed_out` or `system_failed`), or `status: timeout` if the wait
ended first. A timeout ends only the wait. Use `helmr command get COMMAND_ID --json`
for the exit code.

`command logs` writes the command's stdout and stderr to the matching streams.
Without `--follow` it reads one page and prints `output_state` (`open`, `closed`
or `unavailable`) and a `next_cursor` to pass as `--cursor`. With `--follow` it
keeps reading until the output closes, and fails if the complete output cannot be
established. Missing ranges are reported as gaps, not skipped silently.

To stop a command, run `helmr command cancel COMMAND_ID`. The receipt confirms the
request; read the outcome with `command get` or `command wait`.

## Do the same with the SDK

```ts
import { HelmrClient } from "@helmr/sdk"

const client = new HelmrClient({
  url: process.env.HELMR_API_URL!,
  apiKey: process.env.HELMR_API_KEY!,
})

const computer = await client.computers.create({
  template: "repo",
  key: "repo:acme/app",
  idempotencyKey: "computer:acme/app",
})

const command = await computer.exec({
  command: ["git", "-C", "/workspace/app", "status", "--short"],
  timeout: "5m",
  idempotencyKey: "status:1",
})

for await (const record of command.logs.stream()) {
  if (record.kind === "output") {
    const out = record.stream === "stdout" ? process.stdout : process.stderr
    out.write(record.content)
  } else {
    console.warn(`Missing ${record.stream} output: ${record.fromSequence}-${record.throughSequence}`)
  }
}

const outcome = await command.wait()
if (outcome.kind === "exited") console.log("exit code", outcome.exitCode)
```

To reach an existing Computer, use `client.computers.ref(id)`, or find it by key
with `client.computers.list({ key })`. Reconnect to a command with
`client.commands.ref(commandId)`. `logs.stream` yields byte chunks, not lines;
store a record's `cursor` and pass it as `logs.stream({ after })` to resume. See
the [Computer reference](/docs/reference/sdk/computer).

## Delete the Computer

```sh
helmr computer delete --key repo:acme/app --project demo --env staging
```

A Computer is also deleted automatically after it has been unused for its
retention period, seven days unless the definition sets `deleteAfterUnused`.
