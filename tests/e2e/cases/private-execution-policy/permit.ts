import type { JsonValue, Turn } from "@helmr/sdk"
import { fixtureValue } from "../../support/runtime-mcp"

export async function waitForProbePermit(turn: Pick<Turn, "signal" | "onMessage" | "output">, ready: JsonValue) {
  turn.signal.throwIfAborted()
  let permit!: () => void, rejectAbort!: (reason: unknown) => void
  let active = true
  const permitted = new Promise<void>(resolve => { permit = resolve })
  const cancelled = new Promise<never>((_resolve, reject) => { rejectAbort = reject })
  const abort = () => rejectAbort(turn.signal.reason)
  turn.signal.addEventListener("abort", abort, { once: true })
  try {
    await Promise.race([(async () => {
      // The native API scopes its message registration to this Turn and exposes
      // no unsubscribe. Deactivate the retained callback when this wait settles.
      await turn.onMessage(message => { if (active && fixtureValue(message.content) === "probe") permit() })
      turn.signal.throwIfAborted()
      await turn.output.write([{ type: "json", value: ready }])
    })(), cancelled])
    await Promise.race([permitted, cancelled])
    turn.signal.throwIfAborted()
  } finally {
    active = false
    turn.signal.removeEventListener("abort", abort)
  }
}
