# Build a small website with OpenCode

Turn a short brief into local website files, then improve them in the same
conversation. This editable Agent uses the embedded OpenCode SDK **2.0.31** and
OpenCode Go. Each Helmr Session keeps one native conversation and workspace.

## Try it

From the repository root, prepare the SDK with
`nix develop -c ./scripts/build-npm-packages.sh`. In this directory, run
`bun install --frozen-lockfile --ignore-scripts` through the repository Nix
environment. This checkout uses locally built SDK packages; it is not a
standalone download with published dependencies already substituted.

Create `OPENCODE_GO_API_KEY` in the target Environment through your existing
Secret-management workflow. The Computer binds that Secret to `OPENCODE_API_KEY`
in protected mode for `https://opencode.ai`. The workspace receives a placeholder;
the platform substitutes the key only on authorized upstream requests. Do not
put a key in source, image layers or Turn input.

```bash
helmr deploy PATH/TO/opencode-assistant --project PROJECT --env ENVIRONMENT
helmr session create opencode-assistant --text 'Build a small website for a community event. Ask me about the audience and date before creating files.' --project PROJECT --env ENVIRONMENT --json
```

Use the returned Session and Turn IDs. Open the Session in Console to answer
questions and approve individual operations, then inspect the outcome:

```bash
helmr session turn wait TURN_ID --session SESSION_ID --timeout 5m --project PROJECT --env ENVIRONMENT --json
helmr session enqueue SESSION_ID --text 'Add a schedule and make the layout work on a phone.' --project PROJECT --env ENVIRONMENT --json
```

The second request uses the same conversation and working files. The Agent should
explain what it changed and which checks it ran. Inspect `/workspace/index.html`
and any accompanying CSS/JavaScript with the Computer shell or download the files
through your existing workflow. These are local files; this example supplies no
hosting or preview URL. The model may produce different layouts on different runs.

Optionally connect the Agent's Slack app in Console and invite it to a channel.
Mention it with the same brief, answer its questions, and mention it again in the
same thread for follow-ups. No Slack credentials belong in application code.

## Application choices

- OpenCode Go uses `opencode-go/glm-5.3`. The provider policy enables only
  `opencode-go`; provider/model choices live in `agents/settings.ts`.
- Native commands and file edits ask for **one-time** human approval. The example
  does not offer persistent permission grants. Ordinary reads and searches are
  allowed, with additional questions for `.env` files. Subagents and access
  outside the working directory through native file tools are denied.
- Sharing, updates and project/Claude configuration discovery are disabled for
  this scratch-workspace application. Change `nativeOptions` deliberately if
  your application should load project instructions or other native configuration.
- These are native tool policies, not a sandbox around an approved shell command.
  Such a command has the Computer's ordinary filesystem/process access. There is
  no repository authentication, account provisioning or publication workflow.
- Live messages steer the running native task. Stop joins native cancellation;
  pending questions are withdrawn. Native failures do not become successful
  responses. Follow-ups reuse the Session, and reconstruction opens saved native
  history without replaying the old request.

The SDK runs in the Agent process; there is no separately installed `opencode
serve` process. Native state lives under `/var/lib/helmr/opencode/`. This version
does not migrate old example state from `/workspace/.helmr/opencode/`; use a new
Session when trying the updated example.

## Code and checks

`agents/assistant.ts` defines the Computer, protected Secret and Agent. Its small
factory makes paths and native options editable for local use. `settings.ts`
owns the website instructions and application policy. The bridge, content and
recovery files match the OpenCode starter; `questions.ts` keeps this example's
one-time permission choices.

In this directory, run `bun run typecheck` and `bun run test` through Nix. Tests
use the real native SDK with a local synthetic model and the example's permission
configuration. They create and revise files, deny an operation, answer/cancel
questions, steer, handle model failure and reopen saved history. They do not
establish real-model website quality, OpenCode Go connectivity, VM behavior or
Slack/browser acceptance.

After preparing the compiler entries and installing both examples, run
`bun scripts/check-example-projects.ts` from the repository root to check shared
source/dependency alignment and compile the projects. The Nix TypeScript checks
run this automatically. These local checks do not build the Linux Computer image.

## Dependency maintenance

The example overrides transitive MCP client/core and provider-utils versions to
include the fixes for [MCP OAuth credential routing](https://github.com/advisories/GHSA-6qxp-vccf-f47h)
and [provider response limits](https://github.com/advisories/GHSA-866g-f22w-33x8).
The deployment's runtime installation preserves these overrides.

At these pins, `bun audit` also reports [braces](https://github.com/advisories/GHSA-vfj7-8cjw-p6xm)
and [sprintf-js](https://github.com/advisories/GHSA-hp3w-g68c-fv3c), neither with a
listed patched release. The installed watcher calls `micromatch.makeRe`, which
uses picomatch rather than the affected braces walkers. Gray-matter uses the
js-yaml library; its argparse/sprintf dependency belongs to the separate YAML
CLI, not this application's YAML parsing path. This is a bounded call-path
assessment, not a clean audit result. Recheck it when changing dependencies or
adding integrations.
