# Codex Agent

Create this project with `helmr init --template codex`. Add the `OPENAI_API_KEY`
Secret in the Console for each deployment Environment, install dependencies with
`bun install`, then run `helmr dev`.

`agents/assistant.ts` defines the Computer and Agent. `agents/settings.ts` selects
the name, model and key binding. The other files in `agents/` are editable
application code that connects the Codex app-server to Helmr.

The Agent keeps one `CodexSession` in `initResult`. Normal Turns share a native
owner and conversation. A successful Turn may leave native background commands
running in the Session; a later cancellation cleans them and joins their exit
receipts before closing the owner. `onMessage` steers the exact active native Turn; questions
and approvals use `turn.ask`; refusing approval produces a normal response. Native
configuration and conversation identity live under `/var/lib/helmr/codex`, outside
the tools’ writable workspace. Completed commentary goes to `turn.output`, and the
final answer goes to `turn.respond`.

On cancellation, the bridge interrupts the native Turn, cleans its background
terminals, waits for known command completion and closes the native owner. The
next Turn opens the saved conversation on the Computer's disk. Goals and native subagents are disabled
at every native launch, and queued submissions are deleted before resume so opening
a conversation does not restart cancelled work. Missing saved history or identity
is an error when reopening an established conversation or reconstructing the
Helmr Session. If Helmr must reconstruct before the first native conversation
identity was saved, the bridge cannot recover that history and reports the error.
If cleanup cannot be confirmed, `onCancel` fails and Helmr holds the Session for
reconstruction. Arbitrarily detached processes created by user programs are outside
the native command receipts; extend cleanup for such programs before enabling them.

Repository validation imports this same bridge and exercises real Codex and Helmr
Turn runtime code against a local mock model. It does not use provider credentials.
