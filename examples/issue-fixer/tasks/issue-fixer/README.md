# Issue fixer with retained native sessions

These Agents use the pinned Codex app-server and Claude Agent SDK versions in
`package.json`. Each Agent opens one native harness during Session setup and reuses
it across Turns. The wrappers in `codex.ts` and `claude.ts` connect the adapters to
the authored Agent lifecycle; the application owns provider configuration and
interpretation of answers.

- `codex-harness.ts` runs an app-server thread, delivers exact native steering,
  and converts native questions and one-time command/file approvals to Turn asks.
- `claude-harness.ts` retains a streaming query, serializes follow-ups, converts
  native questions/permissions to Turn asks, and rejects background work.
- `native-lifecycle.ts` registers setup and operation lifetimes with the managed
  Runtime. Native terminal text alone does not establish physical convergence.
- `native-content.ts` and `native-questions.ts` translate supported text/JSON and
  question controls. Secret questions and unsupported semantics fail visibly.

## Configure the Computer

Before use, edit `computer.ts` to prepare your fixed repository under
`/workspace/repository` and bind the required provider credentials by stable
Environment Secret ID. The checked-in image only supplies tools and a directory;
it does not fetch your repository or contain credentials. Use a disposable
repository and a separate Computer for each independent Session. Set
`ISSUE_FIXER_REPOSITORY` if you choose another fixed path. Input cannot select a
filesystem path or shell command.

Edit `checks.ts` to use that repository's deterministic validation command. The
sample runs `npm test -- --runInBand`. A native result is followed by that check;
only a successful handler return permits successful Turn settlement and publication
of the staged native response. Failed checks remain failed Turns, even if output
was already visible.

The default native credentials come from the configured Computer environment.
The application does not copy authentication from developer home directories.
Provider calls can incur ordinary provider charges. Tests use synthetic credentials
and local responses. Running the actual Agent requires your configured environment.

## Start and interact

```bash
helmr deploy PATH/TO/issue-fixer --project PROJECT --env ENVIRONMENT
helmr agent start codex-issue-fixer --input-json '[{"type":"text","text":"Fix the login regression"}]' --project PROJECT --env ENVIRONMENT --json
```

Use `claude-issue-fixer` for Claude. Initial input and native follow-ups are text-part
arrays such as `[{"type":"text","text":"Keep the existing public API"}]`.
Send follow-ups to the exact Turn; enqueue for a new Turn. Observe retained message
dispositions when native completion races delivery.

Setup obtains `createRuntimeMcpConnection()` from `@helmr/sdk/mcp` and supplies the
Session-bound endpoint and headers to the native harness. Runtime owns upstream
authority renewal. Do not log the descriptor or put its headers into model input.
Native tool permission policy remains separate from Helmr's runtime authority.
Unsupported native interactive request forms are rejected.

Questions use the ordinary exact-Turn ask list/get/respond workflow. They carry
question controls and authenticated responder attribution. This application maps
an approval answer to one displayed native operation; an answer never grants
Session-wide permission. Helmr validates answer authority and lifecycle while
the application interprets the answer. A late or cancelled answer cannot move to
the next Turn.

Slack is configured in Console and delivered through Helmr's conversation routes.
This example does not host a second Slack ingress, store Slack bot credentials, or
project its own status messages. Use the retained Helmr timeline for full output,
questions and lifecycle history.

## Retention and interruption

`conversation.ts` records native history identity under
`<repository>/.helmr/issue-fixer/<session>/<provider>`. Keep `.helmr/` out of commits
and preserve it with the Computer. Healthy continuation retains setup and native
process state. Exceptional reconstruction must use retained native history and
fail visibly when that history is unavailable; a reserved native ID alone is not
proof of persisted history. Pending native permission callbacks are not reconstructed.

Interrupt the Session and its owned descendants through the Session control API.
Inspect convergence and the returned hold, then resume the owning Session with the
exact authorized hold ID. Queued inputs remain; interrupted input is not replayed.
Runtime owns fencing and physical writer exclusion. Stopping local work does not
roll back an already performed external effect.

## Local verification

From the repository root, build the local SDK with
`scripts/build-npm-packages.sh`. Then, inside this example, run:

```bash
bun install --frozen-lockfile --ignore-scripts
bun run typecheck
bun run test
bun run test:native-probes
```

Run through the repository's Nix environment. The mock tests run in separate
processes so provider mocks cannot leak into other tests. Native probes bundle for
and execute under Node, matching Runtime's required Node APIs. They exercise
warm process identity, questions, application failure, interruption and Codex-only local MCP
connection initialization using real pinned provider processes with loopback
services. They do not call remote models.

These checks do not prove deployed admission, real provider inference, actual
Slack delivery, guest descendant containment, VM capture/restore, or cross-host
continuation. Those require the corresponding runtime qualification.
