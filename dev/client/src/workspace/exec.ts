import { reportPlannedWorkspaceSurface } from "./planned-surface"

reportPlannedWorkspaceSurface({
  name: "workspace-exec",
  expectedSdkSurface: [
    "workspace.exec.create",
    "workspace.exec.wait",
    "workspace.exec.logs",
  ],
  expectedCoverage: [
    "create exec without creating a task session",
    "run command in a materialized workspace",
    "capture stdout and stderr",
    "return exit code and terminal state",
    "preserve workspace version/capture boundaries",
    "reject exec when only a public token is supplied",
  ],
})
