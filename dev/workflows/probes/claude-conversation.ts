// Real pinned SDK and native processes; model responses are loopback fixtures.
// This does not exercise Helmr runtime delivery, VM restore or actual inference.
import { createServer } from "node:http"
import { mkdtemp, rm, stat } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { randomUUID } from "node:crypto"
import assert from "node:assert/strict"
import { query } from "@anthropic-ai/claude-agent-sdk"
import { spawn } from "node:child_process"

const cwd = await mkdtemp(join(tmpdir(), "helmr-claude-native-"))
const requests: any[] = []
const http = createServer(async (req, res) => {
  const chunks: Buffer[] = []
  for await (const chunk of req) chunks.push(Buffer.from(chunk))
  const body = JSON.parse(Buffer.concat(chunks).toString() || "{}")
  if (!req.url?.startsWith("/v1/messages")) { res.writeHead(404); res.end(); return }
  if (req.url.includes("count_tokens")) { res.setHeader("content-type", "application/json"); res.end(JSON.stringify({input_tokens: 1})); return }
  requests.push(body)
  const tool = requests.length === 1 ? {id: "toolu_question", name: "AskUserQuestion", input: {questions: [{question: "Which option?", header: "Choice", multiSelect: false, options: [{label: "A", description: "First"}, {label: "B", description: "Second"}]}]}}
    : requests.length === 2 ? {id: "toolu_command", name: "Bash", input: {command: "printf fixture > denied-command-marker", description: "Write a disposable test marker"}} : undefined
  res.writeHead(200, {"content-type": "text/event-stream"})
  for (const event of [
    {type: "message_start", message: {id: "msg_" + requests.length, type: "message", role: "assistant", content: [], model: body.model, stop_reason: null, stop_sequence: null, usage: {input_tokens: 1, output_tokens: 0}}},
    {type: "content_block_start", index: 0, content_block: tool ? {type: "tool_use", id: tool.id, name: tool.name, input: {}} : {type: "text", text: ""}},
    {type: "content_block_delta", index: 0, delta: tool ? {type: "input_json_delta", partial_json: JSON.stringify(tool.input)} : {type: "text_delta", text: "native fixture response"}},
    {type: "content_block_stop", index: 0},
    {type: "message_delta", delta: {stop_reason: tool ? "tool_use" : "end_turn", stop_sequence: null}, usage: {output_tokens: 3}},
    {type: "message_stop"},
  ]) res.write(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`)
  res.end()
})
await new Promise<void>(resolve => http.listen(0, "127.0.0.1", resolve))
const port = (http.address() as {port:number}).port
const nativeEnv: Record<string, string | undefined> = Object.fromEntries(Object.keys(process.env).map(key => [key, undefined]))
Object.assign(nativeEnv, {PATH: process.env.PATH, HOME: process.env.HOME, CLAUDE_CONFIG_DIR: cwd, ANTHROPIC_BASE_URL: `http://127.0.0.1:${port}`, ANTHROPIC_API_KEY: "local-fixture", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", DISABLE_TELEMETRY: "1"})
const nativeId = randomUUID()
const cancel = new AbortController()
const timer = setTimeout(() => cancel.abort(), 45000)
let native: ReturnType<typeof query> | undefined
let nativeExited: Promise<void> = Promise.resolve()
let questions = 0
let approvals = 0
let endPrompt: (() => void) | undefined
try {
  for (let round = 0; round < 2; round++) {
    const done = new Promise<void>(resolve => { endPrompt = resolve })
    const input = async function* () {
      yield {type: "user" as const, session_id: "", parent_tool_use_id: null, message: {role: "user" as const, content: round ? "second unique input" : "first unique input"}}
      await done
    }
    native = query({prompt: input(), options: {
      permissionMode: "default",
      async canUseTool(name, input) {
        if (name === "AskUserQuestion") {
          questions++
          return {behavior: "allow", updatedInput: {...input, answers: {"Which option?": "A"}}}
        }
        assert.equal(name, "Bash")
        approvals++
        return {behavior: "deny", message: "Fixture user declined this command"}
      },
      spawnClaudeCodeProcess(options) {
        const child = spawn(options.command, options.args, {cwd: options.cwd, env: options.env, signal: options.signal, stdio: ["pipe", "pipe", "pipe"]})
        child.stderr.on("data", chunk => process.stderr.write(chunk))
        nativeExited = new Promise(resolve => child.once("close", () => resolve()))
        return child
      },
      cwd, settingSources: [], persistSession: true, abortController: cancel,
      ...(round ? {resume: nativeId} : {sessionId: nativeId}),
      env: nativeEnv,
    }})
    let completed = false
    for await (const message of native) {
      if (message.type === "result") {
        assert.equal(message.subtype, "success")
        assert.equal(message.session_id, nativeId)
        completed = true
        endPrompt?.()
      }
    }
    assert.ok(completed)
    native.close(); await nativeExited; native = undefined
    console.log(`native round ${round+1} completed`)
  }
  assert.equal(questions, 1)
  assert.equal(approvals, 1)
  assert.ok(requests.length >= 4)
  const blocks = requests.at(-1).messages.flatMap((message: any) => Array.isArray(message.content) ? message.content : [])
  const answered = blocks.find((block: any) => block.type === "tool_result" && block.tool_use_id === "toolu_question")
  assert.ok(answered)
  assert.equal(typeof answered.content, "string")
  assert.ok(answered.content.includes('"Which option?"="A"'))
  await assert.rejects(stat(join(cwd, "denied-command-marker")), {code: "ENOENT"})
  const resumed = JSON.stringify(requests.at(-1).messages)
  for (const value of ["first unique input", "native fixture response", "second unique input", "Which option?", "Fixture user declined this command"]) assert.ok(resumed.includes(value), value)
  console.log("PASS: real Claude SDK question, approval denial and fresh-process conversation continuation")
} finally {
  clearTimeout(timer); cancel.abort(); endPrompt?.(); native?.close(); await nativeExited
  http.closeAllConnections(); await new Promise<void>(resolve => http.close(() => resolve()))
  await rm(cwd, {recursive:true,force:true})
}
