import { image, sandbox, task, tokens } from "@helmr/sdk"
import { z } from "zod"

export const tokenWorkspace = sandbox({ id: "verification-token" })
  .image(image("verification-token").from("node:24-bookworm-slim"))
  .resources({ cpu: 1, memory: "1GiB" })

export const tokenTask = task({
  id: "verification-token",
  maxDuration: "10m",
  payload: z.object({ marker: z.string(), tokenId: z.uuidv7().optional() }).strict(),
  run: async ({ marker, tokenId }) => {
    const token = tokenId
      ? tokens.ref(tokenId)
      : await tokens.create({ timeout: "5m", tags: [marker] })
    const result = await token
      .wait({
        schema: z.object({ approved: z.boolean() }),
        timeout: "5m",
      })
      .unwrap()
    return { marker, token: result }
  },
})
