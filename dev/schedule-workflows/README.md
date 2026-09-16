# Helmr Schedule Validation Workflows

This case-owned bundle exists only to validate declarative Schedule admission,
fire, and archival. It must be promoted into the same environment as the
ordinary dev workflows and followed by an unconditional promotion of
`dev/workflows`, which is intentionally schedule-free.

Do not add permanent background work or unrelated smoke Tasks here.

## SDK resolution

Imports resolve the installed `@helmr/sdk` dependency, including its declarations;
there is no alias to monorepo SDK source outside this project. For local repository
checks, `dev/workflows/scripts/sync-local-sdk.sh` builds and installs the declared
local packages for both samples before typechecking. Release consumers install
the selected SDK/proto package bytes before building the unchanged sample config.
