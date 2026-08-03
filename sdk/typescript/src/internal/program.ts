export type RuntimeArchitecture = "x86_64"

export type ProgramDeclaration =
  | Readonly<{
      kind: "task"
      declaredId: string
      slots: readonly ["handler"] | readonly ["handler", "payloadSchema"]
    }>
  | Readonly<{
      kind: "actor"
      declaredId: string
      slots: readonly ["handler"]
    }>
