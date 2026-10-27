import { agent, image, template, type JsonValue, type RuntimeMcpConnectionOptions, type RuntimeMcpPolicy } from "@helmr/sdk"
import { randomUUID } from "node:crypto"
import { Resolver } from "node:dns/promises"
import { readFile } from "node:fs/promises"
import { connect } from "node:net"
import { connect as connectTLS } from "node:tls"
import { z } from "zod"
import { callMcp, fixtureInput, fixtureValue, waitManagedTurn, type ManagedAdmission } from "../../support/runtime-mcp"

const allowedHost = "checkip.amazonaws.com"
const deniedHost = "example.com"
const preparationFile = "/workspace/execution-policy-preparation"
const base = image("execution-policy-probes").from("node:24-bookworm-slim").workdir("/workspace")
const resources = { cpu: 1, memory: "1GiB" }
const preparationProbe = (allowed: boolean) => `
  import { writeFile } from "node:fs/promises";
  let reached = false;
  try {
    const response = await fetch("https://${allowedHost}", { signal: AbortSignal.timeout(5000), headers: { Connection: "close" } });
    reached = true;
    if (!response.ok) throw new Error("Preparation HTTPS returned " + response.status);
    await response.text();
  } catch (error) {
    if (${allowed}) throw error;
  }
  if (reached !== ${allowed}) throw new Error("Preparation network policy mismatch");
  await writeFile("${preparationFile}", "${allowed ? "prepare-allowed" : "prepare-blocked"}");
`

// Selecting this fixture for deployment explicitly runs its public preparation probe.
export const ordinaryComputer = template({ id: "execution-policy-ordinary", image: base, resources })
export const finiteComputer = template({
  id: "execution-policy-finite", image: base, resources,
  network: { allowDomains: [allowedHost] }, prepareNetwork: { blockAll: true },
  async prepare(ctx) { await ctx.exec(["node", "--input-type=module", "-e", preparationProbe(false)]) },
})
export const blockedComputer = template({
  id: "execution-policy-blocked", image: base, resources,
  network: { blockAll: true }, prepareNetwork: { allowDomains: [allowedHost] },
  async prepare(ctx) { await ctx.exec(["node", "--input-type=module", "-e", preparationProbe(true)]) },
})

function require(condition: unknown, message: string): asserts condition {
  if (!condition) throw new Error(message)
}
function bounded(signal: AbortSignal): AbortSignal { return AbortSignal.any([signal, AbortSignal.timeout(5000)]) }
async function denied(label: string, signal: AbortSignal, operation: () => Promise<unknown>): Promise<void> {
  try { await operation() } catch { signal.throwIfAborted(); return }
  throw new Error(`${label} bypassed the network policy`)
}
async function http(url: string, signal: AbortSignal): Promise<string> {
  const response = await fetch(url, { signal: bounded(signal), headers: { Connection: "close" }, redirect: "error" })
  require(response.ok, `Public prerequisite ${url} returned ${response.status}`)
  return (await response.text()).trim()
}
async function httpConnection(url: string, signal: AbortSignal): Promise<void> {
  const response = await fetch(url, { signal: bounded(signal), headers: { Connection: "close" }, redirect: "manual" })
  // Receiving any HTTP response is a bypass; body cleanup cannot count as denial.
  await response.body?.cancel().catch(() => {})
}
async function dns(host: string, signal: AbortSignal, server?: string): Promise<string[]> {
  const resolver = new Resolver({ timeout: 1000, tries: 1 })
  if (server) resolver.setServers([server])
  const limited = bounded(signal)
  limited.throwIfAborted()
  const cancel = () => resolver.cancel()
  limited.addEventListener("abort", cancel, { once: true })
  try { return await resolver.resolve4(host) } finally { limited.removeEventListener("abort", cancel) }
}
async function socket(address: string, port: number, signal: AbortSignal, tls: boolean, servername?: string): Promise<void> {
  const limited = bounded(signal)
  limited.throwIfAborted()
  await new Promise<void>((resolve, reject) => {
    // These transport probes deliberately ignore certificate identity: a certificate
    // mismatch must not produce a false policy-denial result for a numeric dial.
    const connection = tls
      ? connectTLS({ host: address, port, ...(servername === undefined ? {} : { servername }), rejectUnauthorized: false })
      : connect({ host: address, port })
    const finish = (error?: Error) => {
      limited.removeEventListener("abort", abort)
      connection.removeAllListeners()
      connection.destroy()
      error ? reject(error) : resolve()
    }
    const abort = () => finish(new Error("Network probe timed out"))
    limited.addEventListener("abort", abort, { once: true })
    connection.once(tls ? "secureConnect" : "connect", () => finish())
    connection.once("error", finish)
    connection.once("close", () => finish(new Error("Network probe closed before connection")))
  })
}
const inputSchema = z.object({
  marker: z.string().min(1), expected: z.enum(["ordinary", "finite", "blocked"]),
  destinationIPv4: z.ipv4().optional(),
}).strict()
type ProbeInput = z.infer<typeof inputSchema>
type NetworkReport = { expected: ProbeInput["expected"]; destinationIPv4: string; preparation: string | null; ordinaryPrerequisites: boolean; deniedHost: boolean; dnsBypassDenied: boolean; directIPBypassDenied: boolean; httpBypassDenied: boolean }
async function networkProbe(input: ProbeInput, signal: AbortSignal): Promise<NetworkReport> {
  let destinationIPv4 = input.destinationIPv4
  if (input.expected !== "blocked") {
    const address = await http(`https://${allowedHost}`, signal)
    require(z.ipv4().safeParse(address).success, "HTTPS prerequisite did not return an IPv4 address")
    const addresses = await dns(allowedHost, signal)
    require(addresses.length > 0, "Allowed DNS query returned no A address")
    destinationIPv4 ??= addresses[0]
    require(destinationIPv4, "Allowed DNS query returned no A address")
  }
  require(destinationIPv4, "The blocked probe requires the previously observed public destination")
  if (input.expected === "ordinary") {
    // Establish each remote endpoint before relying on its later denial.
    await http(`http://${allowedHost}`, signal)
    await http(`https://${deniedHost}`, signal)
    await dns(deniedHost, signal)
    await dns(deniedHost, signal, "1.1.1.1")
    await socket(destinationIPv4, 443, signal, true)
    await socket(destinationIPv4, 443, signal, true, deniedHost)
    await socket(destinationIPv4, 80, signal, false)
    return { expected: input.expected, destinationIPv4, ordinaryPrerequisites: true, preparation: null, deniedHost: false, dnsBypassDenied: false, directIPBypassDenied: false, httpBypassDenied: false }
  }
  if (input.expected === "blocked") {
    await denied("Blocked HTTPS", signal, () => httpConnection(`https://${allowedHost}`, signal))
    await denied("Blocked DNS", signal, () => dns(allowedHost, signal))
    await denied("Blocked allowed-SNI numeric dial", signal, () => socket(destinationIPv4, 443, signal, true, allowedHost))
  }
  await denied("Unlisted HTTPS", signal, () => httpConnection(`https://${deniedHost}`, signal))
  await denied("Unlisted DNS", signal, () => dns(deniedHost, signal))
  await denied("Alternate DNS", signal, () => dns(deniedHost, signal, "1.1.1.1"))
  await denied("No-SNI numeric TLS", signal, () => socket(destinationIPv4, 443, signal, true))
  await denied("Denied-SNI numeric TLS", signal, () => socket(destinationIPv4, 443, signal, true, deniedHost))
  await denied("HTTP port numeric dial", signal, () => socket(destinationIPv4, 80, signal, false))
  // Numeric dials with allowed SNI are valid: only the trusted resolved destination
  // is authoritative. Do not mistake them for a bypass the API promises to forbid.
  if (input.expected === "finite") await socket(destinationIPv4, 443, signal, true, allowedHost)
  const preparation = await readFile(preparationFile, "utf8")
  require(preparation === (input.expected === "finite" ? "prepare-blocked" : "prepare-allowed"), "Preparation policy marker did not survive into runtime")
  return { expected: input.expected, destinationIPv4, ordinaryPrerequisites: false, deniedHost: true, dnsBypassDenied: true, directIPBypassDenied: true, httpBypassDenied: true, preparation }
}
export const ordinaryProbe = agent({
  id: "execution-policy-ordinary-probe", template: ordinaryComputer, maxTurnDuration: "3m",
  turn: async (turn, ctx) => ({
    ...await networkProbe(inputSchema.parse(fixtureValue(turn.input)), turn.signal),
    computerId: ctx.computer.id,
  }),
})
export const target = agent({
  id: "execution-policy-target", template: ordinaryComputer, runtimeMcp: { allow: [] }, maxTurnDuration: "3m",
  async turn(turn, ctx) {
    const input = inputSchema.parse(fixtureValue(turn.input))
    require(input.expected === "finite", "The child must test its actual finite Computer")
    const emptyMaximum = await ctx.mcp.getConnectionOptions()
    require(JSON.stringify(await catalog(emptyMaximum, turn.signal)) === JSON.stringify(["skills"]), "Empty Agent maximum was widened by the no-argument getter")
    await deniedTool(emptyMaximum, turn.signal, "list_computers", {})
    return { ...await networkProbe(input, turn.signal), marker: input.marker, computerId: ctx.computer.id, emptyMaximum: true }
  },
})
export const otherTarget = agent({ id: "execution-policy-other-target", template: ordinaryComputer, runtimeMcp: { allow: [] }, turn: async () => ({ other: true }) })
const maximum: RuntimeMcpPolicy = { allow: [
  { operation: "spawn", agents: [target.id, otherTarget.id], computer: "caller" },
  { operation: "inspect_turn", agents: [target.id] },
] }
const targetScope: RuntimeMcpPolicy = { allow: [{ operation: "spawn", agents: [target.id], computer: "caller" }] }
const otherScope: RuntimeMcpPolicy = { allow: [{ operation: "spawn", agents: [otherTarget.id], computer: "caller" }] }
interface Rpc {
  error?: { code: number; message: string }
  result?: { isError?: boolean; content?: { type: string; text?: string }[]; tools?: { name: string }[] }
}
async function rpc(connection: RuntimeMcpConnectionOptions, signal: AbortSignal, method: string, params: object): Promise<Rpc> {
  const response = await fetch(connection.url, {
    method: "POST", signal: AbortSignal.any([signal, AbortSignal.timeout(30_000)]),
    headers: { ...connection.headers, "Content-Type": "application/json", Accept: "application/json, text/event-stream", "MCP-Protocol-Version": "2025-11-25" },
    body: JSON.stringify({ jsonrpc: "2.0", id: randomUUID(), method, params }),
  })
  require(response.ok, `MCP authentication/transport failed: HTTP ${response.status}`)
  return await response.json() as Rpc
}
async function catalog(connection: RuntimeMcpConnectionOptions, signal: AbortSignal): Promise<string[]> {
  const response = await rpc(connection, signal, "tools/list", {})
  require(!response.error && Array.isArray(response.result?.tools), "Malformed MCP catalog")
  return response.result.tools.map(tool => tool.name).sort()
}
async function deniedTool(connection: RuntimeMcpConnectionOptions, signal: AbortSignal, name: string, args: object, receiverCode?: string) {
  const response = await rpc(connection, signal, "tools/call", { name, arguments: args })
  require(response.error || response.result?.isError, `Denied MCP operation ${name} succeeded`)
  if (receiverCode) {
    const text = response.result?.content?.map(part => part.text ?? "").join("\n") ?? ""
    require(response.result?.isError && text.startsWith(`${receiverCode}:`), `MCP ${name} did not preserve receiver denial: ${JSON.stringify(response)}`)
  }
}
export const coordinator = agent({
  id: "execution-policy-coordinator", template: finiteComputer, runtimeMcp: maximum,
  suspension: "manual", maxTurnDuration: "5m",
  async init(ctx) {
    const inherited = await ctx.mcp.getConnectionOptions()
    const explicit = await ctx.mcp.getConnectionOptions(maximum)
    require(inherited.url === explicit.url && inherited.headers.Authorization === explicit.headers.Authorization, "No-argument getter changed the finite maximum")
    return { nonce: randomUUID(), inherited, target: await ctx.mcp.getConnectionOptions(targetScope), other: await ctx.mcp.getConnectionOptions(otherScope), empty: await ctx.mcp.getConnectionOptions({ allow: [] }), turns: 0 }
  },
  async turn(turn, ctx): Promise<JsonValue> {
    const input = inputSchema.parse(fixtureValue(turn.input))
    require(input.expected === "finite", "Coordinator requires its finite Computer")
    const network = await networkProbe(input, turn.signal)
    const state = ctx.initResult
    const inherited = await ctx.mcp.getConnectionOptions()
    require(inherited.url === state.inherited.url && inherited.headers.Authorization === state.inherited.headers.Authorization, "Retained maximum descriptor changed")
    const arguments_ = { agent: target.id, computerId: ctx.computer.id, input: fixtureInput({ marker: input.marker, expected: "finite" }), idempotencyKey: `${ctx.session.id}:${input.marker}:spawn` }
    const child = await callMcp<ManagedAdmission>(state.target, turn.signal, "spawn", arguments_)
    await turn.output.write([{ type: "json", value: { phase: "child-admitted", ...child } }])
    const replay = await callMcp<ManagedAdmission>(state.target, turn.signal, "spawn", arguments_)
    require(child.sessionId === replay.sessionId && child.turnId === replay.turnId, "Scoped retry created replacement work")
    const outcome = await waitManagedTurn(state.inherited, turn.signal, child.sessionId, child.turnId)
    require(outcome.status === "completed" && outcome.result !== null && typeof outcome.result === "object" && !Array.isArray(outcome.result), `Child did not complete: ${JSON.stringify(outcome)}`)
    require("computerId" in outcome.result && outcome.result.computerId === ctx.computer.id && "preparation" in outcome.result && outcome.result.preparation === "prepare-blocked", "Child substituted its default Template for the actual Computer")
    const tools = await catalog(state.target, turn.signal)
    require(JSON.stringify(tools) === JSON.stringify(["list_agents", "skills", "spawn"]), `Scoped catalog widened: ${JSON.stringify(tools)}`)
    const agents = await callMcp<{ agents: { agent: string }[]; nextCursor?: string }>(state.target, turn.signal, "list_agents", { operation: "spawn" })
    require(!agents.nextCursor && agents.agents.length === 1 && agents.agents[0]?.agent === target.id, "Discovery exposed an ungranted Agent")
    await deniedTool(state.target, turn.signal, "inspect_turn", { sessionId: child.sessionId, turnId: child.turnId })
    await deniedTool(state.target, turn.signal, "wait_turn", { sessionId: child.sessionId, turnId: child.turnId, timeoutMs: 1 })
    await deniedTool(state.target, turn.signal, "list_computers", {})
    await deniedTool(state.other, turn.signal, "spawn", arguments_, "runtime_mcp_denied")
    require(JSON.stringify(await catalog(state.empty, turn.signal)) === JSON.stringify(["skills"]), "Empty scope advertised a business operation")
    await deniedTool(state.empty, turn.signal, "spawn", arguments_)
    await deniedTool(state.empty, turn.signal, "list_agents", { operation: "spawn" })
    return { network, nonce: state.nonce, count: ++state.turns, child: { ...child }, childResult: outcome.result, inheritedFiniteMaximum: true, allowedScopedRetry: true, deniedDirectAliasDiscovery: true, deniedCurrentReceipt: true, emptyScope: true }
  },
})
