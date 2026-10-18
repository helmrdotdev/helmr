# Claude Code Agent

A conversational Helmr Agent using Claude Agent SDK 0.3.301. One native process
and conversation stay alive across successful Turns in a Session. Follow-ups
remember earlier messages. Messages received during a Turn enter Claude's native
input stream: Claude may fold them into its current work or process them next.
The Turn waits for results covering all admitted input.

Claude's default permission policy remains in effect. Operations that require
approval and `AskUserQuestion` requests appear as Helmr questions in Slack or the
Console. Read-only operations that Claude permits automatically do not prompt.
Approvals apply once; the bridge does not grant persistent permission rules.

Background work can survive successful Turns. Cancellation requests cancellation of known native
tasks, closes the native process and waits for its exit before reusing the saved
conversation. If cleanup fails or the process disappears unexpectedly, Helmr
holds the Session for explicit recovery. Resumption loads saved history and
submits fresh input; it does not re-submit the interrupted request. The native SDK validates saved history when resuming; the bridge does not parse
its transcript format. A native resume failure holds the Session for explicit
recovery instead of silently creating another conversation.
This also applies if the first request stops before resumable history is saved;
start a new Helmr Session when no saved conversation can be recovered.

The execution image includes `procps` because Claude uses `ps` to discover tool
process descendants. Native shutdown completion does not establish that every descendant has exited.
The pinned SDK can leave descendants alive if discovery fails or times out;
the Session hold above does not detect that condition. If process discovery exceeds the native 500 ms timeout,
tool processes may remain alive after the next Turn starts; installing `procps`
does not resolve this case. Physical Session close contains these processes
through Helmr's existing Session process scope.

| File | Contents |
| --- | --- |
| `agents/settings.ts` | Agent name, introduction, model and model key binding. |
| `agents/<name>.ts` | Template and Agent, including the Session-owned bridge. |
| `agents/claude-bridge.ts` | Native process, input/result association, cancellation and reconstruction. |
| `agents/questions.ts` | One-time approval and question conversion. |

Native history is stored under `/var/lib/helmr/claude/<session-id>`, outside the
working directory. This is storage organization, not an additional security
boundary. Tools still follow Claude's permission policy. Interrupted instructions
remain in conversation history; resumption is not a transcript rewind.

Before deploying, add the `ANTHROPIC_API_KEY` Secret in the Console for the
target Environment. The key is only sent to `https://api.anthropic.com`; the
Computer sees a placeholder.

```sh
bun install
helmr deploy --project PROJECT --env ENVIRONMENT
```

The bridge publishes responses for admitted input. It does not publish unsolicited
background-task notifications to Slack, and does not register optional native UI
dialogs beyond tool approvals and `AskUserQuestion`. Arbitrary detached programs
are not covered by native task-stop receipts; failed convergence relies on Helmr's
Session containment.
Background notifications can trigger native model work between Helmr Turns.
Permission requests without an active Turn are denied; Claude's automatically
permitted operations still follow its native policy.

The starter preserves native settings-source and optional-traffic defaults. Native
user/project/local settings, including their hooks, follow the SDK's behavior.
The application explicitly selects `permissionMode: "default"` so native tool
approval requests can be routed to the active Helmr Turn.
Interrupted-turn resumption is explicitly disabled to preserve fresh-input-only
recovery; remove that pin only while retaining the same replay behavior and tests.
