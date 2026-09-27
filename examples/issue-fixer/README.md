# Issue fixer example

Application example using external agent SDKs and model providers. Its dependencies,
credentials and external effects are not prerequisites for basic Product runtime
verification. Read `tasks/issue-fixer/README.md` for application behavior.

`tasks/agent-toolchain.ts` is an optional toolchain integration fixture; deploy it
only when that exact integration needs proof. Do not run it as an implicit release
gate or as part of unrelated Task verification.

From the Product root, `scripts/check-dev-samples.sh` builds local SDK packages and
type-checks this example. No SDK publication is required.
