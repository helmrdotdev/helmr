import { image, sandbox, task, type PayloadSchema } from "./index"

const sb = sandbox("task-type-test").image(image("task-type-test").from("debian:trixie-slim"))

if (false) {
  const payloadSchema: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate: () => ({ value: { issue: 1 } }),
    },
  }

  task({
    id: "schema-payload-type",
    sandbox: sb,
    payloadSchema,
    run: async (payload) => {
      const parsedIssue: number = payload.issue
      // @ts-expect-error run receives parsed schema output, not trigger input.
      const rawIssue: string = payload.issue
      return { parsedIssue, rawIssue }
    },
  })

  task({
    id: "no-payload-type",
    sandbox: sb,
    run: async (ctx) => {
      const runId: string = ctx.run.id
      return { runId }
    },
  })

  // @ts-expect-error tasks without payloadSchema receive ctx as their only argument.
  task({
    id: "no-payload-rejects-payload-parameter",
    sandbox: sb,
    run: async (_payload: unknown, ctx: any) => ctx.run.id,
  })
}
