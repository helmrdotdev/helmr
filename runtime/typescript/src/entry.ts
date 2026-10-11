import { createWriteStream } from "node:fs"
import { loadAgentBundle, loadComputerBundle } from "./definition-index"
import { runAgentProgram } from "./agent-program"
import { runComputerPreparation } from "./computer-preparation"
import { turnTimingSink } from "./turn-timing"

const index = new URL("file:///opt/helmr/program/helmr/definition-index.json")
if (process.argv[2] === "prepare") {
  const computerId = process.argv[3]
  if (!computerId || process.argv.length !== 4) throw new Error("Preparation requires one Computer definition")
  const cancellation = new AbortController()
  const stop = () => cancellation.abort()
  process.once("SIGTERM", stop)
  process.once("SIGINT", stop)
  try {
    await runComputerPreparation(await loadComputerBundle(index, computerId), cancellation.signal)
  } finally {
    process.removeListener("SIGTERM", stop)
    process.removeListener("SIGINT", stop)
  }
} else {
  // stdout/stderr carry internal process diagnostics. Only this descriptor carries framed
  // Session protocol; it remains open across coherent Computer continuation.
  const control = createWriteStream("", { fd: 3, autoClose: false })
  await runAgentProgram(
    agentId => loadAgentBundle(index, agentId),
    { input: process.stdin, write: frame => new Promise<void>((resolve, reject) => {
      control.write(frame, error => error ? reject(error) : resolve())
    }), timing: turnTimingSink(2, () => !process.stderr.writableNeedDrain && !process.stderr.destroyed) },
  )
  control.end()
}
