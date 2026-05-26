# Helmr Workflow Tasks

This task project contains internal Helmr workflows.

## implement

`implement` coordinates an SDK-backed implementation loop across coding agents:

1. Cursor SDK explores the repository against the feature design and writes an exploration artifact.
2. Claude Agent SDK proposes a plan using that artifact.
3. Codex SDK critiques or revises the plan.
4. Cursor SDK implements with the requested Composer 2.5 model.
5. Codex and Claude review the result.
6. Codex triages findings, keeping only evidence-backed blockers and dropping
   false positives, speculative risks, nits, and duplicate concerns.
7. Cursor SDK fixes findings.
8. The review/fix loop repeats until Codex triage reports zero findings. The
   review-round limit is a runaway guard and defaults to 100.
9. The task commits, pushes the branch, and creates or reuses a draft PR.

The workflow project is standalone and depends on the published `@helmr/sdk`
package at runtime. Local typechecking maps `@helmr/sdk` to this repository's
SDK sources so workflow code can use in-repo context types while the deployed
task remains installable from published dependencies.

The payload does not include a branch name. The Cursor implementation agent must
checkout a new `helmr/...` branch before editing; the workflow discovers that
branch after implementation and uses it for push and PR creation.

Repository identity, requested ref, resolved SHA, pull request metadata, and
workspace paths come from `ctx.source` and `ctx.workspace`, which are supplied by
the Helmr runtime from the run source. The payload contains implementation
intent only, and the workflow requires a GitHub run source. For tag, SHA, or
unknown sources, pass `prBaseBranch` when the PR base cannot be inferred from
source metadata.

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

`claudeModel` defaults to `claude-opus-4-7`, `codexModel` defaults to
`gpt-5.5`, and `cursorModel` defaults to `composer-2.5`. Pass different model
aliases if that is what the account exposes.

Example:

```sh
helmr deploy ./workflow --environment dogfood

helmr run implement \
  --project helmr \
  --environment dogfood \
  --repo helmrdotdev/helmr \
  --ref main \
  --payload-json '{"featureDesign":"Add the first implementation workflow task"}'
```

## light-implement

`light-implement` is the small-task version of `implement`. It keeps the SDK and
Git safety rails plus a review/fix loop, but removes separate exploration,
planning, cross-review, and operator input.

Workflow design:

1. Prepare the GitHub workspace from `ctx.source` and `ctx.workspace`.
2. Require a clean base workspace.
3. Write a compact implementation brief artifact.
4. Run one Cursor SDK implementation pass.
5. Require the agent to have checked out a new safe `helmr/...` branch.
6. Run a lightweight review/fix loop:
   - Review: Claude delegates the current diff review to a custom
     `light-code-reviewer` subagent.
   - Triage: Codex filters the review into structured, evidence-based
     actionable findings and drops false positives, speculative risks, nits,
     and duplicate concerns.
   - Fix: Cursor applies only the triaged findings.
7. Repeat review, triage, and fix until triage reaches zero findings. The
   review-round limit is a runaway guard and defaults to 100.
8. Commit, push, and create or reuse a draft PR after triage reaches zero findings.

This task is intended for narrow changes where the feature design is already
clear and the validation surface is small. Use `implement` when the work needs
separate exploration, planning, independent cross-review, or operator input.

Required secrets:

- `CURSOR_API_KEY` for `@cursor/sdk`.
- `ANTHROPIC_API_KEY` for the Claude review coordinator and review subagent.
- `OPENAI_API_KEY` for Codex triage.
- `GITHUB_TOKEN` for checkout, branch push, and draft PR creation.

Example:

```sh
helmr run light-implement \
  --project helmr \
  --environment dogfood \
  --repo helmrdotdev/helmr \
  --ref main \
  --payload-json '{"featureDesign":"Fix the typo in the install docs"}'
```
