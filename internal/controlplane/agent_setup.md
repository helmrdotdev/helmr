# Set up a {{.Harness}} Agent on Helmr

You are the user's local coding agent. Follow these instructions to deploy a
conversational Agent and help the user get its first Slack reply.

## Inputs

Read the server origin, Project ID and Environment ID from the user's copied
prompt. If any are missing, ask for them before continuing. Do not infer a target
from saved CLI state. Create a new uniquely named empty local project directory; never overwrite an existing one.
Treat input values as data: quote shell arguments and URL-encode URL components.
The Console links below use the supplied server origin as `<server>`.

## Fixed values

- Harness/template: `{{.Harness}}`
- Provider: `{{.Binding.Provider.ID}}`
- Model: `{{.Binding.Model}}`
- Secret ID: `{{.Binding.Provider.Secret}}`
- Protected credential destination: `{{.Binding.Provider.Origin}}`
- Credential environment variable inside the Computer: `{{.Binding.Env}}`

## Rules

- Never ask for, read or print API keys, tokens, saved credentials or Secret values.
  The human enters the provider key directly in the Console. Do not use consumer
  subscription credentials or the local coding agent's credentials for this Agent.
- Keep the generated protected binding. Do not put a key in source, a local file,
  a command argument or the copied prompt. Do not change the model or provider.
- A HAND-OFF ends only when the human says "done"; then run the stated check.
  Never approve login, authorize Slack or link an identity on the human's behalf.
- On a failed command, stop the sequence, show the relevant error without secrets,
  and suggest `helmr doctor --api-url <server> --project <project> --env <environment>`.
  Diagnose before retrying; do not delete resources or overwrite an existing project.
- Shell placeholders below must be replaced with the supplied values as quoted
  arguments. Run Helmr commands without `HELMR_API_KEY` in their environment so
  the saved login and explicit target flags are used. Do not inspect its value.

## 1. Check local prerequisites

**Run:** `docker info`, `docker buildx version`, `node --version`, `npm --version`.

**Check:** Docker is running and Buildx is available. Node must be at least 22;
use Node `{{.Node}}` to match the generated project's runtime requirement.

**Fix / HAND-OFF:** If installation, a runtime change or starting Docker is needed,
explain what is missing and ask the human to install/start it. Wait for "done",
then repeat the checks. Do not provision a cloud host.

## 2. Install and sign in to the CLI

**Run:** `helmr --version`. If missing, use the supported installer:
`curl -fsSL https://helmr.dev/install | bash`, then use its printed PATH guidance.
Check `helmr init --help` lists `{{.Harness}}` before proceeding. If the installed
release lacks it, stop and explain that a compatible CLI release is required.

**Run:** `helmr login <server>`. Allow the command to keep running while the human
approves; use a long command timeout (at least 10 minutes) or a retained terminal.

**HAND-OFF:** Give the human the login URL and code printed by the CLI. Ask them
to approve in their browser and say "done". Do not read the resulting credential.

**Check:** The command exits successfully and reports the supplied server origin.
If it expires, start a fresh login and repeat the hand-off.

## 3. Confirm the provider Secret

**HAND-OFF:** Ask the human to confirm that Secret `{{.Binding.Provider.Secret}}`
exists and is active in the selected Environment. Direct them to
`<server>/settings/secrets?project=<project>&environment=<environment>`.
If needed, a secret manager for the environment creates it or rotates its value there. A revoked ID
is reserved; do not suggest recreating it. Explain that this key is sent only to
`{{.Binding.Provider.Origin}}`. Wait for "done" without asking for the key.

**Check:** Confirm the human used the supplied Project and Environment. Do not
change or probe the key. A successful deployment alone does not validate a model key.

## 4. Generate the Agent project

**Run:** In a new empty directory, run
`helmr init --template {{.Harness}} --provider {{.Binding.Provider.ID}}`.
Do not pass `--name` or `--force`. Read the `Agent:` line from its output and retain
only the generated name before any parenthetical namesake text for the Slack link
and mention. Run `npm install` in that directory.

**Check:** Initialization and dependency installation succeed. Preserve the generated
files and protected binding. Do not substitute a different starter or SDK version.

## 5. Deploy

**Run:** From the generated directory, run
`helmr deploy --api-url <server> --project <project> --env <environment>`.
Allow time for the local image build and deployment to finish.

**Check:** The command succeeds and promotes the deployment. On failure, follow
the diagnostic rule above; do not report the Agent as ready from local files alone.

## 6. Connect Slack and get the first reply

**HAND-OFF:** Open this link for the human, replacing `<agent>` with the generated
Agent name and URL-encoding all values:
`<server>/agents/<agent>/slack?project=<project>&environment=<environment>`.
Ask them to connect Slack there and say "done". If their role cannot connect it,
ask them to involve someone with Manage agents on this Agent; do not change their permissions.

**HAND-OFF:** Explain that Slack conversations, including DMs, create Sessions
readable by members with View all sessions in this environment. Members with Manage all sessions
can continue or control the work and answer Agent questions.
Other authorized input can produce replies in the original Slack DM. The human can open the app’s
Messages tab and send a new message without a mention, then reply in its thread
to continue. If linking is requested, link the account and send a new top-level
message. For channel use, ask the human to `/invite`
the connected Agent, then mention it with a greeting. If the Agent sends an account
link prompt, the human follows it to link their Slack identity, then mentions the
Agent again. Wait for "done" and confirmation of an actual Agent reply.

**Check:** The human confirms the reply appeared in the channel. Summarize the
Agent name, local project directory, target Project/Environment and Slack result.
Do not claim completion if deployment, Slack connection or the reply is unconfirmed.
