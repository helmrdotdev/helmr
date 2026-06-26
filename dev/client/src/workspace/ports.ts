import { reportPlannedWorkspaceSurface } from "./planned-surface"

reportPlannedWorkspaceSurface({
  name: "workspace-ports",
  expectedSdkSurface: [
    "workspace.ports.open",
    "workspace.ports.list",
    "workspace.ports.close",
    "workspace.ports.publicToken",
  ],
  expectedCoverage: [
    "open a port from a materialized workspace",
    "read from a port through a narrow public token",
    "list active ports without session ownership",
    "close a port and revoke active public access",
    "reject exec, PTY, shell, and file writes through public tokens",
  ],
})
