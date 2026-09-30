# Test ownership and execution

Keep a test beside the implementation it directly verifies. Use this directory
for behavior across components and shared test inputs. Local, dedicated Dev and
managed staging are execution targets, not duplicate copies of the same cases.

| Location | Responsibility |
| --- | --- |
| Implementation directories | Unit and component tests, including API/DB tests next to their Go packages |
| `e2e/cases/<behavior>/` | Workload stimulus, assertions, fixture cleanup and the case's own helper tests |
| `e2e/support/` | Shared request deadlines and evidence/cleanup support, with their tests |
| `e2e/fixtures/schedule/` | A separately promoted scheduled workload, not background demand for ordinary cases |
| `browser/` | Browser interaction and visible application behavior |
| `build/` | Bundle construction, guest build inputs and build tooling across components |
| `release/` | Distribution contracts, signatures, publication, installation and artifact consumption |
| `fixtures/` | Inputs or fixture projects reused by multiple tests |

Environment composition and its tests live in `dev/local/` and `dev/runtime/`.
AWS module tests live beside their module under `infra/aws/`. A fixture used by
one case belongs to that case. Do not move tests out of their language's native
package just to put every test here. Human-facing examples live in `examples/`.

## Names

- Directories: `kebab-case`.
- TypeScript/JavaScript: `kebab-case.ts` or `.mjs`; tests use `.test.ts` or
  `.test.mjs`. Browser tests use `.spec.ts`.
- Shell: `kebab-case.sh`; tests use `.test.sh`.
- Python: `snake_case.py`; unittest modules use `test_<subject>.py`.
- Go: native `snake_case.go` and `_test.go` conventions.
- Documentation: `kebab-case.md`; preserve tool-defined names such as `README.md`,
  `Makefile`, `package.json` and generated files.

Name the behavior or subject, not `misc`, `utils`, `new` or `v2`. Avoid repeating
parent-directory information: a case can use `run.ts`, `task.ts` and `cleanup.ts`.
Keep explicit contract versions and fixture filenames whose spelling is itself
an input being tested. File organization does not rename public identifiers or
persisted workload names. Do not retain forwarding wrappers for moved internal
commands.

## Select the proof you need

Use the native runner for the affected implementation. These entrypoints cover
different boundaries; none of them is a universal acceptance gate.

Install the locked workspace dependencies and run `nix develop -c make
platform-entries` before focused Go checks that compile the CLI, host config or
builder packages. Repeat preparation after changing TypeScript or dependencies.
The Make build/test/lint targets and CI entrypoints perform this generation.

- `nix develop -c go test ./internal/jsoncanon ./internal/builder`: example
  focused package checks. Use the local database entrypoint below for selected
  tests that need real PostgreSQL.
- `nix develop -c python3 -m unittest discover -s dev/runtime`: host profile logic
  without deploying a host. The Linux process-boundary check remains separate.
- `nix develop -c bash dev/local/start.test.sh`: local composition tooling.
- `nix develop -c scripts/check-e2e.sh`: case typechecks and helper unit tests;
  does not run Tasks, Actors or browsers and does not prove runtime behavior.
- Build fixture analysis: run `scripts/build-compiler-entry.sh` and
  `scripts/build-hostconfig-entry.sh`, then `node tests/build/check-fixture-analysis.mjs`
  in the Nix development shell. This is compiler analysis, not real execution.
- `scripts/check-packed-sdk-consumer.sh`: packed SDK import/type contract.
- External examples: build local SDK packages, then install/typecheck/test the
  selected example in its own directory. They are not required by case checks.
- Real behavior: choose cases and follow [E2E instructions](e2e/README.md).
- Browser: use the repository Playwright configuration and select the relevant
  spec. Artifact/provider tests retain their own explicit prerequisites.

Fast PR CI keeps its existing source checks. The broader Nix CI entrypoints
compose the same checks explicitly; changing file layout does not add real-host
or all-case execution to PRs. Absence from fast CI does not mean a test is unused.

## Reusing TypeScript build inputs

`nix run .#ci-typescript` builds fresh SDK/proto packages once, packs them into a
private temporary directory, and shares those inputs across its checks. The
archives are removed on exit. Compiler and host-config entries are also prepared
once before their consumers. Each aggregate invocation rebuilds from its current
checkout; there is no persistent build cache or implicit reuse of existing files.

The standalone commands above and `bun run test:ts` still prepare their own inputs.
For explicit composition, `check-e2e.sh --skip-sdk-build` requires fresh packages
in `dist/npm`, while `check-packed-sdk-consumer.sh --sdk-packages DIR` and
`check-fixture-analysis.mjs --sdk-packages DIR` consume the two previously packed
SDK/proto archives. `bun run test:ts:prepared` runs the same unit tests after the
caller has built SDK packages, compiler and host-config entries. These prepared
forms are for a caller that owns the build and keeps its inputs unchanged until
all consuming checks finish; they do not establish artifact freshness themselves.

## Local database verification

When changing SQL, schema, transactions, constraints or persisted state
transitions, select tests that exercise the affected behavior with PostgreSQL.
Include dependent authorization, idempotency or scheduling behavior when shared
queries or schema change. Pure logic tests can run without database services.

For example, from the repository root:

```sh
nix run .#ci-postgres -- '^TestPostgresClaimSlotIsScopedByEnvironment$' ./internal/idempotency
```

This runs the existing `scripts/ci-postgres.sh` with pinned tools. It starts fresh
temporary PostgreSQL and Redis processes on localhost and stops them on exit;
by default it also removes their temporary files. It does not use a developer's
existing database, shared staging or production. The example proves only the
selected idempotency scope behavior; choose the cases relevant to the change.

Selected tests must execute and pass: skips or missing matches fail the command.
A passing fast PR check, which skips PostgreSQL tests, is not database evidence.
Record the selected behavior, exact command, tested revision and result in the PR,
including any database behavior left unproved. See the
[selection details](e2e/README.md#real-postgresql-and-redis) for supported patterns
and the distinction between selected tests and the full database suite.
