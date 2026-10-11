import { HelmrClient } from "../src/index"
const c = await Bun.stdin.json() as {url:string;apiKey:string;sessionId:string;turnId:string;receiptId:string}
const client = new HelmrClient({url:c.url,apiKey:c.apiKey})
const session = client.sessions.ref(c.sessionId)
const turn = session.turns.ref(c.turnId)
const receipt = await turn.interrupt({idempotencyKey:"exact-stop"})
if (receipt.id !== c.receiptId || receipt.turnId !== c.turnId) throw new Error("Exact interrupt replay changed identity")
if ((await turn.get()).status !== "interrupted" || (await session.get()).holds.length !== 0) throw new Error("Exact interrupt did not preserve Session")
