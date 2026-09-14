# Bootstrap artifact stores

By default this module creates the Platform Artifact publisher role and inline
policy. Supply a nonempty `platform_publisher_principal_arns` list of IAM role or
user ARNs; the existing role name, trust and create-only permissions are preserved.

Callers that publish through their own authority can set
`create_platform_publisher = false` and omit the principals. This removes only the
publisher role and policy; `platform_publisher_role_arn` becomes null. Artifact
stores, repository, keys and bucket protections remain managed by this module.
Check consumers of the role/output before applying the saved removal plan.

The module includes native singleton-to-`[0]` address moves for the role and inline
policy. Existing enabled installations need no manual state move or import.

From the repository root, run focused checks with the pinned tools:

```sh
nix develop .#infra --command bash -c \
  'cd infra/aws/modules/bootstrap && tofu init -backend=false -input=false && tofu fmt -check -recursive && tofu test'
nix develop .#infra --command python3 infra/aws/modules/bootstrap/tests/publisher_moves_test.py
```

The module tests use a mock AWS provider. The migration test projects the actual
IAM blocks into an isolated fixture with literal artifact dependencies, seeds
singleton fixture state from native mock output plus known real-provider plan
defaults, and verifies real-provider native
plans with refresh and credential discovery disabled: enabled moves are no-ops;
disabled resources are deletions. It does not read deployment state or prove live
IAM behavior. The full-module mock test separately checks retained store/key
identities when disabling an existing publisher.
