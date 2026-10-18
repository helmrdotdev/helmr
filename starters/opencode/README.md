# OpenCode agent starter

An editable OpenCode 2 project for Helmr. Deploy it with the Helmr CLI, then talk
to the Agent in Slack. Add the provider Secret named in `agents/settings.ts` in
the Console before deploying. The Computer receives a protected placeholder;
Helmr forwards the key only to the configured provider origin.

Choose Anthropic (default), OpenAI or OpenCode Go with
`helmr init --template opencode --provider <provider>`. Edit the model, introduction
and key binding in `agents/settings.ts`. Keep provider names in OpenCode model IDs.

`agents/assistant.ts` defines the Computer and Agent. `opencode-bridge.ts` retains
one embedded OpenCode host and dedicated database per Helmr Session. Successful
Turns reuse native state and may leave intentional background work alive. Messages
sent during execution use native steering. Native permission requests and forms
are answered through Helmr questions; a saved permission uses OpenCode's own
persistence and scope, not a Helmr-wide permission grant. External browser forms
require the native interface.

Cancellation closes input admission, joins native interruption and closes the host
scope, including background notification fibers. The next Turn reconstructs the
host before admitting input. Recovery uses native terminalization with automatic execution
disabled, then retires pending inputs and recovery notifications in this Session's
database. It keeps recorded history and admits only new Helmr input afterward.
Missing reconstructed history is an initialization error. Native plugins and
configuration remain application-owned; custom autonomous startup behavior needs
its own qualification. The bridge does not undo external effects or guarantee
termination of arbitrary detached programs.

The SDK and recovery composition are pinned to OpenCode 2.0.31. Change the package
versions together and verify recovery before upgrading. Source and mock-model
tests do not establish live provider or protected WebSocket compatibility.
