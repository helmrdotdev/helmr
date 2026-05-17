import { SandboxBuilderImpl, type SandboxBuilder } from "./internal"

export const sandbox = (id: string): SandboxBuilder => new SandboxBuilderImpl(id)

export type { SandboxBuilder }
