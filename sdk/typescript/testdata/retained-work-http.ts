import { HelmrClient } from "../src/index"

const config = (await Bun.stdin.json()) as { url: string; apiKey: string }
const client = new HelmrClient({ url: config.url, apiKey: config.apiKey })
const request = {
  input: [{type: "text" as const, text: "start"}],
  sessionKey: "sdk-composition",
  idempotencyKey: "sdk-start",
}
const started = await client.agents.start("agent", request)
const replayStart = await client.agents.start("agent", request)
if (
  !started.created ||
  started.session.id !== replayStart.session.id ||
  started.turn.id !== replayStart.turn.id
)
  throw new Error("SDK start replay changed identity")
const session = started.session
const input = [{type: "text" as const, text: "retained input\u0000雪"}]
const first = await session.enqueue(input, { idempotencyKey: "sdk-retained" })
const replay = await session.enqueue(input, { idempotencyKey: "sdk-retained" })
if (first.id !== replay.id) throw new Error("SDK replay changed Turn identity")
const retained = await first.retrieve()
if (
  retained.status !== "queued" ||
  JSON.stringify(sort(retained.input)) !== JSON.stringify(sort(input))
)
  throw new Error("SDK did not retain input")
const inspected = await session.retrieve()
if (
  inspected.id !== session.id ||
  inspected.status !== "open" ||
  !Array.isArray(inspected.holds)
)
  throw new Error("SDK Session inspection failed")
const page = await session.turns.list({ limit: 1 })
if (page.items.length !== 1 || page.nextCursor === undefined)
  throw new Error("SDK first queue page failed")
const next = await session.turns.list({ cursor: page.nextCursor, limit: 100 })
if (!next.items.some((turn) => turn.id === first.id))
  throw new Error("SDK retained queue omitted admitted Turn")
const listed = await client.sessions.list({ status: "open", limit: 100 })
if (!listed.items.some((value) => value.id === session.id))
  throw new Error("SDK Session list omitted retained Session")
const interrupted = await session.interrupt({ idempotencyKey: "sdk-interrupt" })
const repeatedInterrupt = await session.interrupt({ idempotencyKey: "sdk-interrupt" })
if (interrupted.id !== repeatedInterrupt.id || interrupted.holdId !== repeatedInterrupt.holdId ||
    interrupted.sessionId !== session.id || (await first.retrieve()).status !== "queued")
  throw new Error("SDK interrupt lost its hold receipt or retained work")
await session.resume({ holdId: interrupted.holdId, idempotencyKey: "sdk-resume" })
const releasedReplay = await session.interrupt({ idempotencyKey: "sdk-interrupt" })
if (releasedReplay.id !== interrupted.id || releasedReplay.holdId !== interrupted.holdId ||
    (await session.retrieve()).holds.some(hold => hold.id === interrupted.holdId))
  throw new Error("SDK interrupt retry recreated a released hold")
const closing = await session.close({ idempotencyKey: "sdk-close" })
const closingReplay = await session.close({ idempotencyKey: "sdk-close" })
if (closing.id !== closingReplay.id || closing.status !== "accepted" ||
    (await session.retrieve()).status !== "closing" ||
    (await first.retrieve()).status !== "queued")
  throw new Error("SDK close did not preserve admitted work and its receipt")
try {
  await session.enqueue([{type:"text", text:"after close"}], { idempotencyKey: "sdk-after-close" })
  throw new Error("SDK close accepted new work")
} catch (error) {
  if (!(error instanceof Error) || !("code" in error) || error.code !== "admission_unavailable") throw error
}
await session.cancel({ idempotencyKey: "sdk-cancel" })
const cancelled = await first.retrieve()
if (
  cancelled.status !== "cancelled" ||
  cancelled.result !== undefined ||
  cancelled.terminalAt === undefined
)
  throw new Error("SDK cancellation did not settle retained work")
const events = await session.events.list({ limit: 1 })
if (events.records.length !== 1 || !events.hasMore || events.records[0]?.kind !== "turn.queued")
  throw new Error("SDK first event page failed")
const later = await session.events.list({ after: events.nextAfter, limit: 1000 })
if (!later.records.some((event) => event.kind === "turn.cancelled" && event.turnId === first.id) || later.hasMore)
  throw new Error("SDK event continuation omitted cancellation")
const empty = await session.events.list({ after: later.nextAfter })
if (empty.records.length !== 0 || empty.nextAfter !== later.nextAfter)
  throw new Error("SDK empty event page lost cursor")
try {
  await session.events.list({ after: later.nextAfter + 1 })
  throw new Error("SDK accepted future event cursor")
} catch (error) {
  if (!(error instanceof Error) || !("code" in error) || error.code !== "invalid_cursor") throw error
}
console.log("SDK HTTP retained-work composition passed")

function sort(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sort)
  if (value !== null && typeof value === "object")
    return Object.fromEntries(
      Object.entries(value)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([key, value]) => [key, sort(value)]),
    )
  return value
}
