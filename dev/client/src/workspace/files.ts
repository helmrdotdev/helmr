import { reportPlannedWorkspaceSurface } from "./planned-surface"

reportPlannedWorkspaceSurface({
  name: "workspace-files",
  expectedSdkSurface: [
    "workspace.files.read",
    "workspace.files.write",
    "workspace.files.list",
    "workspace.files.stat",
  ],
  expectedCoverage: [
    "read persisted files without materializing a VM",
    "list persisted files without materializing a VM",
    "stat persisted files without materializing a VM",
    "write files through the filesystem primitive, not exec",
    "reject path traversal",
    "allow public token only for narrow file reads",
  ],
})
