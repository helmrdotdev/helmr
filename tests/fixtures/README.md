# Shared test fixtures

Reusable test inputs and fixture projects live here. Keep fixtures used by only
one behavior case alongside that case in `tests/e2e/cases/`.

- `contracts/deployment-v0/golden.json`: shared JSON canonicalization, invalid
  input and manifest digest expectations consumed by the Go JSON, deployment
  and database tests. The file supplies test data; it does not deploy a runtime.
- `agentic-work/` and `native-environment/`: projects used by bundle-builder and
  guest integration tests.

Scheduled deployment fixtures remain in `tests/e2e/fixtures/schedule/` with their
own preparation and deployment instructions.
