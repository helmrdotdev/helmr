# Helmr Dev Workflows

This task project contains Helmr product development diagnostics.

These workflows validate Helmr runtime behavior, waitpoints, checkpoints, and
agent toolchain availability. Company operating workflows live in
`../../../company/automation`, not in this product repo.

## Tasks

| Task | Purpose |
|------|---------|
| `toolchain-check` | Validates the task image, Nix, GitHub access, agent SDKs, and basic namespace/runtime assumptions. |
| `checkpoint-waitpoint-diagnostic` | Exercises human waitpoints across checkpoint restore boundaries. |

## Deploy & run

```sh
helmr deploy ./dev/workflows --environment dogfood

helmr run toolchain-check \
  --project helmr \
  --environment dogfood \
  --payload-json '{"repository":"helmrdotdev/helmr","ref":"main"}'
```
