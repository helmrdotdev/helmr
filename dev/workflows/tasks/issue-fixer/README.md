# Issue fixer with native human interaction

These editable Actors use the prerelease Session/Turn SDK with the provider versions
already pinned in `dev/workflows/package.json`: Codex app-server 0.133.0 and Claude
Agent SDK 0.3.149. They are application examples, not a provider adapter package.
The Actors accept JSON and validate it locally with Zod. Input formats are not
declared on the Actor.

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
Each Helmr Turn starts a native process and resumes the Session's saved native
conversation when present. The conversation files must survive in the Workspace;
see the continuity requirements and qualification limits below.

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
send The login form loses its error message
fix Add a regression test for password reset
send Keep the current public API
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
Automatic `session.send({issue})` uses the same conversational payload both idle and
active. The Slack `send` command demonstrates it; `fix` deliberately queues a new
Turn. Both native Actors accept issue payloads as follow-ups, alongside independently
validated exact questions/approvals. Codex registers its handler only after the native
Turn exists, so startup input can wait in Helmr without being prematurely rejected.
A provider completion can still beat delivery; the application rejects that message
rather than pretending it was consumed. Inspect the retained message disposition.

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
  It also enables `features.default_mode_request_user_input`; on pinned 0.133.0,
  experimental client capability alone does not allow questions in Default mode.
- [Claude approvals and questions](https://code.claude.com/docs/en/agent-sdk/user-input):
  callbacks only cover requests reaching the native permission prompt, not all tools.
  No bypass mode or Session-wide grants are requested by this sample.
- [Slack signatures](https://docs.slack.dev/authentication/verifying-requests-from-slack/)
  and [event delivery](https://docs.slack.dev/apis/events-api/).

## Conversation continuity

`conversation.ts` stores each Session/provider's native conversation ID and provider
home under `<repository>/.helmr/issue-fixer/<session>/<provider>`. Keep `.helmr/` out
of commits and preserve it in the captured Workspace; do not delete it when checking
out the next task. Codex uses this location as CODEX_HOME, starts a persisted thread
and later calls thread/resume. Claude uses it as CLAUDE_CONFIG_DIR with persistence
and later passes resume. Configure existing provider authentication through the
application's environment; the samples do not copy credentials from host homes.
These locations contain private transcripts and possibly provider state.

Each Run can start fresh native processes while retaining conversation history.
A missing/corrupt saved conversation must fail visibly, never silently start a new
conversation. Only provider history/state persists: pending approval callbacks and
permission grants are not reconstructed. An interruption still requires Helmr's
physical convergence and exact current-hold resume before queued work proceeds.

The local Claude fixture invokes the Actor twice with separate Run heaps and checks
that the second query receives the persisted native ID. This proves application
wiring only, not native model memory, provider crash durability or Workspace restore.
Native end-to-end context continuation remains a runtime/provider qualification gate.

Run the real pinned Codex process qualification separately from mocked native tests:

```sh
nix develop --command bun test dev/workflows/probes/codex-conversation.test.ts
```

This probe uses a loopback Responses fixture and the actual Actor/app-server code.
It checks native questions and answers (including the answer after resume), denied
command approval, interruption while a question is pending, stale reply rejection,
and saved conversation identity/history after restarting the native process.
It does not call a remote model or execute the denied command. Helmr handler
delivery and repository checks are fixtures; this is not a deployed Session,
queue/hold exercise, crash-durability proof or VM Workspace restore test.

A non-inference probe of pinned Codex 0.133.0 accepted thread/start but a new
process immediately attempting thread/resume returned "no rollout found". An empty
thread's ID is not proof of persisted history. The sample deliberately propagates
that failure instead of silently opening a replacement conversation; applications
must reconcile missing native history. Neither a Helmr output receipt nor Workspace
capture can make a provider persist data that it has not written. The probe made no
turn/start, model, login or outbound interface request.
