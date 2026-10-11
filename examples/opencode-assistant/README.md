# OpenCode assistant

An editable Helmr Agent using OpenCode and OpenCode Go. One native server and
conversation belong to each Helmr Session. Slack and Console use the same Agent;
the application does not store Slack credentials or host another Slack endpoint.

Create an Environment Secret for your OpenCode Go API key, and replace the example
Secret UUID in `tasks/assistant.ts` with its stable ID. Never put the key in source,
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

For local checks, first build the repository's SDK with
`scripts/build-npm-packages.sh`, then run `bun install --frozen-lockfile
--ignore-scripts`, `bun run typecheck`, `bun run test` and `bun run test:native`
inside this example through the repository Nix environment. The native check
uses OpenCode 1.18.30 and a local synthetic model. It does not use your Go key,
call paid models, or establish deployed VM/Slack acceptance.

Tool start events do not become conversation progress messages. The example
returns the assistant response and surfaces questions and permission requests;
Helmr shows processing through the channel’s native status.
