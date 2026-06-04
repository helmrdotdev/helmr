import { image, sandbox, task, type PayloadSchema } from "./index"

const sb = sandbox("task-type-test").image(image("task-type-test").from("debian:trixie-slim"))

if (false) {
  const payload: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate: () => ({ value: { issue: 1 } }),
    },
  }
  const validationOnlySchema: PayloadSchema<unknown, { readonly approved: boolean }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate: () => ({ value: { approved: true } }),
    },
  }

  task({
    id: "schema-payload-type",
    sandbox: sb,
    payload,
    run: async (payload) => {
      const parsedIssue: number = payload.issue
      // @ts-expect-error run receives parsed schema output, not trigger input.
      const rawIssue: string = payload.issue
      return { parsedIssue, rawIssue }
    },
  })

  task({
    id: "token-validation-schema-type",
    sandbox: sb,
    run: async (ctx) => {
      const token = await ctx.wait.human({ schema: validationOnlySchema })
      const approved: boolean = token.approved
      // @ts-expect-error wait.human receives parsed schema output.
      const rawApproved: string = token.approved
      return { approved, rawApproved }
    },
  })

  task({
    id: "validation-only-payload",
    sandbox: sb,
    payload: validationOnlySchema,
    run: async (payload) => payload.approved,
  })

  task({
    id: "no-payload-type",
    sandbox: sb,
    run: async (ctx) => {
      const runId: string = ctx.run.id
      return { runId }
    },
  })

  // @ts-expect-error tasks without payload receive ctx as their only argument.
  task({
    id: "no-payload-rejects-payload-parameter",
    sandbox: sb,
    run: async (_payload: unknown, ctx: any) => ctx.run.id,
  })
}
