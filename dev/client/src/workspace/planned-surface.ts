export interface PlannedWorkspaceSurface {
  readonly name: string
  readonly expectedSdkSurface: readonly string[]
  readonly expectedCoverage: readonly string[]
}

export function reportPlannedWorkspaceSurface(surface: PlannedWorkspaceSurface): never {
  console.error(JSON.stringify({
    ok: false,
    status: "not_implemented",
    surface: surface.name,
    expectedSdkSurface: surface.expectedSdkSurface,
    expectedCoverage: surface.expectedCoverage,
  }, null, 2))
  process.exit(2)
}
