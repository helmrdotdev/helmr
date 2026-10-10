export type RuntimeArchitecture = "x86_64"
export interface DefinitionIndex {
  readonly apiVersion: "helmr.definition-index.v1"
  readonly agents: readonly {
    readonly id: string
    readonly computerDefinitionId: string
    readonly modulePath: string
    readonly exportName: string
  }[]
  readonly computers: readonly {
    readonly id: string
    readonly modulePath: string
    readonly exportName: string
    readonly throughAgent: boolean
  }[]
}
