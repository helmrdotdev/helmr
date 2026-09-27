// Loopback model fixtures shared by native and Control Plane integration probes.
import { createServer } from "node:http"

export function claudeModel(requests: any[], interrupted = false) {
  return createServer(async (req, res) => {
    const chunks: Buffer[] = []
    for await (const chunk of req) chunks.push(Buffer.from(chunk))
    const body = JSON.parse(Buffer.concat(chunks).toString() || "{}")
    if (!req.url?.startsWith("/v1/messages")) { res.writeHead(404); res.end(); return }
    if (req.url.includes("count_tokens")) { res.setHeader("content-type", "application/json"); res.end(JSON.stringify({ input_tokens: 1 })); return }
    requests.push(body)
    const tool = requests.length === 1
      ? { id: "toolu_question", name: "AskUserQuestion", input: { questions: [{ question: "Which option?", header: "Choice", multiSelect: false, options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] } }
      : requests.length === 2 && !interrupted
      ? { id: "toolu_command", name: "Bash", input: { command: "printf fixture > denied-command-marker", description: "Write a disposable test marker" } }
      : undefined
    res.writeHead(200, { "content-type": "text/event-stream" })
    for (const event of [
      { type: "message_start", message: { id: `msg_${requests.length}`, type: "message", role: "assistant", content: [], model: body.model, stop_reason: null, stop_sequence: null, usage: { input_tokens: 1, output_tokens: 0 } } },
      { type: "content_block_start", index: 0, content_block: tool ? { type: "tool_use", id: tool.id, name: tool.name, input: {} } : { type: "text", text: "" } },
      { type: "content_block_delta", index: 0, delta: tool ? { type: "input_json_delta", partial_json: JSON.stringify(tool.input) } : { type: "text_delta", text: "native fixture response" } },
      { type: "content_block_stop", index: 0 },
      { type: "message_delta", delta: { stop_reason: tool ? "tool_use" : "end_turn", stop_sequence: null }, usage: { output_tokens: 3 } },
      { type: "message_stop" },
    ]) res.write(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`)
    res.end()
  })
}

export function codexModel(requests: any[], approval = false, questionThenApproval = false) {
  return createServer(async (req, res) => {
    const chunks: Buffer[] = []
    for await (const chunk of req) chunks.push(Buffer.from(chunk))
    requests.push(JSON.parse(Buffer.concat(chunks).toString()))
    const item = (requests.length === 1 && approval) || (requests.length === 2 && questionThenApproval)
      ? { type: "function_call", id: "fc_command", call_id: "call_command", name: "exec_command", arguments: JSON.stringify({ cmd: questionThenApproval ? "printf fixture > denied-command-marker" : "printf native-approval-fixture", sandbox_permissions: "require_escalated", justification: "Local approval fixture" }) }
      : requests.length === 1
      ? { type: "function_call", id: "fc_question", call_id: "call_question", name: "request_user_input", arguments: JSON.stringify({ questions: [{ id: "choice", header: "Choice", question: "Which option?", options: [{ label: "A", description: "First" }, { label: "B", description: "Second" }] }] }) }
      : { id: `msg_${requests.length}`, type: "message", role: "assistant", status: "completed", content: [{ type: "output_text", text: "fixture response", annotations: [] }] }
    res.writeHead(200, { "content-type": "text/event-stream" })
    for (const event of [
      { type: "response.created", response: { id: `resp_${requests.length}` } },
      { type: "response.output_item.added", output_index: 0, item },
      { type: "response.output_item.done", output_index: 0, item },
      { type: "response.completed", response: { id: `resp_${requests.length}`, status: "completed", output: [item], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } } },
    ]) res.write(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`)
    res.end()
  })
}
