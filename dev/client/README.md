# Helmr Dev Client Smoke

This directory contains the external-client smoke run immediately before the
AWS/Firecracker validation. It uses only the v0 public contract:

- typed Workspace create and ref by declared ID;
- exact create and BasicExec idempotency replay;
- synchronous one-shot BasicExec with bounded stdin, stdout, and stderr;
- normal nonzero exit capture into the Workspace head;
- committed file read, stat, and list without a live VM;
- typed Task start on the same Workspace and typed Run wait;
- typed different-Workspace child Task calls from both a Task and an Actor;
- terminal Actor Run polling and durable Actor output inspection;
- attempt-scoped metadata mutation and all four structured logger levels;
- finite, authenticated Run log and event page reads after projection;
- authenticated Run-independent Token create, retrieve, and cancel;
- idempotent Workspace deletion.

The old public materialize/connect/stop, async process, stream, and PTY
diagnostics were removed with those surfaces.

Run from the repository root:

```sh
HELMR_API_URL=https://dev.helmr.dev \
HELMR_API_KEY=... \
dev/client/scripts/workspace-lifecycle-smoke.sh
```

The script deploys `dev/workflows` first. Set `SKIP_DEPLOY=1` to reuse the
current deployment.

Set `HELMR_SMOKE_SECRET_NAME` to an Environment Secret name to additionally
verify immutable Secret delivery to BasicExec. The secret value is never
printed; the smoke only reports that a non-empty value was present.
