// Test-only Worker transport. Physical execution/capture remain Go fixtures.
import { randomUUIDv7 } from "node:crypto"
import { create, fromBinary, toBinary } from "@bufbuild/protobuf"
import { programProto } from "../../../proto/typescript/src/index"
import { runProgram } from "../src/program"
import type { PassThrough } from "node:stream"

export function frame(schema: Parameters<typeof create>[0], value: any) {
  const body = toBinary(schema, create(schema, value)), result = Buffer.alloc(body.length + 4)
  result.writeUInt32BE(body.length); result.set(body, 4); return result
}

export function runNativeProgram(bridge: string, config: any, definition: any, input: PassThrough, hooks: {
  received?(result: any): Promise<void>
  ready?(): void
  outcome(value: programProto.ActorOutcome): void | Promise<void>
  beforeSettle(value: any): Promise<void>
  settled(): Promise<void>
}) {
    const worker = async (operation: string, body: any) => {
      const response = await fetch(`${bridge}/worker/${operation}`, {
        method: "POST", headers: { "content-type": "application/json" },
        body: JSON.stringify({ lease: config.lease, ...body }),
      })
      if (!response.ok) throw new Error(`${operation}: ${response.status} ${await response.text()}`)
      return await response.json() as any
    }
    let stopSent = false
    const beforeRejection = async (failure: any) => {
      if (stopSent || !["turn_stopping", "session_held", "session_stopped"].includes(failure.code)) return
      const control = await worker("control", { correlation_id: randomUUIDv7(), run_generation: config.runGeneration })
      if (!control.hold_id) throw new Error("stopped operation has no exact Session stop authority")
      input.write(frame(programProto.ResumeDecisionSchema, { kind: "session_stop", dataJson: JSON.stringify({
        execution: { session_id: config.sessionId, run_id: config.runId, attempt_number: 1, run_generation: config.runGeneration },
        turn_id: control.turn_id, hold_id: control.hold_id, reason: control.reason,
      }) }))
      stopSent = true
    }
    input.write(frame(programProto.ProgramStartSchema, {
      entrypointDeclaredId: definition.id, runId: config.runId, attemptNumber: 1,
      deploymentId: config.deploymentId, deploymentVersion: "v1", workspaceId: config.workspaceId,
      baseWorkspaceVersionId: config.baseWorkspaceVersionId, cause: { kind: { case: "actorStart", value: {} } },
      entrypoint: { case: "actor", value: { sessionId: config.sessionId, startInputSequence: BigInt(config.startInputSequence ?? 0),
        inputHighWatermark: BigInt(config.inputHighWatermark ?? 1), runGeneration: BigInt(config.runGeneration) } },
    }))
    input.write(frame(programProto.EntrypointReleaseSchema, { runId: config.runId, attemptNumber: 1,
      entrypoint: { declaredId: definition.id, kind: { case: "actor", value: {} } } }))
    return runProgram(new URL("file:///opt/program/declarations.json"), {
      input, importModule: async () => ({ definition }),
      readLocator: async () => JSON.stringify({ formatVersion: 0, runtimeContract: "helmr.runtime.v0",
        architecture: "x86_64", configResultDigest: `sha256:${"4".repeat(64)}`, queues: [],
        declarations: [{ kind: "actor", declaredId: definition.id, manifest: {},
          locator: { exportName: "definition", sourcePath: "main.ts", slot: "handler" } }] }),
      write: async data => {
        const event = fromBinary(programProto.RunEventSchema, data.subarray(4)).event
        if (event.case === "entrypointReady" || event.case === "resumeConsumed") return
        if (event.case === "actorOutcome") { await hooks.outcome(event.value); return }
        const value = event.value as any
        const reply = (data: unknown = {}, kind = "completed") => input.write(frame(programProto.ResumeDecisionSchema, {
          correlationId: value.correlationId, runWaitId: value.runWaitId ?? "", resumeAttachId: value.resumeAttachId ?? "",
          kind, dataJson: JSON.stringify(data),
        }))
        const scope = { correlation_id: value.correlationId, turn_id: value.execution?.turnId,
          run_generation: config.runGeneration }
        if (event.case === "runWaitRequested" && value.kind === "actor_input") {
          const result = await worker("receive", { correlation_id: value.correlationId,
            run_wait_id: value.runWaitId, resume_attach_id: value.resumeAttachId, kind: "actor_input",
            params: JSON.parse(value.paramsJson), actor_speculative_input_sequence: Number(value.actorSpeculativeInputSequence) })
          if (!result.resolution_kind) throw new Error("Qualification expected an immediately resolved input wait")
          await hooks.received?.(result)
          reply(result.resolution, result.resolution_kind); return
        }
        const operation = {
          turnReadyRequested: "ready", turnSettlementBeginRequested: "settling",
          turnMessageClaimRequested: "claim", turnMessageCompleteRequested: "handled",
        }[event.case as string]
        if (operation) {
          const result = await worker(operation, { ...scope,
            ...(value.deliveryId ? { delivery_id: value.deliveryId } : {}),
            ...(value.messageId ? { message_id: value.messageId, status: value.status, code: value.code,
              ...(value.detailsJson ? { details: JSON.parse(value.detailsJson) } : {}) } : {}) })
          if (operation === "ready") hooks.ready?.()
          if (result.failed) { await beforeRejection(result.failed); reply(result.failed, "failed") }
          else reply(operation === "claim" ? { delivery: result.delivery } : {})
          return
        }
        if (event.case === "turnOutputWriteRequested") {
          const result = await worker("output", { ...scope, data: JSON.parse(value.dataJson),
            idempotency_key: value.idempotencyKey, ...(value.messageDeliveryId ? { message_delivery_id: value.messageDeliveryId } : {}) })
          if (result.failed) { await beforeRejection(result.failed); reply(result.failed, "failed"); return }
          const output = result.completed
          reply({ id: output.id, sequence: output.sequence, session_id: output.session_id,
            turn_id: output.turn_id, run_id: output.provenance.run_id,
            attempt_number: output.provenance.attempt_number, run_generation: output.provenance.run_generation }); return
        }
        if (event.case === "turnSettleRequested") {
          await hooks.beforeSettle(value)
          const result = await worker("settle", { ...scope, disposition: value.disposition,
            target_input_sequence: Number(value.targetInputSequence),
            ...(value.resultJson === undefined ? {} : { result: JSON.parse(value.resultJson) }) })
          await hooks.settled()
          reply({ event_id: result.event_id, workspace_version_id: result.workspace_version_id }, "committed"); return
        }
        throw new Error(`Unexpected runtime event ${event.case}`)
      },
    })
}
