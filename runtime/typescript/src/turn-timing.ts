import { write } from "node:fs"
import type { TurnTiming } from "./agent-session"

// A diagnostic has no lifecycle authority. Keep at most one bounded write in
// flight; fs.write reports an asynchronous pipe failure to this callback rather
// than emitting an unhandled error on the authored stderr stream.
export function turnTimingSink(fd: number, accepting: () => boolean): (record: TurnTiming) => void {
  let pending = false
  return record => {
    if (pending || !accepting()) return
    const line = JSON.stringify({ source: "helmr.runtime", event: "turn.settled", ...record }) + "\n"
    pending = true
    try { write(fd, line, () => { pending = false }) }
    catch { pending = false }
  }
}
