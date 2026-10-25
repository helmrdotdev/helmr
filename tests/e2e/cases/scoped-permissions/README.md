# Scoped machine execution

This case executes real guests. Offline typechecks do not establish isolation.
Run only on an explicitly authorized dedicated runtime candidate with the current
schema and at least three available VM slots for the small fixture Computers.

Prepare `cases/scoped-permissions` through `tests/e2e/prepare_project.py` and deploy
that prepared fixture using the normal authorized deployment workflow. Supply
`HELMR_API_URL`, a verification `HELMR_API_KEY` with the Session Operator and Computer Administrator Roles, and a new private `HELMR_EVIDENCE_DIR`. The operator key owns
verification cleanup, not either restricted execution identity.

Set `HELMR_SCOPED_KEY_A` and `HELMR_SCOPED_KEY_B` to two different Environment keys,
each granting only `agents.use` in that same Environment. Keep tokens in the normal
secret surface and outside evidence. Run the prepared project's
`cases/scoped-permissions/run.ts`. The case never creates credentials, deploys,
changes a role, starts a host, or modifies infrastructure.

The case asserts distinct native Computers and empty initial guest file state
across key origins, persistence within the first Session, absence of the `HELMR_API_KEY`
environment variable, denial of cross-origin read/events/input/Ask answers, denial of shell
Computer metadata, and rejection of broad attachment to the scoped Computer.
The standard verifier records owned IDs, cancels every created Session and deletes
its Computers on success or failure. Recover recorded fixtures after interruption,
then follow the dedicated host's normal stop procedure.

This case does not certify human/key cross-origin behavior, helper lineage,
snapshot restore, role removal during streams, or provider network isolation.
It does not inspect every environment variable, mounted credential file or runtime
credential channel, so the single-variable probe does not certify absence of all
ambient Helmr credentials. Those remain separate acceptance evidence; do not infer them from this result.
