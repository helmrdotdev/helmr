import assert from "node:assert/strict"
import { HelmrClient } from "../src/client"
const config = JSON.parse(await Bun.stdin.text()) as { url: string; apiKey: string; deploymentId: string; agentId: string }
const client = new HelmrClient(config)
const agent = await client.agentDefinitions.get("agent", { deploymentId: config.deploymentId })
assert.deepEqual(agent, { id: "agent", agentId: config.agentId, deploymentId: config.deploymentId })
const agents = await client.agentDefinitions.list({ deploymentId: config.deploymentId, limit: 1 })
assert.deepEqual(agents.items, [{ id: "agent", agentId: config.agentId }])
assert.equal(agents.deploymentId, config.deploymentId)
assert.equal("agents" in client, false)
const page = await client.templateDefinitions.list({ deploymentId: config.deploymentId, limit: 1 })
assert.deepEqual(page, { deploymentId: config.deploymentId, items: [{ id: "fixture-computer" }] })
assert.deepEqual(await client.templateDefinitions.get("fixture-computer", { deploymentId: config.deploymentId }), { id: "fixture-computer", deploymentId: config.deploymentId })
const secret = await client.secrets.create({ name: "catalog-token", value: "fixture-only", idempotencyKey: "catalog-token" })
const secrets = [
  { secretId: secret.id, env: { name: "CATALOG_TOKEN", mode: "raw" as const } },
  { secretId: secret.id, env: { name: "PROTECTED_TOKEN", mode: "protected" as const, allowedOrigins: ["https://api.example.com"] } },
]
const request = { key: "catalog", secrets, idempotencyKey: "catalog-create" }
const computer = await client.computers.create({ template: "fixture-computer", ...request })
assert.equal((await client.computers.create({ template: "fixture-computer", ...request })).id, computer.id)
const snapshot = await computer.get()
assert.equal(snapshot.definitionId, "fixture-computer")
assert.equal(snapshot.deploymentId, config.deploymentId)
assert.deepEqual(snapshot.secrets, secrets)
assert.equal((await client.computers.list({ key: "catalog" })).items[0]?.id, computer.id)
assert.equal("sandboxes" in client, false)
