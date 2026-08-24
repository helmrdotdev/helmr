# Helmr Schedule Validation Workflows

This case-owned bundle exists only to validate declarative Schedule admission,
fire, and archival. It must be promoted into the same environment as the
ordinary dev workflows and followed by an unconditional promotion of
`dev/workflows`, which is intentionally schedule-free.

Do not add permanent background work or unrelated smoke Tasks here.
