import { SandboxBuilderImpl, type SandboxBuilder, type SandboxNetwork } from "./internal"

export const sandbox = (id: string): SandboxBuilder => new SandboxBuilderImpl(id)

export type { SandboxBuilder, SandboxNetwork }
