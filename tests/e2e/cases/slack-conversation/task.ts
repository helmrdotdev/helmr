import { callMcp, waitManagedTurn, type ManagedAdmission } from "../../support/runtime-mcp"
import { agent, computer, image, type ComputerRef, type Json, type Turn } from "@helmr/sdk"
import { setTimeout as delay } from "node:timers/promises"

const modes = ["progress", "long", "text", "choice", "withdraw", "empty", "silent", "stop", "multi"] as const
type Mode = typeof modes[number]

export const conversationComputer = computer({
  id: "verification-conversation",
  image: image("verification-conversation").from("node:24-bookworm-slim").workdir("/sandbox"),
  resources: { cpu: 1, memory: "1GiB" },
})

function command(input: unknown): { mode: Mode; marker: string } {
  if (!Array.isArray(input)) throw new Error("Use text content parts: case:<mode> marker:<unique-ascii-marker>")
  const text = input.map((part: unknown) => {
    if (part === null || typeof part !== "object" || !("type" in part) || part.type !== "text" || !("text" in part) || typeof part.text !== "string") throw new Error("This fixture accepts text commands only")
    return part.text
  }).join("")
  const match = /^\s*case:([a-z]+)\s+marker:([A-Za-z0-9_-]{1,64})\s*$/.exec(text)
  if (!match || !modes.includes(match[1] as Mode)) throw new Error(`Use case:<${modes.join("|")}> marker:<unique-ascii-marker>`)
  return { mode: match[1] as Mode, marker: match[2]! }
}

async function progress(turn: Turn, speaker: string, marker: string, count: number, interval = 250, long = false) {
  for (let sequence = 1; sequence <= count; sequence++) {
    await turn.output.write([
      { type: "text", text: `${speaker} ${marker} progress ${sequence}/${count}\n${long ? "Synthetic content for continuation and ordering. ".repeat(40) : "Verification progress."}` },
      { type: "json", value: { marker, speaker, sequence, total: count, explicitNull: null } },
    ])
    await delay(interval, undefined, { signal: turn.signal })
  }
}

async function conversation(turn: Turn, speaker: string, currentComputer: ComputerRef): Promise<Json> {
  const { mode, marker } = command(turn.input)
  const result: { marker: string; speaker: string; mode: Mode; privateMarker: string; answer?: Json } = {
    marker, speaker, mode, privateMarker: `PRIVATE_RETURN_${marker}`,
  }
  if (mode === "multi") {
    if (speaker !== "Alpha" || marker.length > 59) throw new Error("Start multi with Alpha and a marker of at most 59 characters")
    await turn.output.write(`Alpha ${marker}: starting a separate Beta task on this Computer.`)
    const child = await callMcp<ManagedAdmission>(turn.signal, "start", {
      agentId: beta.id,
      computerId: currentComputer.id,
      input: [{ type: "text", text: `case:choice marker:${marker}_beta` }],
      idempotencyKey: `${turn.id}:beta`,
    })
    const response = await turn.ask({ prompt: [{ type: "text", text: `Alpha ${marker}: answer this task separately from Beta.` }], answer: { type: "text" } })
    result.answer = response.answer
    await turn.respond(`Alpha ${marker}: independent question finished.`)
    return { ...result, childSessionId: child.sessionId, childTurnId: child.turnId }
  }
  if (mode === "silent") return result
  if (mode === "empty") {
    await turn.respond([])
    return result
  }
  if (mode === "stop") {
    await turn.output.write(`${speaker} ${marker}: waiting for explicit Stop; no final response has been staged.`)
    await delay(8 * 60_000, undefined, { signal: turn.signal })
    throw new Error("Stop was not received within the fixture window")
  }
  if (mode === "withdraw") {
    await Promise.all([
      (async () => {
        const controller = new AbortController()
        const timer = setTimeout(() => controller.abort(), 15_000)
        let answered = false
        try {
          await turn.ask({ prompt: [{ type: "text", text: `${speaker} ${marker}: leave this question unanswered; it will be withdrawn.` }], answer: { type: "text" } }, { signal: AbortSignal.any([controller.signal, turn.signal]) })
          answered = true
          throw new Error("Withdrawal case was answered instead of left pending")
        } catch (error) {
          if (answered || !controller.signal.aborted || turn.signal.aborted) throw error
        } finally { clearTimeout(timer) }
      })(),
      progress(turn, speaker, marker, 12, 1500),
    ])
  } else if (mode === "text" || mode === "choice") {
    const control = mode === "choice" ? {
      type: "choice" as const, multiple: true, allowText: true,
      options: [{ id: "first", label: "First synthetic option", value: { selected: 1 } }, { id: "second", label: "Second synthetic option", value: null }],
    } : { type: mode }
    const [response] = await Promise.all([
      turn.ask({ prompt: [{ type: "text", text: `${speaker} ${marker}: submit a synthetic ${mode} answer. Use synthetic values only.` }], answer: control }),
      progress(turn, speaker, marker, 12, 1500),
    ])
    result.answer = response.answer
    await turn.output.write([{ type: "json", value: { marker, speaker, phase: "answered", responderKind: response.respondedBy.kind } }])
  } else {
    await progress(turn, speaker, marker, mode === "long" ? 24 : 3, 250, mode === "long")
  }
  await turn.respond(`${speaker} ${marker}: ${mode} finished. This response becomes visible only after successful settlement.`)
  return result
}

export const alpha = agent({
  id: "verification-alpha", computer: conversationComputer,
  maxTurnDuration: "10m", turn: (turn, ctx) => conversation(turn, "Alpha", ctx.computer),
})
export const beta = agent({
  id: "verification-beta", computer: conversationComputer,
  maxTurnDuration: "10m", turn: (turn, ctx) => conversation(turn, "Beta", ctx.computer),
})

// This front Agent delegates progress without emitting authored output itself.
export const coordinator = agent({
  id: "verification-coordinator", computer: conversationComputer, maxTurnDuration: "10m",
  turn: async (turn, ctx): Promise<Json> => {
    const { mode, marker } = command(turn.input)
    if (mode !== "progress" || marker.length > 58) throw new Error("Start the coordinator with progress and a marker of at most 58 characters")
    const child = await callMcp<ManagedAdmission>(turn.signal, "spawn", {
      agentId: beta.id,
      computerId: ctx.computer.id,
      input: [{ type: "text", text: `case:progress marker:${marker}_child` }],
      idempotencyKey: `${turn.id}:beta`,
    })
    const outcome = await waitManagedTurn(turn.signal, child.sessionId, child.turnId)
    if (outcome.status !== "completed") throw new Error(`Child ended as ${outcome.status}`)
    return { marker, privateMarker: `PRIVATE_RETURN_${marker}`, childSessionId: child.sessionId, childTurnId: child.turnId }
  },
})
