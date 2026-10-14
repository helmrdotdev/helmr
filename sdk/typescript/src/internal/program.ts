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

/** Validated module boundary; authoring uses the stricter AgentDefinition. */
export type LoadedAgentDefinition = Omit<import("../agent").AgentDefinition<any, any, any>, "init" | "inputSchema" | "resultSchema"> & {
  readonly init?: (context: import("../agent").InitContext) => unknown
  readonly inputSchema?: import("../agent").StandardSchemaV1
  readonly resultSchema?: import("../agent").StandardSchemaV1
}
