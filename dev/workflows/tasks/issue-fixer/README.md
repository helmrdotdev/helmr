# Issue fixer with native human interaction

These editable Actors use the prerelease Session/Turn SDK with the provider versions
already pinned in `dev/workflows/package.json`: Codex app-server 0.133.0 and Claude
Agent SDK 0.3.149. They are application examples, not a provider adapter package.
No Helmr public SDK or lifecycle primitive changes are part of this sample.

- `codex.ts`: a native thread/turn, exact `turn/steer`, command/file approvals and
  `item/tool/requestUserInput`. Unsupported interactive methods fail closed. Secret
  questions require a private application channel and are rejected here.
- `claude.ts`: `canUseTool`, `AskUserQuestion`, and streaming-input follow-ups. Claude
  serializes those inputs after each native result; this sample does not describe
  them as Codex-style steering.
- `human.ts`: live request correlation and the fresh permission admission write.
  This map disappears with the process; it is not a Token or managed durable wait.
- `../../interfaces/issue-fixer-slack.ts`: verified Slack intake and a retained-event
  projection, independent of the chosen Actor/provider.

## Running in an isolated workspace

Set `ISSUE_FIXER_REPOSITORY` to a disposable, Session-owned repository checkout in
the Actor workspace. Different Sessions must not share its writable directory.
Install the pinned workflow dependencies, configure the chosen provider's ordinary
credentials on the runtime host, and edit `checks.ts` to use that repository's fixed
deterministic check command. The webhook cannot select a path or shell command.
Provider use can incur its normal usage charges; local tests below do not invoke a
model. The samples do not create commits, push changes or open pull requests.

Deploy these Actors using the existing workflow configuration. Start either Actor
with the ordinary `actors.start`/CLI flow and retain the resulting Session ID.
Each Helmr Turn creates a new native conversation; workspace files persist through
Helmr's Session lifecycle. Provider conversation resume/checkpointing is deliberately
not claimed by this sample.

Mount `acceptSlackEvent` in your HTTP application, supplying the original body
bytes, a 64 KiB body limit and a request-read timeout. Bind one configured Slack
app/team/channel and an explicit user allowlist to the Session reference. Subscribe
to `app_mention`. Signature validation is not user authorization; both checks apply.
The function waits at most 1.8 seconds for the Helmr request after reading the body,
then acknowledges only success. A timeout returns non-2xx so Slack retries the same
event ID/idempotency key. Monitor failed deliveries: Slack retries are finite; this
small ingress does not promise indefinite durable delivery. For stronger intake
availability, persist the verified event in the application's durable queue before
ACK and dispatch with that same key. Do not replay unknown effects after the
operation history retention window without reconciliation.

Commands after mentioning the bot:

```text
fix The login form loses its error message
fix Add a regression test for password reset
reply TURN_A {"type":"update_constraints","text":"Do not change the public API"}
reply TURN_A {"type":"answer","requestId":"REQUEST_ID","answers":{"QUESTION_KEY":["Keep existing behavior"]}}
reply TURN_A {"type":"approval","requestId":"REQUEST_ID","allow":true}
stop TURN_A
resume CURRENT_HOLD_ID
```

Use the question keys from `human_requested`: Codex uses question IDs; Claude uses
question text. Approvals apply only to the displayed operation, never a future or
Session-wide permission. Application reply payloads are freely designed; these
schemas are examples, not new Helmr message types.

Create a bot-owned Slack status message once and configure its channel/`ts` for
`projectSlackStatus`. Run one projector per Session/state file, calling it repeatedly.
It updates that fixed message and saves the event cursor only after success. A crash
between update and cursor persistence repeats an update instead of creating a second
message. Keep the state path dedicated to the same Session and destination. This
compact status projection retains eight event summaries; use the Helmr timeline for
full request bodies/history. A large request may be truncated on Slack: inspect its
full action in the timeline before replying. Use a restricted channel; tool inputs,
model output and replies can contain repository content. Never send secrets here.

A remote failure or `cursor_expired` must remain visible to the host; do not reset the
cursor or treat a failed update as delivered. Use a bot token/destination that remain
valid for long waits rather than a short-lived interaction response URL. This code
was not run against a real Slack workspace as part of local qualification.

## Lifecycle exercise

1. Enqueue issue A, then issue B. B must remain queued while A is running.
2. While A awaits a native question/approval, send a constraint or reply to **A's**
   exact Turn. Human waits do not occupy the serial Helmr message handler.
3. Stop A before replying. The live request is invalidated; a late reply cannot be
   redirected to B or grant an action. Stop is not an ordinary application failure.
4. Read the Session after convergence and resume with its **current** hold ID. The
   interrupt receipt's initial hold can already be stale. Recovery-required holds
   need operator reconciliation rather than ordinary resume.
5. B runs after resume. Provider stream completion alone does not complete it:
   deterministic checks run next, and only their success calls `turn.complete()`.
   A failed check calls `turn.fail()`.

The runtime owns writer exclusion, Workspace proof and the terminal stop state.
Stopping a local process does not undo remote effects. Native background/external
work requires application reconciliation before recovery. The small samples do not
qualify arbitrary native sockets or callback Promises for checkpoint/restore.

## Primitive design observations

`session.enqueue(input)` and `session.turn(id).send(message)` express different
intent here: new work versus interaction with an already identified operation.
Automatic `session.send` is useful when an input has the same meaning both idle and
active; its typed signature requires the intersection of input and message types.
It is not appropriate for a delayed approval or for an issue that must become a new
Turn. Replacing these with a mode flag would preserve the same distinctions while
making target/type constraints less explicit. This sample alone does not justify
removing or broadening automatic send.

The permission fence deserves separate scrutiny: `output.write` provides a fresh,
Turn-scoped admission ordering point, not a permission interpretation. The record
binds the exact live native operation and is awaited before allow. Failed or
ambiguous writes never grant. A separate permission primitive would need to provide
an additional guarantee beyond that fence; merely hiding provider-specific pending
request state would not do so. Native cancellation and native response formats remain
application responsibilities. Any proposed public primitive change needs Founder
review before implementation, even though prerelease compatibility is unnecessary.

## Local validation and remaining proof

```sh
nix develop .#default --command sh -c 'bun run --cwd dev/workflows typecheck && bun test dev/workflows/tests/issue-fixer.test.ts && bun test dev/workflows/tests/issue-fixer-native.test.ts'
```

The tests cover delayed subprocess exit before checks, two rapid Claude follow-ups,
native cancellation while output projection is blocked, live correlation,
duplicate/stale replies, cancellation during a
fresh admission write, stop-before-local-signal and ambiguous writes, plus signed
interface authorization, Slack text decoding, immutable request targeting and
repeated fixed-message updates after an uncertain response. They do not prove a
provider executed or cancelled an action, an actual Slack delivery, or VM/process-tree
exclusion. The complete A/B interruption exercise still requires package 5's isolated
runtime/provider qualification. Do not mark those gates passed from these tests.

Protocol references checked 2026-09-20:

- [Codex app-server](https://learn.chatgpt.com/docs/app-server): the pinned binary's
  `app-server generate-ts` supplies the version-matched question/answer definitions.
  Question APIs are experimental. The sample initializes that capability explicitly.
- [Claude approvals and questions](https://code.claude.com/docs/en/agent-sdk/user-input):
  callbacks only cover requests reaching the native permission prompt, not all tools.
  No bypass mode or Session-wide grants are requested by this sample.
- [Slack signatures](https://docs.slack.dev/authentication/verifying-requests-from-slack/)
  and [event delivery](https://docs.slack.dev/apis/events-api/).
