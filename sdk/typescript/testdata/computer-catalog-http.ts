import assert from "node:assert/strict"
import { HelmrClient } from "../src/client"
const config = JSON.parse(await Bun.stdin.text()) as { url: string; apiKey: string; deploymentId: string }
const client = new HelmrClient(config)
const agent = await client.agents.get("agent", { deploymentId: config.deploymentId })
assert.deepEqual(agent, { agent: "agent", template: "fixture-computer", deploymentId: config.deploymentId, inputKind: "text" })
const agents = await client.agents.list({ deploymentId: config.deploymentId, limit: 1 })
assert.deepEqual(agents.items, [{ agent: "agent", template: "fixture-computer", inputKind: "text" }])
assert.equal(agents.deploymentId, config.deploymentId)
assert.equal("agentDefinitions" in client, false)
const page = await client.templates.list({ deploymentId: config.deploymentId, limit: 1 })
assert.deepEqual(page, { deploymentId: config.deploymentId, items: [{ template: "fixture-computer", resources: { milliCpu: 1000, memoryMiB: 512 } }] })
assert.deepEqual(await client.templates.get("fixture-computer", { deploymentId: config.deploymentId }), { template: "fixture-computer", resources: { milliCpu: 1000, memoryMiB: 512 }, deploymentId: config.deploymentId })
const secret = await client.secrets.create({ secret: "catalog-token", value: "fixture-only", idempotencyKey: "catalog-token" })
const secrets = [
  { secret: secret.secret, env: { name: "CATALOG_TOKEN", mode: "raw" as const } },
  { secret: secret.secret, env: { name: "PROTECTED_TOKEN", mode: "protected" as const, allowedOrigins: ["https://api.example.com"] } },
]
const request = { key: "catalog", secrets, idempotencyKey: "catalog-create" }
const computer = await client.computers.create({ template: "fixture-computer", ...request })
assert.equal((await client.computers.create({ template: "fixture-computer", ...request })).id, computer.id)
const snapshot = await computer.get()
assert.equal(snapshot.template, "fixture-computer")
assert.equal(snapshot.deploymentId, config.deploymentId)
assert.deepEqual(snapshot.secrets, secrets)
assert.equal((await client.computers.list({ key: "catalog" })).items[0]?.id, computer.id)
assert.equal("sandboxes" in client, false)
