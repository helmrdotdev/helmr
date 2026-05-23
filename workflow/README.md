# Helmr Workflow Tasks

This task project contains internal Helmr workflows.

## implement

`implement` coordinates an SDK-backed implementation loop across coding agents:

1. Claude Agent SDK proposes a plan.
2. Codex SDK critiques or revises the plan.
3. Cursor SDK implements with the requested Composer 2.5 model.
4. Codex and Claude review the result.
5. Codex triages findings.
6. Cursor SDK fixes findings.
7. The review/fix loop repeats until Codex triage reports zero findings.
8. The task commits, pushes the branch, and creates or reuses a draft PR.

The workflow project is standalone and depends on the published `@helmr/sdk`
package. It does not import from the parent repository.

The payload does not include a branch name. The Cursor implementation agent must
checkout a new `helmr/...` branch before editing; the workflow discovers that
branch after implementation and uses it for push and PR creation.

Required secrets:

- `ANTHROPIC_API_KEY` for the Claude Agent SDK.
- `OPENAI_API_KEY` for the Codex SDK.
- `CURSOR_API_KEY` for `@cursor/sdk`.
- `GITHUB_TOKEN` for branch push and draft PR creation.

Run artifacts are written to `.helmr-workflow-artifacts/` and excluded from the
feature commit.

`cursorModel` defaults to `composer-2.5`. Pass a different Cursor model alias,
such as `composer-latest`, if that is what the account exposes.

Example:

```sh
helmr deploy ./workflow --project helmr --environment dogfood

helmr run implement \
  --project helmr \
  --environment dogfood \
  --repo helmrdotdev/helmr \
  --ref main \
  --payload-json '{"featureDesign":"Add the first implementation workflow task"}'
```
