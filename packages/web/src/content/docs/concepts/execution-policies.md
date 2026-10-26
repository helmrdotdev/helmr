---
title: Execution policies
description: Limit Computer outbound networking and the Runtime MCP operations an Agent can obtain.
---

# Execution policies

A Template's `network` policy limits its Computer's outbound traffic. An Agent's
optional `runtimeMcp` policy limits the managed operations its Session can obtain.
Both intersect with existing authorization and installation restrictions.

Omitting network policy keeps ordinary IPv4 connectivity under installation
restrictions. Omitting MCP
policy adds no authored restriction to base-authorized Runtime MCP operations.
Choose explicit limits when an application needs a smaller authority boundary.

## Configure outbound networking

```ts
import { image, source, template } from "@helmr/sdk"

export const workerComputer = template({
  id: "worker-computer",
  image: image("worker")
    .from("node:24-bookworm-slim")
    .copy(source.directory("."), "/workspace")
    .workdir("/workspace"),
  resources: { cpu: 2, memory: "4GiB" },
  network: {
    allowDomains: [
      "api.anthropic.com",
      "*.services.example.com",
      { domain: "records.corp.example", ipRanges: ["10.60.12.0/24"] },
    ],
  },
  secrets: [{
    secret: "ANTHROPIC_API_KEY",
    env: {
      name: "ANTHROPIC_API_KEY",
      mode: "protected",
      allowedOrigins: ["https://api.anthropic.com"],
    },
  }],
  prepareNetwork: { allowDomains: ["registry.npmjs.org"] },
  prepareSecrets: [],
  async prepare(ctx) {
    await ctx.exec(["npm", "ci"], { cwd: "/workspace" })
  },
})
```

This example assumes a project with `package.json` and `package-lock.json`.
The destination list is illustrative: package installation, redirects, registry
authentication and CDNs can require additional hosts. Include every destination
used by the actual client. The private service also requires installation-approved
routing, DNS, address ranges and TLS trust.

Allowlist mode supports TLS with visible SNI on TCP 443. Exact names grant only
that hostname. `*.services.example.com` grants any depth of subdomain, excluding
`services.example.com` itself. String entries permit public IPv4 addresses;
object entries bound every address for that hostname to their own `ipRanges`.
Ranges do not grant direct-IP, non-SNI, arbitrary-port or protocol access.
One complete matching entry must allow a connection; entries do not lend one
another address bounds. A narrower exact entry cannot override a broad wildcard.

Private RFC1918 addresses require explicit entry ranges within the installation's
approved business ceiling, which defaults to empty. Management, metadata, host,
peer and other mandatory denies always apply. HTTP/80, SSH, arbitrary TCP/UDP,
QUIC, IPv6, custom guest DNS and URL/path/account rules are outside allowlist
mode. An allowed hostname is a TLS destination grant, not HTTP account or data
access authorization. Clients use ordinary sockets; proxy environment variables
are unnecessary.

Use `{ blockAll: true }` for no external guest traffic or external DNS.
`{ allowDomains: [] }` has the same meaning. Omission or `{ blockAll: false }`
keeps ordinary IPv4 TCP/UDP/ICMP connectivity under mandatory denies and the
private-business ceiling. Combining either boolean with `allowDomains` is invalid.
The [Computer reference](/docs/reference/sdk/computer#outbound-network-policy)
lists grammar, normalization and bounds.

## Preparation and existing Computers

With `prepare`, omitted `prepareNetwork` inherits runtime `network`, including
its ordinary default. An explicit value replaces that phase's whole policy.
Without a callback, preparation is fully blocked, and explicit `prepareNetwork`
or nonempty `prepareSecrets` is invalid. Seed stages always block guest egress.
Preparation connections cannot retain broader authority into runtime.

The resolved stage policy contributes to preparation identity in both callback
and seed-only paths. Policy changes can invalidate cached preparations. Other
image, Program, Runtime and Secret eligibility inputs continue to apply.
VM `prepareSecrets` and `PrepareContext` serve preparation; project
`config.build.secrets` still serves host-side dependency installation.

A Computer retains the exact Template and Deployment selected at creation across
Commands, Sessions, background processes and restore. An Agent's Template selects
a new Computer; placing its Session on an existing Computer uses that Computer's
actual policy. Redeploying stricter definitions affects newly selected definitions,
without changing existing Computers or Sessions.

## Limit Runtime MCP authority

An optional Agent maximum contains only an allowlist:

```ts
import { agent } from "@helmr/sdk"
// workerComputer is the Template above. Export reviewer in this same deployment.
export const reviewer = agent({
  id: "reviewer",
  template: workerComputer,
  runtimeMcp: { allow: [] },
  async turn() { return null },
})
```

Omitted `runtimeMcp` preserves all supported operations and targets permitted by
base authorization, including later additions. `{ allow: [] }` grants zero remote
business operations. An explicit policy requires `allow`; there is no MCP
`blockAll` or unrestricted argument. Ordinary non-use requires no policy: simply
do not obtain or inject a connection into the native client.

Each finite rule names one operation. Targeted rules name exact Agent keys from
the same published manifest. `start` and `spawn` select `computer: "new"` or
`"caller"`; omission means `"new"`. Separate rules permit both. Other targeted
operations have no placement field. `list_computers` has neither targets nor
placement. Rule overlap unions grants; creation adds no implicit inspection or
control grant. See the complete [Runtime MCP policy types](/docs/reference/sdk/agent#runtime-mcp-policy).

A no-argument getter inherits the Agent maximum. A supplied allowlist narrows
that maximum for the returned descriptor and rejects widening. Finite getter
keys still resolve against the caller's pinned manifest under an unrestricted
maximum. Equivalent normalized requests reuse credentials. A Session process
supports at most 16 distinct scopes, including its maximum, without eviction.

### Inject the connection into a native client

This Claude Agent SDK example obtains a restricted descriptor and explicitly
passes its URL and headers as a native HTTP MCP server. The native dependency
must be included in the project's Runtime dependencies.

```ts
import { agent } from "@helmr/sdk"
import { query } from "@anthropic-ai/claude-agent-sdk"
// workerComputer and reviewer are the exported definitions above.
export const coordinator = agent({
  id: "coordinator",
  template: workerComputer,
  runtimeMcp: { allow: [
    { operation: "spawn", agents: ["reviewer"], computer: "new" },
    { operation: "inspect_session", agents: ["reviewer"] },
    { operation: "inspect_turn", agents: ["reviewer"] },
  ] },
  async init(ctx) {
    const connection = await ctx.mcp.getConnectionOptions({ allow: [
      { operation: "spawn", agents: ["reviewer"], computer: "new" },
      { operation: "inspect_session", agents: ["reviewer"] },
    ] })
    return { helmr: { type: "http" as const, ...connection } }
  },
  async turn(turn, { initResult }) {
    const abortController = new AbortController()
    turn.onCancel(() => abortController.abort())
    for await (const message of query({
      prompt: turn.input.map(part => part.text).join(""),
      options: { mcpServers: { helmr: initResult.helmr }, abortController },
    })) {
      if (message.type === "result") {
        if (message.subtype !== "success") throw new Error("Native run failed")
        await turn.respond(message.result)
      }
    }
    return null
  },
})
```

The injected descriptor omits the maximum's `inspect_turn` grant. The application
can request a still narrower descriptor, such as only
`inspect_session`. A getter never lifts a finite maximum. Keeping MCP out of
one native client's configuration removes its tool surface; application code
can still obtain descriptors up to its Agent maximum. Treat the bearer header
as a process credential: keep it out of logs, model input and retained results.

Normal hold/resume, reattachment and coherent hibernation preserve descriptor
continuity. Process reconstruction reruns `init`; obtain fresh options from the
new context. An old process's descriptor cannot authorize new calls after
reconstruction. Exact-Turn output, respond, questions and finalization remain
available with an empty MCP allowlist.

## Admission, placement and trust

Every managed call still passes current Role, authenticated Environment, origin,
relationship, visibility and lifecycle checks. Unrestricted MCP retains existing
eligible-Computer placement authority. Finite creation rules narrow reuse to the
caller's Computer; restricting only a destination Agent does not protect its
Computer from otherwise authorized callers.

`start` selects the latest Deployment, `spawn` uses the caller's pinned Deployment,
and operations on an existing Session use that Session's pin. Runtime MCP `start` and `spawn` require valid initial input. The selected Agent, Deployment, Computer and initial Turn
are fixed when work is admitted. Delayed delivery rechecks current mutable
authorization and Secret eligibility; caller reconstruction does not cancel
already admitted work.

Retry a mutation with its original idempotency key and body. The current
descriptor must permit the operation before an old receipt is returned. A request
that originally omitted a Computer still requests `new` on retry, even after its
first attempt created one. Receipts acknowledge admission or control intent;
they do not promise completion, physical shutdown or recall of external effects.
Control authority also retains exact hold ownership and existing tree boundaries.

Sessions sharing a Computer are mutually trusted: they share files, credentials,
background processes and reachable relay authority. Descriptor scopes do not
isolate hostile processes on the same Computer. Put Agents with different trust requirements on
separate Computers, and restrict placement and delegation at every relevant
caller and delegate maximum. An independently authorized delegate keeps its own
network and MCP authority; policies do not intersect recursively down a chain.

Network blocking still permits Helmr result/control relays, retained outputs,
questions and configured integration effects. Deliberately supplied management
API credentials are another authority path. These policies provide no air-gap,
universal DLP, live policy editing or incident-wide timed cutoff guarantee.
Use existing lifecycle controls for their documented scopes; a redeploy does not
stop old pinned resources.

## Inspect the pins

```ts
// client is an authenticated HelmrClient; IDs identify existing resources.
const computer = await client.computers.get(computerId)
const template = await client.templates.get(computer.template, {
  deploymentId: computer.deploymentId,
})
template.network
template.prepareNetwork // Resolved callback policy, or full blocking without one.
template.prepareSecrets

const session = await client.sessions.get(sessionId)
const agent = await client.agents.get(session.agent, {
  deploymentId: session.deploymentId,
})
agent.runtimeMcp // undefined = unrestricted; { allow: [] } = zero remote grants.
```

These reads show normalized published configuration at each resource's own pin.
The Session's default Template does not substitute for the actual Computer.
Installation restrictions and current base authorization remain separate; a read
is not a live packet-enforcement acknowledgement.

## Policy errors

Policy validation uses the existing API error envelope and a safe field path.
Runtime MCP tool errors retain their `code: message` text form.

| Code | Meaning |
| --- | --- |
| `invalid_network_policy` | Invalid network grammar, preparation phase or protected-origin combination. |
| `invalid_runtime_mcp_policy` | Invalid MCP policy, targets or supplied narrowing request. |
| `runtime_mcp_denied` | The current descriptor does not permit the operation, target or placement. |
| `runtime_mcp_scope_limit` | The process has reached its distinct-scope limit. |
| `network_policy_unsupported` | Worker-host endpoints only: HTTP 409 when a Worker's network capabilities are not accepted. |

For public API/SDK callers, affected allocations remain pending until the
incompatible Worker is corrected or existing drain/fencing controls release the
allocation for placement.

Unknown or invisible resources keep their existing non-disclosure behavior.
Denied guest connections surface as native socket or TLS errors.
