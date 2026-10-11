import assert from "node:assert/strict"
import { test } from "node:test"
import { mkdtemp, open, readFile, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { setTimeout as delay } from "node:timers/promises"
import { turnTimingSink } from "./turn-timing"
import type { TurnTiming } from "./agent-session"

const record: TurnTiming = { sessionId: "session", turnId: "turn", sequence: 1, outcome: "completed", handlerReturnToSettlementMs: 10, handlerReturnToFinalizationMs: 3, finalizationToSettlementMs: 7 }
test("timing sink drops backpressured records and contains asynchronous descriptor failure", async () => {
  const directory = await mkdtemp(join(tmpdir(), "helmr-turn-timing-"))
  const path = join(directory, "diagnostics")
  const file = await open(path, "w")
  try {
    let accepting = false
    const sink = turnTimingSink(file.fd, () => accepting)
    sink(record)
    await delay(10)
    assert.equal(await readFile(path, "utf8"), "")
    accepting = true
    sink(record)
    sink({ ...record, sequence: 2 })
    for (let attempt = 0; !(await readFile(path, "utf8")).endsWith("\n"); attempt++) {
      assert(attempt < 100)
      await delay(1)
    }
    assert.deepEqual((await readFile(path, "utf8")).trim().split("\n").map(line => JSON.parse(line)), [{ source: "helmr.runtime", event: "turn.settled", ...record }])
    // A closed, nonnegative descriptor makes fs.write fail asynchronously. The
    // node:test runner also fails this test if an uncaught error is emitted.
    const closedFD = file.fd
    await file.close()
    const failed = turnTimingSink(closedFD, () => true)
    failed(record)
    await delay(10)
    failed(record)
    await delay(10)
  } finally { await file.close(); await rm(directory, { recursive: true, force: true }) }
})
