import { image, sandbox, task } from "@helmr/sdk"

const base = image("hello-world").from("debian:trixie-slim")

const sbx = sandbox("hello-world")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

interface Payload {
  readonly name?: string
}

export const helloWorld = task({
  id: "hello-world",
  sandbox: sbx,
  maxDuration: 300,
  run: async (payload: Payload, ctx) => {
    const name = payload.name?.trim() || "Helmr"
    const greeting = `hello ${name}`
    await Bun.write("hello.txt", `${greeting}\nrun=${ctx.run.id}\n`)
    ctx.log.info({ message: "wrote greeting", path: "hello.txt" })
    return { greeting, runId: ctx.run.id }
  },
})
