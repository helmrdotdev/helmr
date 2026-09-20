import { actor, MessageRejected, type JsonValue } from "@helmr/sdk"
import { z } from "zod"
import { CodexStdio, spawnCodex, type NativeMessage } from "./codex-stdio"
import { HumanRequests, replySchema } from "./human"
import { checkRepository, issueSchema, repository } from "./checks"

import { conversation } from "./conversation"

const threadResult = z.object({ thread: z.object({ id: z.string() }) })
const turnResult = z.object({ turn: z.object({ id: z.string(), status: z.string() }) })
const requestParams = z.object({ threadId: z.string(), turnId: z.string() }).passthrough()
const questionsSchema = z.array(z.object({ id: z.string(), isSecret: z.boolean() })).min(1)

export const codexIssueFixer = actor({
  id: "codex-issue-fixer",
  async run(session) {
    const cwd = repository()
    for (;;) {
      const turn = await session.receive()
      if (!turn) return
      const parsed = issueSchema.safeParse(turn.input)
      if (!parsed.success) { await turn.fail(parsed.error); continue }
      const input = parsed.data
      const saved = await conversation(cwd, session.id, "codex")
      const lifetime = new AbortController()
      const human = new HumanRequests(value => turn.output.write(value), turn.signal)
      const requests = new Map<string, AbortController>()
      const jobs = new Set<Promise<void>>()
      const resolved = new Set<string>()
      let threadId: string | undefined
      let nativeTurnId: string | undefined
      let accepting = false
      let stopped: Promise<void> | undefined
      const server = new CodexStdio(message => {
        if (message.method === "serverRequest/resolved") {
          const params = z.object({ threadId: z.string(), requestId: z.union([z.string(), z.number()]) }).parse(message.params)
          if (params.threadId === threadId) {
            const key = JSON.stringify(params.requestId)
            resolved.add(key)
            requests.get(key)?.abort()
          }
        } else if (message.method === "turn/completed") {
          const params = turnResult.extend({ threadId: z.string() }).parse(message.params)
          if (params.threadId === threadId) { accepting = false; lifetime.abort() }
        }
      }, spawnCodex(saved.directory))
      const stop = () => {
        accepting = false
        lifetime.abort()
        // Runtime still owns process-tree exclusion and the final interrupted state.
        // An RPC interrupt acknowledgement alone would not prove that state.
        stopped = server.close()
      }
      turn.signal.addEventListener("abort", stop, { once: true })
      if (turn.signal.aborted) stop()

      const handleRequest = async (message: NativeMessage): Promise<void> => {
        const id = message.id!
        const key = JSON.stringify(id)
        if (resolved.has(key) || lifetime.signal.aborted) return
        const cancellation = new AbortController()
        requests.set(key, cancellation)
        const signal = AbortSignal.any([lifetime.signal, cancellation.signal])
        try {
          const params = requestParams.parse(message.params)
          if (params.threadId !== threadId || params.turnId !== nativeTurnId) throw new Error("Unexpected native request target")
          const action = JSON.parse(JSON.stringify({ provider: "codex", nativeRequestId: id, method: message.method, params })) as JsonValue
          let result: unknown
          if (message.method === "item/tool/requestUserInput") {
            const questions = questionsSchema.parse(params.questions)
            // Retained output is not a secret-input channel.
            if (questions.some(q => q.isSecret)) throw new Error("Secret questions require a private application channel")
            const keys = questions.map(q => q.id)
            if (new Set(keys).size !== keys.length) throw new Error("Duplicate native question ID")
            const reply = await human.ask("answer", action, signal, keys)
            if (reply.type !== "answer") throw new Error("Unexpected answer")
            result = { answers: Object.fromEntries(Object.entries(reply.answers).map(([key, answers]) => [key, { answers }])) }
          } else if (message.method === "item/commandExecution/requestApproval" || message.method === "item/fileChange/requestApproval") {
            const reply = await human.ask("approval", action, signal)
            result = { decision: reply.type === "approval" && reply.allow ? "accept" : "decline" }
          } else {
            await server.send({ id, error: { code: -32601, message: "Unsupported interactive request" } })
            return
          }
          signal.throwIfAborted()
          await server.send({ id, result })
        } catch (error) {
          if (!signal.aborted) throw error
        } finally { requests.delete(key) }
      }

      try {
        await server.request("initialize", { clientInfo: { name: "helmr-issue-fixer", version: "1.0.0" }, capabilities: { experimentalApi: true } })
        await server.send({ method: "initialized" })
        threadId = threadResult.parse(await server.request(saved.id ? "thread/resume" : "thread/start", {
          ...(saved.id ? { threadId: saved.id } : {}),
          cwd, sandbox: "workspace-write", approvalPolicy: "on-request",
          // The pinned provider otherwise rejects questions in Default mode.
          config: { "features.default_mode_request_user_input": true },
        })).thread.id
        await saved.remember(threadId)
        nativeTurnId = turnResult.parse(await server.request("turn/start", { threadId, input: [{ type: "text", text: `Fix this issue and explain the change: ${input.issue}`, text_elements: [] }] })).turn.id
        accepting = !lifetime.signal.aborted
        await turn.onMessage(async ({ data: raw }) => {
          const parsed = z.union([issueSchema, replySchema]).safeParse(raw)
          if (!parsed.success) throw new MessageRejected("Invalid interaction")
          const data = parsed.data
          if (!accepting) throw new MessageRejected("Codex turn is not accepting messages")
          if ("issue" in data || data.type === "update_constraints") {
            await server.request("turn/steer", { threadId, expectedTurnId: nativeTurnId, input: [{ type: "text", text: "issue" in data ? data.issue : data.text, text_elements: [] }] })
          } else await human.reply(data)
        })
        let completed = false
        for await (const message of server.messages) {
          if (message.id !== undefined) {
            // Never block notification consumption on an unanswered human request.
            const job = handleRequest(message)
            jobs.add(job)
            void job.then(() => jobs.delete(job), error => { jobs.delete(job); server.messages.end(error) })
            continue
          }
          if (message.method === "turn/completed") {
            const params = turnResult.extend({ threadId: z.string() }).parse(message.params)
            if (params.threadId !== threadId || params.turn.id !== nativeTurnId) throw new Error("Unexpected native completion")
            accepting = false
            lifetime.abort()
            if (params.turn.status !== "completed") throw new Error(`Codex ended: ${params.turn.status}`)
            completed = true
            break
          } else if (message.method === "item/agentMessage/delta") {
            await turn.output.write({ type: "provider_event", provider: "codex", event: JSON.parse(JSON.stringify(message.params)) as JsonValue })
          }
        }
        if (!completed) throw new Error("Codex ended without terminal completion")
      } catch (error) {
        accepting = false
        lifetime.abort()
        await (stopped ?? server.close())
        await Promise.allSettled(jobs)
        turn.signal.throwIfAborted()
        await turn.fail(error)
        continue
      } finally {
        accepting = false
        lifetime.abort()
        await (stopped ?? server.close())
        await Promise.allSettled(jobs)
        turn.signal.removeEventListener("abort", stop)
      }
      try {
        await checkRepository(cwd, turn.signal)
      } catch (error) {
        turn.signal.throwIfAborted()
        await turn.fail(error)
        continue
      }
      await turn.complete()
    }
  },
})
