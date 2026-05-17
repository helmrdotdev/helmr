import type { WorkspaceSpec } from "../internal"

export type RunWorkspace = {
  readonly repository: string
  readonly ref: string
  readonly subpath?: string
}

export function runWorkspaceFromSpec(spec: WorkspaceSpec): RunWorkspace {
  return {
    repository: spec.repository,
    ref: spec.ref,
    ...(spec.subpath === undefined ? {} : { subpath: spec.subpath }),
  }
}
