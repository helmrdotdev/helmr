import { image, sandbox, task } from "@helmr/sdk"

const base = image("secret-vault").from("debian:trixie-slim")

const sbx = sandbox("secret-vault")
  .image(base)
  .resources({ cpu: 1, memory: "1Gi" })

export const useSecret = task({
  id: "use-secret",
  sandbox: sbx,
  maxDuration: 300,
  secrets: {
    API_TOKEN: { env: "API_TOKEN" },
  },
  run: async (_payload: unknown, ctx) => {
    if (!process.env.API_TOKEN) {
      throw new Error("API_TOKEN was not injected")
    }
    ctx.log.info({ secret: "API_TOKEN", available: true })
    return { ok: true }
  },
})
