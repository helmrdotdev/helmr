export type AllowedDomain = string | { readonly domain: string; readonly ipRanges: readonly string[] }
/** Outbound guest connectivity. Omission keeps ordinary protected IPv4 access. */
export type NetworkPolicy =
  | { readonly blockAll: boolean; readonly allowDomains?: never }
  | { readonly allowDomains: readonly AllowedDomain[]; readonly blockAll?: never }
export type RuntimeMcpTargetOperation = "enqueue" | "send_turn" | "inspect_session" | "inspect_turn" | "interrupt_turn" | "interrupt_session" | "resume_session" | "close_session" | "cancel_session"
export type RuntimeMcpRule =
  | { readonly operation: "start" | "spawn"; readonly agents: readonly string[]; readonly computer?: "new" | "caller" }
  | { readonly operation: RuntimeMcpTargetOperation; readonly agents: readonly string[]; readonly computer?: never }
  | { readonly operation: "list_computers"; readonly agents?: never; readonly computer?: never }
export interface RuntimeMcpPolicy { readonly allow: readonly RuntimeMcpRule[] }


function object(value: unknown, fields: readonly string[], path: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value) || Object.keys(value).some(k => !fields.includes(k))) throw new Error(`${path}: invalid or unknown members`)
  return value as Record<string, unknown>
}
function domain(value: unknown, path: string): string {
  if (typeof value !== "string" || !/^[\x00-\x7f]+$/.test(value)) throw new Error(`${path}: expected an ASCII DNS name`)
  let name = value.toLowerCase().replace(/\.$/, "")
  const suffix = name.startsWith("*.") ? name.slice(2) : name
  const labels = suffix.split(".")
  if (name.length > 253 || labels.length < 2 || labels.some(label => !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label)) || /^[0-9.]+$/.test(suffix) || suffix === "localhost" || suffix.endsWith(".localhost")) throw new Error(`${path}: invalid DNS pattern`)
  return name
}
function cidr(value: unknown, path: string): string {
  if (typeof value !== "string") throw new Error(`${path}: expected a canonical IPv4 CIDR`)
  const m = /^(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\.(0|[1-9]\d{0,2})\/(0|[1-9]\d?)$/.exec(value)
  if (!m) throw new Error(`${path}: expected a canonical IPv4 CIDR`)
  const octets = m.slice(1,5).map(Number), bits = Number(m[5])
  if (octets.some(x => x > 255) || bits > 32) throw new Error(`${path}: invalid IPv4 CIDR`)
  const address = octets.reduce((v,x) => (v << 8n) + BigInt(x), 0n)
  if ((address & ((1n << BigInt(32-bits))-1n)) !== 0n) throw new Error(`${path}: CIDR has host bits`)
  return value
}
export function normalizeNetwork(value: unknown, path: string): NetworkPolicy {
  const input = object(value, ["blockAll", "allowDomains"], `invalid_network_policy ${path}`)
  const block = Object.hasOwn(input,"blockAll"), allow = Object.hasOwn(input,"allowDomains")
  if (block === allow) throw new Error(`invalid_network_policy ${path}: specify exactly one of blockAll or allowDomains`)
  if (block) {
    if (typeof input["blockAll"] !== "boolean") throw new Error(`invalid_network_policy ${path}.blockAll: expected boolean`)
    return { blockAll: input["blockAll"] }
  }
  if (!Array.isArray(input["allowDomains"]) || input["allowDomains"].length > 256) throw new Error(`invalid_network_policy ${path}.allowDomains: expected at most 256 entries`)
  const seen = new Set<string>()
  const entries = input["allowDomains"].map((entry: unknown, i: number) => {
    const field = `invalid_network_policy ${path}.allowDomains[${i}]`
    const obj = typeof entry === "string" ? undefined : object(entry,["domain","ipRanges"],field)
    const pattern = domain(obj ? obj["domain"] : entry, field)
    if (seen.has(pattern)) throw new Error(`${field}: duplicate domain`)
    seen.add(pattern)
    if (!obj) return pattern
    if (!Array.isArray(obj["ipRanges"]) || obj["ipRanges"].length < 1 || obj["ipRanges"].length > 64) throw new Error(`${field}.ipRanges: expected 1 to 64 CIDRs`)
    const ranges = obj["ipRanges"].map((x: unknown) => cidr(x,`${field}.ipRanges`)).sort()
    if (new Set(ranges).size !== ranges.length) throw new Error(`${field}.ipRanges: duplicate range`)
    return { domain: pattern, ipRanges: ranges }
  }).sort((a,b) => compare(typeof a === "string" ? a : a.domain, typeof b === "string" ? b : b.domain))
  return entries.length === 0 ? { blockAll: true } : { allowDomains: entries }
}
const targets = new Set(["enqueue","send_turn","inspect_session","inspect_turn","interrupt_turn","interrupt_session","resume_session","close_session","cancel_session"])
export function normalizeRuntimeMcp(value: unknown, keys: ReadonlySet<string> | undefined, path: string): RuntimeMcpPolicy {
  const label = `invalid_runtime_mcp_policy ${path}`
  const input = object(value,["allow"],label)
  if (!Array.isArray(input["allow"]) || input["allow"].length > 256) throw new Error(`${label}.allow: expected at most 256 rules`)
  const groups = new Map<string,{operation:string;computer?:"new"|"caller";agents:Set<string>}>()
  for (const [i,entry] of input["allow"].entries()) {
    const field = `${label}.allow[${i}]`
    const rule = object(entry,["operation","agents","computer"],field)
    const op = rule["operation"]
    switch (op) {
      case "list_agents": throw new Error(`${field}.operation: list_agents is derived from start/spawn grants; author start or spawn instead`)
      case "skills": throw new Error(`${field}.operation: skills is local guidance derived from descriptor capabilities, not an authored business operation`)
      case "list_sessions": throw new Error(`${field}.operation: list_sessions is derived from inspect_session; author inspect_session instead`)
      case "wait_turn": throw new Error(`${field}.operation: wait_turn is derived from inspect_turn; author inspect_turn instead`)
    }
    if (op === "list_computers") {
      if (Object.hasOwn(rule,"agents") || Object.hasOwn(rule,"computer")) throw new Error(`${field}: list_computers has no agents or computer`)
      groups.set(op,{operation:op,agents:new Set()}); continue
    }
    if (typeof op !== "string" || (!targets.has(op) && op !== "start" && op !== "spawn")) throw new Error(`${field}.operation: unsupported operation`)
    const creation = op === "start" || op === "spawn"
    if ((!creation && Object.hasOwn(rule,"computer")) || (creation && rule["computer"] !== undefined && rule["computer"] !== "new" && rule["computer"] !== "caller")) throw new Error(`${field}.computer: invalid placement`)
    if (!Array.isArray(rule["agents"]) || rule["agents"].length < 1 || rule["agents"].length > 256 || new Set(rule["agents"]).size !== rule["agents"].length) throw new Error(`${field}.agents: expected 1 to 256 unique Agent keys`)
    for (const key of rule["agents"]) if (typeof key !== "string" || !/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(key) || (keys !== undefined && !keys.has(key))) throw new Error(`${field}.agents: unknown Agent key`)
    const placement = creation ? (rule["computer"] ?? "new") as "new"|"caller" : undefined
    const groupKey = `${op}\0${placement ?? ""}`
    const group = groups.get(groupKey) ?? { operation:op,...(placement === undefined ? {} : {computer:placement}),agents:new Set<string>() }
    for (const key of rule["agents"]) group.agents.add(key)
    groups.set(groupKey,group)
  }
  const allow: RuntimeMcpRule[] = []
  for (const [,group] of [...groups].sort(([a],[b]) => a < b ? -1 : a > b ? 1 : 0)) {
    if (group.operation === "list_computers") { allow.push({operation:"list_computers"}); continue }
    const agents = [...group.agents].sort()
    for (let i=0;i<agents.length;i+=256) allow.push({operation:group.operation,agents:agents.slice(i,i+256),...(group.computer === undefined ? {} : {computer:group.computer})} as RuntimeMcpRule)
  }
  return { allow }
}

function compare(a: string,b: string): number { return a < b ? -1 : a > b ? 1 : 0 }

export function validatePolicyOrigins(policy: NetworkPolicy, bindings: readonly import("./agent").SecretBinding[], path: string): void {
  for (const [i,binding] of bindings.entries()) {
    if (!binding.env || binding.env.mode !== "protected") continue
    for (const origin of binding.env.allowedOrigins) {
      const url = new URL(origin)
      const host = url.hostname
      const allowed = "blockAll" in policy ? !policy.blockAll : policy.allowDomains.some(entry => {
        const pattern = typeof entry === "string" ? entry : entry.domain
        return pattern.startsWith("*.") ? host.endsWith(pattern.slice(1)) && host !== pattern.slice(2) : host === pattern
      })
      if (!allowed || url.protocol !== "https:" || ("allowDomains" in policy && url.port !== "" && url.port !== "443")) throw new Error(`invalid_network_policy ${path}[${i}]: protected origin is outside phase network policy`)
    }
  }
}
