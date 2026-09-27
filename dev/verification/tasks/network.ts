import { task, tokens, logger } from "@helmr/sdk"
import { z } from "zod"

export const networkTask = task({
  id: "verification-network",
  maxDuration: "10m",
  retry: { enabled: false },
  payload: z.object({ marker: z.string().min(1), startToken: z.string(), finishToken: z.string() }).strict(),
  run: async ({ marker, startToken, finishToken }, ctx) => {
    const positive = await fetch("https://example.com/", { signal: AbortSignal.timeout(15_000) })
    if (!positive.ok) throw new Error(`public HTTPS control failed: ${positive.status}`)
    await positive.arrayBuffer()
    // Take the baseline after public egress, then keep this exact VM alive.
    await logger.info("verification network ready", { marker, phase: "ready" })
    await tokens.ref(startToken).wait({ timeout: "8m", idleTimeout: "8m" }).unwrap()
    let blocked = false
    try {
      // Probe only the metadata index; never request credentials or an IMDS token.
      const response = await fetch("http://169.254.169.254/latest/meta-data/", { signal: AbortSignal.timeout(5000) })
      await response.body?.cancel()
    } catch { blocked = true }
    if (!blocked) throw new Error("metadata endpoint returned an HTTP response")
    await logger.info("verification network observed", { marker, phase: "observed", positiveStatus: positive.status, blocked })
    await tokens.ref(finishToken).wait({ timeout: "8m", idleTimeout: "8m" }).unwrap()
    return { marker, blocked, positiveStatus: positive.status, runId: ctx.run.id, workspaceId: ctx.workspace.id }
  },
})
