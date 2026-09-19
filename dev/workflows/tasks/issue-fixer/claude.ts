import { spawn } from "node:child_process"
import { actor, MessageRejected, type JsonValue } from "@helmr/sdk"
import { query, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk"
import { z } from "zod"
import { HumanRequests, replySchema } from "./human"
import { Queue } from "./queue"
import { checkRepository, issueSchema, repository } from "./checks"

export const claudeIssueFixer = actor({
  id: "claude-issue-fixer", input: issueSchema, message: replySchema,
  async run(session) {
    const cwd = repository()
    for (;;) {
      const turn = await session.receive()
      if (!turn) return
      const lifetime = new AbortController()
      const prompts = new Queue<SDKUserMessage>()
      const human = new HumanRequests(value => turn.output.write(value), turn.signal)
      let accepting = true
      const followups: string[] = []
      let completed = false
      let failure: { error: unknown } | undefined
      let nativeExited: Promise<void> = Promise.resolve()
      const prompt = (text: string): SDKUserMessage => ({
        type: "user", session_id: "", parent_tool_use_id: null,
        message: { role: "user", content: text },
      })
      const stop = () => lifetime.abort()
      turn.signal.addEventListener("abort", stop, { once: true })
      if (turn.signal.aborted) stop()
      // A separate native query per Helmr Turn. Files remain in the Session workspace.
      const native = query({
        prompt: prompts,
        options: {
          cwd, abortController: lifetime, permissionMode: "default", settingSources: [],
          includePartialMessages: true,
          spawnClaudeCodeProcess(options) {
            const child = spawn(options.command, options.args, {
              cwd: options.cwd, env: options.env, signal: options.signal,
              stdio: ["pipe", "pipe", "pipe"],
            })
            child.stderr.on("data", chunk => process.stderr.write(chunk))
            nativeExited = new Promise(resolve => child.once("close", () => resolve()))
            return child
          },
          async canUseTool(toolName, input, options) {
            const signal = AbortSignal.any([lifetime.signal, options.signal])
            const action = JSON.parse(JSON.stringify({ provider: "claude", toolUseID: options.toolUseID, toolName, input })) as JsonValue
            if (toolName === "AskUserQuestion") {
              const questions = z.array(z.object({ question: z.string() })).min(1).parse(input.questions)
              const keys = questions.map(q => q.question)
              if (new Set(keys).size !== keys.length) return { behavior: "deny", message: "Ambiguous duplicate question text" }
              const reply = await human.ask("answer", action, signal, keys)
              signal.throwIfAborted()
              if (reply.type !== "answer") throw new Error("Unexpected answer")
              return { behavior: "allow", updatedInput: { ...input, answers: Object.fromEntries(Object.entries(reply.answers).map(([key, values]) => [key, values.join(", ")])) } }
            }
            const reply = await human.ask("approval", action, signal)
            signal.throwIfAborted()
            return reply.type === "approval" && reply.allow
              ? { behavior: "allow", updatedInput: input }
              : { behavior: "deny", message: "User denied this operation" }
          },
        },
      })
      try {
        await turn.onMessage(async ({ data }) => {
          if (!accepting) throw new MessageRejected("Native processing has finished")
          if (data.type === "update_constraints") {
            // Serialize native calls ourselves; do not assume one native result per
            // rapidly submitted input or describe this as Codex-style steering.
            followups.push(data.text)
          } else await human.reply(data)
        })
        prompts.push(prompt(`Fix this issue and explain the change: ${turn.input.issue}`))
        for await (const message of native) {
          await turn.output.write({ type: "provider_event", provider: "claude", event: JSON.parse(JSON.stringify(message)) as JsonValue })
          if (message.type === "result") {
            if (message.subtype !== "success" || message.is_error) throw new Error("Claude did not complete successfully")
            const next = followups.shift()
            if (next === undefined) { accepting = false; completed = true; break }
            prompts.push(prompt(next))
          }
        }
        if (!completed) throw new Error("Claude ended before returning every result")
      } catch (error) {
        failure = { error }
      } finally {
        accepting = false
        lifetime.abort()
        prompts.end()
        native.close()
        // close() only requests shutdown. No checks/settlement until direct exit.
        // This is not proof of descendant exclusion or remote-effect rollback.
        await nativeExited
        turn.signal.removeEventListener("abort", stop)
      }
      if (failure !== undefined) {
        if (turn.signal.aborted) throw failure.error
        await turn.fail(failure.error)
        continue
      }
      try {
        await checkRepository(cwd, turn.signal)
      } catch (error) {
        if (turn.signal.aborted) throw error
        await turn.fail(error)
        continue
      }
      await turn.complete()
    }
  },
})
