import { actor, tokens } from "@helmr/sdk"
import { z } from "zod"

// Output can finish before the application knows whether the Turn succeeded.
export const checkedReply = actor({
  id: "checked-reply",
  async run(session) {
    for (;;) {
      const turn = await session.receive()
      if (turn === null) return
      let result: { characters: number }
      try {
        const input = z.object({ text: z.string(), expected: z.string() }).parse(turn.input)
        await turn.output.pipe([{ type: "reply", text: input.text }])
        // Replace this check with the project's deterministic test command.
        // The preceding finite stream ending is not success.
        if (input.text !== input.expected) throw new Error("Reply check failed")
        result = { characters: input.text.length }
      } catch (error) {
        if (turn.signal.aborted) throw error
        await turn.fail(error)
        continue
      }
      await turn.complete(result)
    }
  },
})

// A generic Token wait works inside a Turn without a turn.tokens namespace.
export const externalCI = actor({
  id: "external-ci",
  async run(session) {
    for (;;) {
      const turn = await session.receive()
      if (turn === null) return
      try {
        const input = z.object({ commit: z.string() }).parse(turn.input)
        const completion = await tokens.create({
          timeout: "24h",
          idempotencyKey: `ci:${turn.id}`,
          metadata: { commit: input.commit },
        })
        // An authenticated integration consumes this event and starts CI,
        // deduplicating its remote effect using the retained event ID.
        // Do not publish completion.callbackUrl or publicAccessToken here.
        await turn.output.write({ type: "ci_requested", tokenId: completion.id, commit: input.commit })
        const result = await completion.wait({
          timeout: "24h",
          schema: z.object({ passed: z.boolean(), reportUrl: z.string() }),
        }).unwrap()
        if (!result.passed) throw new Error(`CI failed: ${result.reportUrl}`)
        await turn.output.write({ type: "ci_passed", reportUrl: result.reportUrl })
      } catch (error) {
        // A stop belongs to runtime convergence, not a second application outcome.
        if (turn.signal.aborted) throw error
        await turn.fail(error)
        continue
      }
      await turn.complete()
    }
  },
})
