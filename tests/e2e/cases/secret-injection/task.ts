import { image, task, sandbox } from "@helmr/sdk"
import { createHash } from "node:crypto"
import { z } from "zod"
const base = image("verification-secret").from("node:24-bookworm-slim")
export const secretWorkspace = sandbox({ id: "helmr-secret-smoke" })
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })
export const secretTask = task({
  id: "secret-smoke",
  maxDuration: "2m",
  retry: { enabled: false },
  payload: z.object({ sha256: z.string().length(64) }).strict(),
  run: async (input) => {
    const value = process.env.HELMR_VERIFICATION_SECRET
    if (!value || createHash("sha256").update(value).digest("hex") !== input.sha256)
      throw new Error("Secret injection did not match")
    return { matched: true }
  },
})
