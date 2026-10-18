// In-process application acceptance. Real native SDKs run against synthetic
// models; this driver does not claim deployed or VM lifecycle coverage.
import { SessionRuntime, type SessionDriver } from "../../runtime/typescript/src/agent-session"
import type { InitContext } from "../../sdk/typescript/src/agent"
import type { AskAnswer, NormalizedQuestion } from "../../sdk/typescript/src/question"
import type { Content } from "../../sdk/typescript/src/content"

export const delay = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))
export async function until(check: () => boolean) {
  for (let n = 0; n < 1000; n++) { if (check()) return; await delay(20) }
  throw new Error("Expected native activity did not occur")
}

export function exampleSession(definition: ConstructorParameters<typeof SessionRuntime>[0], recovery: InitContext["recovery"]["kind"] = "initial") {
  const questions: NormalizedQuestion[] = [], responses: Content[] = []
  const controls = {
    answer: async (question: NormalizedQuestion, _signal: AbortSignal): Promise<AskAnswer> => ({
      answer: question.type === "text" ? "A community event" : { selected: [question.options.find(option => option.id === "once") ?? question.options[0]!] },
      answeredBy: { kind: "user", id: "tester" },
    }),
  }
  const driver: SessionDriver = {
    registerMessages: async () => {}, closeProcessing: async () => {},
    ask: async (_turn, _id, question, signal) => { questions.push(question); return { answer: controls.answer(question, signal), withdraw: async () => {} } },
    respond: async (_turn, _id, content) => { responses.push(content) },
    output: async () => ({ sequence: 1 }), confirmDrain: async () => {},
    finalize: async (_id, result) => ({ status: "completed", result }),
    fail: async (_id, error) => error.code === "interrupted" ? { status: "interrupted" } : { status: "failed", error },
    hold: async () => {},
  }
  const context: InitContext = {
    session: { id: "example" }, computer: { id: "computer" }, deployment: { id: "deployment" },
    recovery: { kind: recovery }, signal: new AbortController().signal,
    mcp: { getConnectionOptions: async () => { throw new Error("This example does not use MCP") } },
  }
  const runtime = new SessionRuntime(definition, context, driver)
  let sequence = 0
  return {
    runtime, questions, responses, controls,
    dispatch(text: string) {
      const n = ++sequence, id = `turn-${n}`
      return { id, result: runtime.dispatch({ id, sequence: n, createdAt: new Date().toISOString(), input: [{ type: "text", text }], source: { kind: "api_key" } }) }
    },
  }
}
