# Helmr Dev Client Smoke

This directory contains the external-client smoke run immediately before the
AWS/Firecracker validation. It uses only the v0 public contract:

- typed Workspace creation from a Sandbox ID and UUID-only Workspace refs;
- exact create and BasicExec idempotency replay;
- synchronous one-shot BasicExec with bounded stdin, stdout, and stderr;
- normal nonzero exit capture into the Workspace head;
- typed Task start on the same Workspace and typed Run wait;
- post-Task Workspace Exec verification of committed head continuity;
- typed different-Workspace child Task calls from both a Task and an Actor;
- successive Actor inputs, continuation Run placement, paginated durable output,
  Session close, and output retention after close;
- attempt-scoped metadata mutation and all four structured logger levels;
- finite, authenticated Run log and event page reads after projection;
- current Deployment retrieve and declarative Schedule list/retrieve/fire;
- Secret create, name ref, list, rotate, and revoke without reading its value;
- authenticated Run-independent Token create, retrieve, complete, list, and cancel;
- one externally created Token resuming two waiting Runs plus a
  completion-before-wait Run through `tokens.ref(id)`;
- Run list and cancellation of a parked timer Run;
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

The declared Schedule targets the stable Workspace key `release-gate`. The
client creates that Workspace with a fixed idempotency key and intentionally
keeps it until environment or campaign cleanup; deleting it after each smoke
would invalidate the deployed Schedule between repetitions.

Set `HELMR_CLIENT_SMOKE_RESULT_FILE` to produce the bounded machine result
consumed by deployment validation. The result contains
only check IDs and public resource IDs; it does not contain logs, payloads, or
secret values.
