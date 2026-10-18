# Pi agent starter

An editable Pi coding-agent project for Helmr. Deploy it with the Helmr CLI, then
talk to the Agent in Slack. Add the provider Secret named in `agents/settings.ts`
in the Console before deploying. The Computer receives a protected placeholder;
Helmr forwards the key only to the configured provider origin.

Choose Anthropic (default), OpenAI or OpenCode Go with
`helmr init --template pi --provider <provider>`. Edit the model, introduction and
key binding in `agents/settings.ts`. Model IDs include their Pi provider name.

`agents/agent.ts` defines the Computer and Agent. `pi-bridge.ts` retains one
native AgentSession across Turns, with native tools, configuration and extensions.
Messages sent during execution use native steering. Extension select, confirm,
input and editor dialogs use Helmr questions. Cancelled confirmations return false;
other cancelled dialogs return no answer. Custom terminal components require Pi's
interactive interface. Terminal display controls retain the SDK's headless behavior.
Dialogs requested without an active Helmr Turn return no answer.

Cancellation closes admission, clears queued steering and follow-up messages, and
joins native abort and any admitted prompt before reuse. Native tools must honor
cancellation or finish; an uncooperative custom tool can keep abort pending, leaving
Helmr's failed-convergence and Session containment boundary responsible for cleanup.
The bridge does not undo external effects or promise termination of detached work.

Reconstruction verifies the saved native file and session identity, loads history
through SessionManager, then waits for new input. Missing history fails initialization
instead of silently starting an empty conversation. The manifest is saved only after
initialization succeeds; a process lost before its first persisted prompt also fails
closed on reconstruction because Pi has not written history yet. Native startup extensions remain
application-owned and need qualification for any autonomous behavior they introduce.
The SDK is pinned to 1.3.0. Source and mock-model tests do not establish live provider
or protected transport compatibility.
