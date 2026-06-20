import { reportPlannedWorkspaceSurface } from "./planned-surface"

reportPlannedWorkspaceSurface({
  name: "workspace-pty",
  expectedSdkSurface: [
    "workspace.pty.open",
    "workspace.pty.write",
    "workspace.pty.resize",
    "workspace.pty.close",
  ],
  expectedCoverage: [
    "open PTY without creating a task session",
    "stream PTY output from a materialized workspace",
    "write input and observe shell state",
    "resize terminal dimensions",
    "close PTY and report terminal state",
    "reject PTY when only a public token is supplied",
  ],
})
