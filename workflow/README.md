# Helmr Workflow Tasks

This task project contains internal Helmr workflows.

## implement

`implement` coordinates an SDK-backed implementation loop across coding agents:

1. Cursor SDK explores the repository against the feature design and writes an exploration artifact.
2. Claude Agent SDK proposes a plan using that artifact.
3. Codex SDK critiques or revises the plan.
4. Cursor SDK implements with the requested Composer 2.5 model.
5. Codex and Claude review the result.
6. Codex triages findings.
7. Cursor SDK fixes findings.
8. The review/fix loop repeats until Codex triage reports zero findings.
9. The task commits, pushes the branch, and creates or reuses a draft PR.

The workflow project is standalone and depends on the published `@helmr/sdk`
package. It does not import from the parent repository.

The payload does not include a branch name. The Cursor implementation agent must
checkout a new `helmr/...` branch before editing; the workflow discovers that
branch after implementation and uses it for push and PR creation.

If the run workspace does not include Git metadata, the workflow recreates a
checkout from `payload.repository` or `GITHUB_REPOSITORY`. `payload.ref`
selects the checkout ref and defaults to `baseBranch`.

Claude planning and Codex plan review can ask the operator one question at a
time through `ctx.wait.message()`. Set `operatorInput` to `false` to disable
those pauses. `operatorInputTimeout` defaults to 3600 seconds, and
`maxOperatorQuestionsPerPhase` defaults to 3.

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
  --payload-json '{"repository":"helmrdotdev/helmr","ref":"main","featureDesign":"Add the first implementation workflow task"}'
```
