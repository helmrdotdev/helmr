# Helmr Workflow Tasks

This task project contains internal Helmr dogfooding workflows.

## implement

`implement` coordinates an implementation loop across external coding agents:

1. Claude proposes a plan.
2. Codex critiques or revises the plan.
3. Cursor Composer 2.5 implements the approved plan.
4. Codex and Claude review the result.
5. Codex triages findings.
6. Cursor Composer 2.5 fixes findings.
7. The review/fix loop repeats until Codex triage reports zero findings.
8. A pull request URL is recorded.

The task does not run Claude, Codex, Cursor, or GitHub itself. It records their
outputs through Helmr waitpoints so the process is auditable in a run.

Example:

```sh
helmr deploy ./workflow --project helmr --environment dogfood

helmr run implement \
  --project helmr \
  --environment dogfood \
  --repo helmrdotdev/helmr \
  --ref main \
  --payload-json '{"goal":"Add the first dogfooding workflow task","targetBranch":"codex/add-implementation-workflow"}'
```
