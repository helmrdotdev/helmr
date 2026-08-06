import type {
  SessionCloseRequest,
  SessionCloseReceipt,
  SessionInputRecord,
  SessionInputSendRequest,
  SessionOutputQuery,
  SessionOutputRecord,
  ActorStartOptions,
  Session,
  Duration,
  JsonValue,
  Metadata,
  RunHandle,
  TaskCallOptions,
  TaskResult,
  TaskStartOptions,
} from "../contract"
import type { LogAttributes, RunLogLevel } from "../logger"
import type {
  TokenCreateRequest,
  TokenCreateResult,
  TokenWaitOptions,
} from "../tokens"
import type {
  WorkspaceCreateRequest,
  WorkspaceDeleteRequest,
  WorkspaceDeleteReceipt,
  WorkspaceExecRequest,
  WorkspaceExecResult,
  WorkspaceFileEntry,
  WorkspaceFileListQuery,
  Workspace,
} from "../workspace"
import type { CursorPage } from "../contract"

const runtimeOperationsSymbol = Symbol.for("helmr.sdk.v0.runtime_operations")

export interface RuntimeOperations {
  readonly taskStart: (
    target: Readonly<{ declaredId: string; payloadPresent: boolean }>,
    payload: JsonValue | undefined,
    options: TaskStartOptions,
  ) => Promise<RunHandle>
  readonly taskCall: (
    target: Readonly<{ declaredId: string; payloadPresent: boolean }>,
    payload: JsonValue | undefined,
    options: TaskCallOptions,
  ) => Promise<TaskResult<JsonValue>>
  readonly waitFor: (duration: Duration) => Promise<void>
  readonly waitUntil: (date: Date) => Promise<void>
  readonly actorInputSend: (
    sessionId: string,
    input: JsonValue,
    request?: SessionInputSendRequest,
    signal?: AbortSignal,
  ) => Promise<SessionInputRecord>
  readonly actorStart: (
    declaredId: string,
    options: ActorStartOptions,
  ) => Promise<Readonly<{ sessionId: string; runId: string }>>
  readonly sessionRetrieve: (
    sessionId: string,
    signal?: AbortSignal,
  ) => Promise<Session>
  readonly sessionClose: (
    sessionId: string,
    request?: SessionCloseRequest,
    signal?: AbortSignal,
  ) => Promise<SessionCloseReceipt>
  readonly sessionOutputPage: (
    sessionId: string,
    query?: SessionOutputQuery,
    signal?: AbortSignal,
  ) => Promise<Readonly<{
    records: readonly SessionOutputRecord[]
    nextAfter: number
    hasMore: boolean
  }>>
  readonly workspaceCreate: (
    declaredId: string,
    request?: WorkspaceCreateRequest,
    signal?: AbortSignal,
  ) => Promise<Readonly<{ workspaceId: string }>>
  readonly workspaceRetrieve: (
    workspaceId: string,
    signal?: AbortSignal,
  ) => Promise<Workspace>
  readonly workspaceFileRead: (
    workspaceId: string,
    path: string,
    signal?: AbortSignal,
  ) => Promise<Uint8Array>
  readonly workspaceFileStat: (
    workspaceId: string,
    path: string,
    signal?: AbortSignal,
  ) => Promise<WorkspaceFileEntry>
  readonly workspaceFileList: (
    workspaceId: string,
    path: string,
    query?: WorkspaceFileListQuery,
    signal?: AbortSignal,
  ) => Promise<CursorPage<WorkspaceFileEntry>>
  readonly workspaceExec: (
    workspaceId: string,
    request: WorkspaceExecRequest,
    signal?: AbortSignal,
  ) => Promise<WorkspaceExecResult>
  readonly workspaceDelete: (
    workspaceId: string,
    request?: WorkspaceDeleteRequest,
    signal?: AbortSignal,
  ) => Promise<WorkspaceDeleteReceipt>
  readonly tokenCreate: (
    request: TokenCreateRequest,
  ) => Promise<TokenCreateResult>
  readonly tokenWait: (
    tokenId: string,
    options: TokenWaitOptions,
  ) => Promise<JsonValue>
  readonly metadataSet: (key: string, value: JsonValue) => Promise<void>
  readonly metadataPatch: (values: Metadata) => Promise<void>
  readonly metadataIncrement: (key: string, amount: number) => Promise<void>
  readonly structuredLog: (
    level: RunLogLevel,
    message: string,
    attributes: LogAttributes,
  ) => Promise<void>
}

type RuntimeGlobal = typeof globalThis & {
  [runtimeOperationsSymbol]?: RuntimeOperations
}

export function installRuntimeOperations(
  operations: RuntimeOperations,
): () => void {
  const target = globalThis as RuntimeGlobal
  if (target[runtimeOperationsSymbol] !== undefined) {
    throw new Error("Helmr runtime operations are already installed")
  }
  const installed = Object.freeze(operations)
  target[runtimeOperationsSymbol] = installed
  return () => {
    if (target[runtimeOperationsSymbol] === installed) {
      delete target[runtimeOperationsSymbol]
    }
  }
}

export function currentRuntimeOperations(): RuntimeOperations {
  const operations = (globalThis as RuntimeGlobal)[runtimeOperationsSymbol]
  if (operations === undefined) {
    throw new Error(
      "runtime operation is unavailable without the Helmr managed runtime",
    )
  }
  return operations
}

export function runtimeOperationsInstalled(): boolean {
  return (globalThis as RuntimeGlobal)[runtimeOperationsSymbol] !== undefined
}
