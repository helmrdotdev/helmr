import { verify, assert, assertEqual } from "../../support/context"
await verify("deployment", async ({ client, objects }) => {
  const deployment = await client.deployments.current({
    signal: AbortSignal.timeout(30_000),
  })
  assert(deployment !== null, "current Deployment was not available")
  const retrievedDeployment = await client.deployments.retrieve(deployment.id, {
    signal: AbortSignal.timeout(30_000),
  })
  assertEqual(
    retrievedDeployment.id,
    deployment.id,
    "Deployment retrieve changed the current Deployment ID",
  )
  objects.deployment_ids.push(deployment.id)
  return { currentDeploymentRetrieved: true }
})
