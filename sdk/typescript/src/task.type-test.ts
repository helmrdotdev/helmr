import { image, sandbox, task, type PayloadSchema, type PayloadValidationSchema } from "./index"

const sb = sandbox("task-type-test").image(image("task-type-test").from("debian:trixie-slim"))

if (false) {
  const payloadSchema: PayloadSchema<{ readonly issue: string }, { readonly issue: number }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate: () => ({ value: { issue: 1 } }),
    },
    toJSONSchema() {
      return {}
    },
  }
  const validationOnlySchema: PayloadValidationSchema<unknown, { readonly approved: boolean }> = {
    "~standard": {
      version: 1,
      vendor: "test",
      validate: () => ({ value: { approved: true } }),
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
    id: "token-validation-schema-type",
    sandbox: sb,
    run: async (ctx) => {
      const token = await ctx.wait.token({ schema: validationOnlySchema })
      const approved: boolean = token.approved
      // @ts-expect-error wait.token receives parsed schema output.
      const rawApproved: string = token.approved
      return { approved, rawApproved }
    },
  })

  task({
    id: "payload-schema-requires-metadata",
    sandbox: sb,
    // @ts-expect-error task payloadSchema requires JSON metadata.
    payloadSchema: validationOnlySchema,
    run: async (payload) => payload,
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
