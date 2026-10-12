export type RuntimeArchitecture = "x86_64"
export interface DefinitionIndex {
  readonly apiVersion: "helmr.definition-index.v1"
  readonly agents: readonly {
    readonly id: string
    readonly templateDefinitionId: string
    readonly modulePath: string
    readonly exportName: string
  }[]
  readonly templates: readonly {
    readonly id: string
    readonly modulePath: string
    readonly exportName: string
    readonly throughAgent: boolean
  }[]
}
