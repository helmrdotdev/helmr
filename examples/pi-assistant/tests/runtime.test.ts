import assert from "node:assert/strict"
import { test } from "node:test"
import { cp, mkdtemp, mkdir, readFile, rm } from "node:fs/promises"
import { join, resolve } from "node:path"
import { tmpdir } from "node:os"
import { createAssistantMessageEventStream, type AssistantMessage } from "@earendil-works/pi-ai"
import { DefaultResourceLoader, ModelRuntime, SettingsManager } from "@earendil-works/pi-coding-agent"
import { createAnalysisAssistant } from "../agents/assistant"
import { introduction } from "../agents/settings"
import type { PiSession } from "../agents/pi-bridge"
import { exampleSession, delay, until } from "../../../tests/fixtures/example-session"

const shellQuote = (text: string) => "'" + text.replaceAll("'", "'\\''") + "'"

// Scripted model output exercises real native file/tool execution. It is not a
// claim that a paid model will generate this program or draw correct conclusions.
const analysis = `import { readFileSync, writeFileSync } from 'node:fs';
const rows = readFileSync('data/sales.csv', 'utf8').trim().split('\\n').slice(1).map(line => {
  const [month, product, region, revenue] = line.split(',');
  return { month, product, region, revenue: Number(revenue) };
});
const total = month => rows.filter(row => row.month === month).reduce((sum, row) => sum + row.revenue, 0);
const august = total('2026-08'), september = total('2026-09');
writeFileSync('report.md', '# Synthetic sales comparison\\n\\nAugust: $' + august + '\\nSeptember: $' + september + '\\nChange: $' + (september - august) + '\\n\\nRevenue alone does not explain the cause.\\n');
console.log(JSON.stringify({ august, september, change: september - august }));`

test("analysis example runs a reproducible report and follows up in the same native session", { timeout: 90_000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-analysis-")), cwd = join(root, "workspace"), agentDir = join(root, "config")
  await mkdir(cwd)
  await cp(resolve("data"), join(cwd, "data"), { recursive: true })
  const calls: string[] = [], commands: string[] = [], releases: Array<() => void> = []
  let hold = false, modelError = false
  const modelRuntime = await ModelRuntime.create({ authPath: join(root, "auth.json"), modelsPath: null, modelsStorePath: join(root, "models.json"), allowModelNetwork: false, refreshOnCreate: false })
  modelRuntime.registerProvider("fixture", {
    baseUrl: "https://invalid.example", api: "fixture-api", apiKey: "offline",
    models: [{ id: "probe", name: "Synthetic", reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 100000, maxTokens: 1000 }],
    streamSimple(model, context, options) {
      calls.push(JSON.stringify(context))
      const stream = createAssistantMessageEventStream()
      const command = commands.shift()
      const message: AssistantMessage = {
        role: "assistant", content: command ? [{ type: "toolCall", id: crypto.randomUUID(), name: "bash", arguments: { command } }] : [{ type: "text", text: "Read report.md and rerun node analysis.mjs to check the totals." }],
        api: model.api, provider: model.provider, model: model.id,
        usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } },
        stopReason: modelError ? "error" : command ? "toolUse" : "stop", timestamp: Date.now(),
      }
      let ended = false
      const finish = (aborted = false) => {
        if (ended) return; ended = true
        if (aborted || modelError) { message.stopReason = aborted ? "aborted" : "error"; message.errorMessage = "Synthetic failure"; stream.push({ type: "error", reason: message.stopReason, error: message }) }
        else stream.push({ type: "done", reason: message.stopReason as "stop" | "toolUse", message })
        stream.end(message)
      }
      if (options?.signal?.aborted) finish(true)
      else { options?.signal?.addEventListener("abort", () => finish(true), { once: true }); if (hold) releases.push(() => finish()); else queueMicrotask(() => finish()) }
      return stream
    },
  })
  const settingsManager = SettingsManager.inMemory({ compaction: { enabled: false }, retry: { enabled: false }, cacheWarming: "off" })
  const resourceLoader = new DefaultResourceLoader({
    cwd, agentDir, settingsManager, noExtensions: true, noSkills: true, noThemes: true, noContextFiles: true,
    appendSystemPrompt: [introduction],
    extensionFactories: [pi => { pi.registerCommand("clarify", { description: "Confirm the comparison", handler: async (_, ctx) => { await ctx.ui.confirm("Comparison", "Compare September with August?") } }) }],
  })
  await resourceLoader.reload()
  const application = createAnalysisAssistant({ cwd, stateDirectory: join(root, "state"), model: "fixture/probe", native: { agentDir, modelRuntime, settingsManager, resourceLoader } })
  let session = exampleSession(application), bridge: PiSession | undefined
  try {
    bridge = (await session.runtime.initialize()).initResult as PiSession
    const writeScript = `require('node:fs').writeFileSync('analysis.mjs', ${JSON.stringify(analysis)})`
    commands.push(`node -e ${shellQuote(writeScript)} && node analysis.mjs`)
    assert.equal((await session.dispatch("Compare September sales with August").result).status, "completed")
    assert.equal(await readFile(join(cwd, "analysis.mjs"), "utf8"), analysis)
    const report = await readFile(join(cwd, "report.md"), "utf8")
    assert.match(report, /August: \$3000/); assert.match(report, /September: \$2300/); assert.match(report, /Change: \$-700/)
    assert.match(report, /does not explain the cause/)
    assert.ok(calls.some(call => call.includes('"change":-700') || call.includes('\\"change\\":-700')), "the model receives actual command results")

    const breakdown = `const fs = require('node:fs');
const rows = fs.readFileSync('data/sales.csv', 'utf8').trim().split('\\n').slice(1).map(line => line.split(','));
const total = (month, product) => rows.filter(row => row[0] === month && row[1] === product).reduce((sum, row) => sum + Number(row[3]), 0);
fs.appendFileSync('report.md', '\\n' + ['Atlas', 'Beacon'].map(product => product + ' change: $' + (total('2026-09', product) - total('2026-08', product))).join('\\n') + '\\n');`
    commands.push(`node analysis.mjs && node -e ${shellQuote(breakdown)}`)
    assert.equal((await session.dispatch("Break the change down by product").result).status, "completed")
    assert.match(await readFile(join(cwd, "report.md"), "utf8"), /Atlas change: \$-800/)
    assert.ok(calls.at(-1)!.includes("Compare September sales with August"))
    assert.equal((await session.dispatch("/clarify").result).status, "completed")
    assert.equal(session.questions.length, 1)

    hold = true
    const steering = session.dispatch("Wait for another comparison detail")
    await until(() => releases.length > 0)
    await session.runtime.deliverMessage(steering.id, "detail", [{ type: "text", text: "Also show regional totals" }], { kind: "api_key" })
    hold = false; releases.splice(0).forEach(release => release())
    assert.equal((await steering.result).status, "completed")
    assert.ok(calls.at(-1)!.includes("Also show regional totals"))

    const answer = session.controls.answer, responsesBeforeCancel = session.responses.length
    session.controls.answer = async (_question, signal) => new Promise((_, reject) => { signal.addEventListener("abort", () => reject(signal.reason), { once: true }) })
    const waiting = session.dispatch("/clarify")
    await until(() => session.questions.length === 2)
    await session.runtime.interrupt(waiting.id, new Error("cancel clarification"))
    assert.equal((await waiting.result).status, "interrupted")
    assert.equal(session.responses.length, responsesBeforeCancel)
    session.controls.answer = answer
    assert.equal((await session.dispatch("Continue after cancellation").result).status, "completed")

    modelError = true
    const responsesBeforeFailure = session.responses.length
    assert.equal((await session.dispatch("Exercise a model failure").result).status, "failed")
    assert.equal(session.responses.length, responsesBeforeFailure)
    modelError = false
    await bridge.close()
    const count = calls.length
    session = exampleSession(application, "reconstructed")
    bridge = (await session.runtime.initialize()).initResult as PiSession
    await delay(50)
    assert.equal(calls.length, count, "reconstruction does not replay input")
    assert.equal((await session.dispatch("Continue from the saved report").result).status, "completed")
    assert.ok(calls.at(-1)!.includes("Compare September sales with August"))
    await bridge.close(); bridge = undefined
    const manifest = JSON.parse(await readFile(join(root, "state", "example", "session.json"), "utf8")) as { file: string }
    await rm(manifest.file)
    await assert.rejects(exampleSession(application, "reconstructed").runtime.initialize(), /ENOENT/)
  } finally { await bridge?.close(); await rm(root, { recursive: true, force: true }) }
})
