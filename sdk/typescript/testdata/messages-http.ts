import { HelmrClient } from "../src/index"
const config = await Bun.stdin.json() as {url:string; apiKey:string; sessionId:string; turnId:string; phase:string}
const client = new HelmrClient({url:config.url, apiKey:config.apiKey})
const session = client.sessions.ref(config.sessionId)
const turn = session.turns.ref(config.turnId)
const value = [{type:"text" as const,text:"steer\u0000雪"}]
const routed = await turn.send(value, {idempotencyKey:"sdk-message"})
if (routed.status !== "accepted" || !routed.id) throw new Error("Turn send did not return a message receipt")
const repeated = await turn.send(value, {idempotencyKey:"sdk-message"})
if (repeated.id !== routed.id) throw new Error("Turn send replay changed message")
const exact = await turn.send(value, {idempotencyKey:"sdk-exact"})
const again = await turn.send(value, {idempotencyKey:"sdk-exact"})
if (exact.id !== again.id || exact.status !== "accepted") throw new Error("Turn send retry changed identity")
if (config.phase === "closed") {
 try { await turn.send(value, {idempotencyKey:"sdk-closed"}); throw new Error("closed Turn accepted message") }
 catch (error) { if (!(error instanceof Error) || !("code" in error) || error.code !== "message_closed") throw error }
 const queued = await session.enqueue(value, {idempotencyKey:"sdk-fallback"})
 const retried = await session.enqueue(value, {idempotencyKey:"sdk-fallback"})
 if (queued.id === turn.id || retried.id !== queued.id) throw new Error("fallback changed Turn")
}
