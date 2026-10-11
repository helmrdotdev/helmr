import { normalizeInput, type InputContent } from "../../../sdk/typescript/src/content"
import { create, fromBinary } from "@bufbuild/protobuf"
import { agentProto } from "@helmr/proto"
import type { AgentDefinition, Json, SetupContext, Turn } from "../../../sdk/typescript/src/agent"
import { requireJson } from "../../../sdk/typescript/src/agent"
import { AgentChannel, OperationError, MAX_AGENT_FRAME_BYTES } from "./agent-channel"
import { SessionActivity } from "./agent-activity"
import { SessionNativeRegistry } from "./agent-native"
import { installAgentMcp } from "./agent-mcp"
import { GuestSessionDriver } from "./agent-driver"
import { MessageBusy, SessionRuntime, type TurnTiming } from "./agent-session"
import { MessageRejected } from "../../../sdk/typescript/src/message-error"

export interface AgentProgramIO {
  readonly input: AsyncIterable<Uint8Array>
  write(frame: Uint8Array): Promise<void>
  timing?(record: TurnTiming): void
}
function json(bytes: Uint8Array): Json {
  const value: unknown = JSON.parse(Buffer.from(bytes).toString("utf8"))
  requireJson(value)
  return value
}
function safeSequence(value: bigint): number {
  if (value < 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error("Session sequence is outside the supported range")
  return Number(value)
}
function source(bytes: Uint8Array): Turn['source'] {
  const raw = json(bytes)
  if (raw === null || Array.isArray(raw) || typeof raw !== "object") throw new Error("Invalid Turn source")
  const value = raw as Record<string, Json>
  if (typeof value['kind'] !== "string") throw new Error("Invalid Turn source")
  if (value['requesterSessionId'] !== undefined && typeof value['requesterSessionId'] !== "string") throw new Error("Invalid requester Session")
  if (value['originTurnId'] !== undefined && typeof value['originTurnId'] !== "string") throw new Error("Invalid origin Turn")
  return { kind: value['kind'], ...(value['requesterSessionId'] === undefined ? {} : { requesterSessionId: value['requesterSessionId'] }), ...(value['originTurnId'] === undefined ? {} : { originTurnId: value['originTurnId'] }) }
}
function failure(error: unknown): agentProto.OperationError {
  const code = error instanceof MessageBusy ? "message_busy" : error instanceof MessageRejected ? "message_rejected" : error instanceof OperationError ? error.code : "runtime_error"
  return create(agentProto.OperationErrorSchema, { code, message: error instanceof Error ? error.message : String(error) })
}

// A Session owns one process, one setup Promise and one permanent command reader.
// Dispatch is intentionally not awaited by the reader: its own operations need
// responses from that same pipe, as do incoming messages and interruption.
export type AgentDefinitionLoader = (agentId: string) => Promise<AgentDefinition<InputContent, Json, unknown>>

export async function runAgentProgram(definitionSource: AgentDefinition<InputContent, Json, unknown> | AgentDefinitionLoader, io: AgentProgramIO): Promise<void> {
  const stopped = new AbortController()
  const failureConvergenceMs = 10_000
  const activity = new SessionActivity()
  const native = new SessionNativeRegistry(stopped.signal, failureConvergenceMs, undefined, activity)
  const uninstallNative = native.install()
  let uninstallMcp: (() => void) | undefined
  let channel: AgentChannel | undefined
  let runtime: SessionRuntime | undefined
  let initialized = false
  let shutdown = false
  let controlSequence = 0n
  const tasks = new Set<Promise<void>>()
  const observe = (pending: Promise<void>) => {
    tasks.add(pending)
    void pending.finally(() => tasks.delete(pending)).catch(error => { stopped.abort(error); channel?.close(error) })
  }
  try {
    for await (const command of readCommands(io.input, stopped.signal)) {
      if (stopped.signal.aborted) break
      if (!channel) {
        if (command.command.case !== "start") throw new Error("First guest command must start a Session")
        const start = command.command.value
        if (command.controlSequence < 0n) throw new Error("Invalid initial control sequence")
        controlSequence = command.controlSequence
        if (!command.identity || !start.agentId || !start.computerId || !start.deploymentId) throw new Error("Invalid Session start identity")
        channel = new AgentChannel(command.identity, frame => io.write(frame), error => stopped.abort(error))
        let definition: AgentDefinition<InputContent, Json, unknown>
        try {
          definition = typeof definitionSource === "function" ? await activity.authored(() => definitionSource(start.agentId)) : definitionSource
          if (start.agentId !== definition.id) throw new Error("Agent bundle does not match Session start")
        } catch (error) {
          const { code, message } = failure(error)
          await channel.event({ case: "failed", value: create(agentProto.SessionFailedSchema, { code, message }) })
          return
        }
        uninstallMcp = installAgentMcp(channel)
        const driver = new GuestSessionDriver(channel, native)
        const kind = start.recoveryKind === agentProto.SessionStart_RecoveryKind.INITIAL ? "initial" : start.recoveryKind === agentProto.SessionStart_RecoveryKind.RECONSTRUCTED ? "reconstructed" : undefined
        if (!kind) throw new Error("Invalid Session recovery kind")
        const context: SetupContext = {
          session: { id: command.identity.sessionId, ...(start.conversationKey === undefined ? {} : { key: start.conversationKey }), ...(start.parentSessionId === undefined ? {} : { parent: Object.freeze({ id: start.parentSessionId }) }) },
          computer: { id: start.computerId }, deployment: { id: start.deploymentId }, recovery: { kind, ...(start.recoveryReason ? { reason: start.recoveryReason } : {}) }, signal: stopped.signal,
        }
        runtime = new SessionRuntime(definition, context, driver, { failureConvergenceMs, terminalSequence: safeSequence(start.terminalSequence), native, activity, ...(io.timing ? { onTiming: io.timing } : {}) })
        const activeChannel = channel
        observe(runtime.initialize().then(async () => {
          initialized = true
          await activeChannel.event({ case: "ready", value: create(agentProto.SessionReadySchema) })
        }, async error => {
          const { code, message } = failure(error)
          await activeChannel.event({ case: "failed", value: create(agentProto.SessionFailedSchema, { code, message }) })
        }))
        continue
      }
      if (!channel.matches(command.identity)) throw new Error("Guest command belongs to another Session process")
      if (command.command.case === "operationResult") { channel.receive(command.identity, command.command.value); continue }
      if (command.command.case === "start") throw new Error("Session already started")
      if (command.command.case === "checkpoint") {
        const checkpointId = command.command.value.checkpointId
        // The supervisor has sealed admission and drained earlier local writes.
        // Inspect native resources without manufacturing a public delivery
        // receipt. The physical owner fences termination and broken pipes.
        const activeChannel = channel
        observe((async () => {
          let outcome: agentProto.SessionCheckpointReady["outcome"]
          try {
            if (!checkpointId || !initialized || !runtime!.canCheckpoint || shutdown) throw new Error("Session is not ready for coherent capture")
            // Handle destruction is reported on the next loop turn.
            await new Promise<void>(resolve => setImmediate(resolve))
            const evidence = await native.evidence()
            if (!evidence.reusable || !runtime!.canCheckpoint) throw new Error("Session continuation is not reusable")
            activity.assertIdle()
            outcome = { case: "scopesJson", value: Buffer.from(JSON.stringify(evidence.scopes)) }
          } catch (error) { outcome = { case: "error", value: failure(error) } }
          await activeChannel.event({ case: "checkpointReady", value: create(agentProto.SessionCheckpointReadySchema, { checkpointId, outcome }) })
        })())
        continue
      }
      if (!command.deliveryId) throw new Error("Guest command delivery identity is required")
      const current = runtime!
      const activeChannel = channel
      const run = async (): Promise<Json> => {
        const control = command.command.case === "interrupt" || command.command.case === "suspend" || command.command.case === "resume"
        if (control) {
          if (command.controlSequence <= 0n) throw new Error("Control sequence is required")
          if (command.controlSequence <= controlSequence) return { superseded: true }
        }
        switch (command.command.case) {
          case "dispatch": {
            if (!initialized) throw new Error("Session setup has not completed")
            const turn = command.command.value
            return await current.dispatch({ id: turn.turnId, sequence: safeSequence(turn.sequence), createdAt: turn.createdAt, input: normalizeInput(json(turn.inputJson)), source: source(turn.sourceJson) }) as unknown as Json
          }
          case "message": {
            const message = command.command.value
            await current.deliverMessage(message.turnId, message.messageId, normalizeInput(json(message.inputJson))); return null
          }
          case "interrupt": current.interrupt(command.command.value.turnId, new Error(command.command.value.reason)); controlSequence = command.controlSequence; return null
          case "suspend": current.suspend(new Error(command.command.value.reason)); controlSequence = command.controlSequence; return null
          case "reconcile": return await current.reconcileFinalization(command.command.value.turnId) as unknown as Json
          case "acknowledged": current.acknowledgeTerminal(command.command.value.turnId, safeSequence(command.command.value.sequence)); return null
          case "resume":
            controlSequence = command.controlSequence
            await current.waitForResume()
            if (controlSequence !== command.controlSequence) return { superseded: true }
            current.resume(); return null
          case "shutdown": shutdown = true; stopped.abort(new Error(command.command.value.reason)); return null
          default: throw new Error("Unknown guest command")
        }
      }
      const delivery = run().then(
        value => activeChannel.event({ case: "deliveryResult", value: create(agentProto.DeliveryResultSchema, { deliveryId: command.deliveryId, outcome: { case: "valueJson", value: Buffer.from(JSON.stringify(value)) } }) }),
        error => activeChannel.event({ case: "deliveryResult", value: create(agentProto.DeliveryResultSchema, { deliveryId: command.deliveryId, outcome: { case: "error", value: failure(error) } }) }),
      )
      observe(delivery)
      if (command.command.case === "shutdown") { await delivery; break }
    }
    if (!channel) throw new Error("Guest pipe closed before Session start")
    if (!shutdown) throw stopped.signal.reason ?? new Error("Guest pipe closed unexpectedly")
  } finally {
    const error = stopped.signal.reason ?? new Error("Guest pipe closed")
    stopped.abort(error)
    channel?.close(error)
    uninstallMcp?.()
    uninstallNative()
    activity.close()
    void native.stop(error).catch(() => {})
    // Uncooperative authored tasks are fenced by the guest process owner. Never
    // report a clean shutdown merely because their JavaScript promises hung.
  }
}

async function* readCommands(input: AsyncIterable<Uint8Array>, signal: AbortSignal): AsyncGenerator<agentProto.GuestCommand> {
  const iterator = input[Symbol.asyncIterator]()
  const header = Buffer.alloc(4)
  let body: Buffer | undefined
  let filled = 0
  try {
    for (;;) {
      const next = await new Promise<IteratorResult<Uint8Array>>((resolve, reject) => {
        const abort = () => reject(signal.reason)
        signal.addEventListener("abort", abort, { once: true })
        Promise.resolve().then(() => { signal.throwIfAborted(); return iterator.next() }).then(resolve, reject).finally(() => signal.removeEventListener("abort", abort))
        if (signal.aborted) abort()
      })
      if (next.done) break
      const chunk = next.value
      let offset = 0
      while (offset < chunk.length) {
        const destination = body ?? header
        const count = Math.min(destination.length - filled, chunk.length - offset)
        destination.set(chunk.subarray(offset, offset + count), filled)
        filled += count; offset += count
        if (filled !== destination.length) continue
        filled = 0
        if (!body) {
          const length = header.readUInt32BE(0)
          if (!length || length > MAX_AGENT_FRAME_BYTES) throw new Error("Invalid guest frame length")
          body = Buffer.allocUnsafe(length)
        } else {
          yield fromBinary(agentProto.GuestCommandSchema, body)
          body = undefined
        }
      }
    }
    if (filled || body) throw new Error("Truncated guest frame")
  } finally {
    // A failed output pipe must not wait forever for a still-open input pipe.
    void Promise.resolve().then(() => iterator.return?.()).catch(() => {})
  }
}
