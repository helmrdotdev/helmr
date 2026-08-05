import { image, source, task, sandbox, type JsonValue } from "@helmr/sdk"
import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { z } from "zod"

const execFileAsync = promisify(execFile)
const probeSource = source.file("tasks/smoke/datapath-network-probe.py")

const base = image("helmr-datapath-network")
  .from("node:24-bookworm-slim")
  .copy("/opt/helmr/datapath-network-probe.py", probeSource)
  .run([
    "sh",
    "-ceu",
    "apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*",
  ])
  .user("root")
  .workdir("/sandbox")

export const datapathNetworkWorkspace = sandbox({ id: "helmr-datapath-network" })
  .image(base)
  .resources({ cpu: 1, memory: "1GiB" })

const address = z.string().min(1).max(253).regex(/^[A-Za-z0-9.:%_-]+$/)
const mac = z.string().regex(/^[0-9a-fA-F]{2}(?::[0-9a-fA-F]{2}){5}$/)

const payload = z.object({
  campaignId: z.string().regex(/^[a-z0-9][a-z0-9-]{0,62}$/),
  caseId: z.string().regex(/^[a-z0-9][a-z0-9-]{0,62}$/),
  mode: z.enum(["tcp", "udp", "icmp", "dns", "raw-ip", "raw-mac", "ipv6", "hold"]),
  target: address.optional(),
  port: z.number().int().min(1).max(65535).optional(),
  interface: z.string().regex(/^[A-Za-z0-9_.-]{1,15}$/).optional(),
  sourceAddress: address.optional(),
  sourceMac: mac.optional(),
  destinationMac: mac.optional(),
  attempts: z.number().int().min(1).max(32).default(1),
  startDelayMs: z.number().int().min(0).max(120000).default(0),
  intervalMs: z.number().int().min(0).max(1000).default(50),
  timeoutMs: z.number().int().min(50).max(10000).default(1000),
  holdMs: z.number().int().min(1).max(300000).optional(),
  expectReply: z.boolean().optional(),
  queryName: z.string().min(1).max(253).optional(),
  transport: z.enum(["udp", "tcp"]).optional(),
}).strict()

type Payload = z.infer<typeof payload>

export const datapathNetwork = task({
  id: "datapath-network",
  maxDuration: "10m",
  retry: { enabled: false },
  payload,
  run: async (input: Payload): Promise<JsonValue> => {
    if (input.startDelayMs > 0) {
      await new Promise((resolve) => setTimeout(resolve, input.startDelayMs))
    }
    const probeInput = {
      mode: input.mode,
      ...(input.target === undefined ? {} : { target: input.target }),
      ...(input.port === undefined ? {} : { port: input.port }),
      ...(input.interface === undefined ? {} : { interface: input.interface }),
      ...(input.sourceAddress === undefined ? {} : { sourceAddress: input.sourceAddress }),
      ...(input.sourceMac === undefined ? {} : { sourceMac: input.sourceMac }),
      ...(input.destinationMac === undefined ? {} : { destinationMac: input.destinationMac }),
      attempts: input.attempts,
      intervalMs: input.intervalMs,
      timeoutMs: input.timeoutMs,
      ...(input.holdMs === undefined ? {} : { holdMs: input.holdMs }),
      ...(input.expectReply === undefined ? {} : { expectReply: input.expectReply }),
      ...(input.queryName === undefined ? {} : { queryName: input.queryName }),
      ...(input.transport === undefined ? {} : { transport: input.transport }),
    }
    const { stdout } = await execFileAsync(
      "python3",
      ["/opt/helmr/datapath-network-probe.py", JSON.stringify(probeInput)],
      {
        timeout: 330_000,
        maxBuffer: 64 * 1024,
        windowsHide: true,
      },
    )
    const result: unknown = JSON.parse(stdout)
    if (
      typeof result !== "object" ||
      result === null ||
      (result as { schema?: unknown }).schema !== "helmrdotdev.datapath-probe-result.v0" ||
      (result as { mode?: unknown }).mode !== input.mode ||
      !Array.isArray((result as { attempts?: unknown }).attempts)
    ) {
      throw new Error("datapath probe returned an invalid result")
    }
    const attempts = (result as {
      attempts: Array<{ outcome?: unknown; flow?: unknown }>
    }).attempts
    if (
      input.mode !== "hold" &&
      (attempts.length !== input.attempts ||
        attempts.some((attempt) => attempt.outcome !== "observed"))
    ) {
      throw new Error("datapath probe did not observe the requested flow")
    }
    if (
      input.mode === "tcp" &&
      attempts.some((attempt) => {
        const flow = attempt.flow
        return (
          typeof flow !== "object" ||
          flow === null ||
          (flow as { protocol?: unknown }).protocol !== "tcp" ||
          typeof (flow as { sourceAddress?: unknown }).sourceAddress !== "string" ||
          typeof (flow as { destinationAddress?: unknown }).destinationAddress !== "string" ||
          !Number.isInteger((flow as { sourcePort?: unknown }).sourcePort) ||
          !Number.isInteger((flow as { destinationPort?: unknown }).destinationPort)
        )
      })
    ) {
      throw new Error("datapath TCP probe returned an invalid flow identity")
    }
    return {
      campaignId: input.campaignId,
      caseId: input.caseId,
      probe: result as JsonValue,
    }
  },
})
