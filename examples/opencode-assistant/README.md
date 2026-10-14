# OpenCode assistant

An editable Helmr Agent using OpenCode and OpenCode Go. One native server and
conversation belong to each Helmr Session. Slack and Console use the same Agent;
the application does not store Slack credentials or host another Slack endpoint.

Create the `OPENCODE_GO_API_KEY` Secret in the target Environment for your
OpenCode Go API key. The declaration uses that same identifier. Never put the key in source,
an image, a prompt or a Slack message. Protected Secret delivery substitutes the
key only on authorized HTTPS requests to `https://opencode.ai`; the Computer and
its commands receive a placeholder. The sample selects `opencode-go/glm-5.3`
for both the main and small model, with no other enabled provider. Change those
two model fields together if you prefer another model in your Go subscription.

Deploy this directory through the ordinary Helmr CLI. Open `opencode-assistant`
in Console and connect its dedicated Slack app. Invite that app to the intended
workspace channel. Each Agent has one configured Slack connection.
Mention the installed Helmr app with a plain-language request. OpenCode questions
become Helmr questions that can be answered in Slack; command and edit permissions
use one-time choices. A final response is staged until successful Turn settlement.
Reasoning and raw tool output are not copied into the public conversation.

The Computer starts with an empty `/workspace` and ordinary Node, Git and ripgrep.
The slim Debian Node image bootstraps HTTPS package installation with Node's
bundled root certificates, then installs the system CA bundle. Package installation
uses HTTPS repositories, and the small base keeps initial Computer preparation light.
It has no GitHub authentication or repository access. Use a small scratch project
first, or deliberately prepare a selected repository before deployment. Native
commands and edits request approval, external-directory access and subagents are
disabled, and automatic sharing and project OpenCode configuration are disabled.
Repository `AGENTS.md`/`CLAUDE.md` instructions are also not automatically loaded;
put trusted application instructions in this adapter's system prompt instead.
Edit approvals include the native diff; denying an operation lets the model
explain or propose another approach. These are application policies, not a
security boundary between OpenCode and approved code in the same Computer.
Approved commands have the Computer's filesystem and local process access,
including the native server's environment. Review commands and edits accordingly;
the platform's protected Secret and Computer isolation are separate boundaries.

Completed follow-ups reuse the native conversation. While a Turn is running,
answer its question or enqueue another instruction; live message steering is
explicitly rejected. Stop aborts the current native prompt and pending questions;
an unresponsive server is closed and cannot be silently reused. Native history
is retained under `/workspace/.helmr/opencode/` with the Computer.
The sample's local HTTP client allows human answers to take longer than five
minutes. Turn deadlines and native cancellation still bound the operation.

For local checks, first build the repository's SDK with
`scripts/build-npm-packages.sh`, then run `bun install --frozen-lockfile
--ignore-scripts`, `bun run typecheck`, `bun run test`
and `bun run test:native`
inside this example through the repository Nix environment. The native check
uses OpenCode 1.18.30 and a local synthetic model. It does not use your Go key,
call paid models, or establish deployed VM/Slack acceptance.
The native check includes a permission answer delayed for 310 seconds and then
checks that the same process and conversation can complete another Turn.

Assistant explanations preceding a tool boundary are written as retained human
output. The final text candidate is held until the owned native prompt completes
and is then staged once as the response. Tool events, arguments and results do
not become public text. Questions and permission requests remain explicit human
interactions; Helmr shows processing through the channel’s native status.

## Try a conversation

Start with ordinary conversation; there are no fixture commands to memorize.
For example, mention the connected Agent and send:

> Help me plan a small personal website. Ask one question with two short choices
> about its purpose before proposing the page structure. Allow me to write my own answer.

Answer in Slack, then mention the same Agent in the same thread to continue:

> Use that choice. Suggest three sections and a short title for each.

To exercise descriptions and multiple selection, ask:

> Ask me which website features to include, with three explained choices and
> multiple selection enabled. Wait for my answer before continuing.

Short choices appear as buttons; explained choices use radios, and multiple
choices use checkboxes. Radios and checkboxes require Submit answer. Writing a
custom answer opens a modal. Answering a question does not require another mention.
An ordinary follow-up does require the Agent mention.

To try Stop, request a scratch task that takes enough time to interrupt, such as
creating a small multi-page website in `/workspace`. Approve only the native
operations you intend to run, then use Slack's native Stop while it is working.
After cessation, mention the Agent again to continue. A question-only wait is
suspended and has no native Slack Stop; answer it, or interrupt the exact Turn
through the CLI. Stop does not undo files already written by an approved command.

The editable entry point is `agents/assistant.ts`: it selects the workspace,
Secret and model, and delegates each Turn to the native adapter. The other files
own OpenCode process lifetime, question translation and human-text routing.
The sample is conversational but is not granted repository or account access.
