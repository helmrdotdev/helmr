import { HelmrClient } from "../src/index"
const config = await Bun.stdin.json() as {url:string; apiKey:string; sessionId:string; turnId:string; phase:string}
const client = new HelmrClient({url:config.url, apiKey:config.apiKey})
const session = client.sessions.get(config.sessionId)
const turn = session.turn(config.turnId)
const value = [{type:"text" as const,text:"steer\u0000雪"}]
const routed = await session.send(value, {idempotencyKey:"sdk-message"})
if (routed.kind !== "messaged" || routed.turn.id !== turn.id) throw new Error("Session send changed disposition")
const repeated = await session.send(value, {idempotencyKey:"sdk-message"})
if (repeated.kind !== "messaged" || repeated.message.id !== routed.message.id) throw new Error("Session send replay changed message")
const exact = await turn.send(value, {idempotencyKey:"sdk-exact"})
const again = await turn.send(value, {idempotencyKey:"sdk-exact"})
if (exact.id !== again.id || exact.status !== "accepted") throw new Error("Turn send retry changed identity")
if (config.phase === "closed") {
 try { await turn.send(value, {idempotencyKey:"sdk-closed"}); throw new Error("closed Turn accepted message") }
 catch (error) { if (!(error instanceof Error) || !("code" in error) || error.code !== "message_closed") throw error }
 const queued = await session.send(value, {idempotencyKey:"sdk-fallback"})
 const retried = await session.send(value, {idempotencyKey:"sdk-fallback"})
 if (queued.kind !== "enqueued" || queued.turn.id === turn.id || retried.kind !== "enqueued" || retried.turn.id !== queued.turn.id) throw new Error("fallback changed Turn")
}
