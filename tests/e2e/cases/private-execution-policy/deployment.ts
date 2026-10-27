import assert from "node:assert/strict"
import type { HelmrClient } from "@helmr/sdk"

// The current-deployment preflight cannot bind resources created after a promotion.
export async function verifyDeploymentPins(
  deployments: Pick<HelmrClient["deployments"], "get">,
  computerDeploymentId: string,
  sessionDeploymentId: string,
  expectedBundleDigest: string,
) {
  const [computer, session] = await Promise.all([
    deployments.get(computerDeploymentId), deployments.get(sessionDeploymentId),
  ])
  assert.equal(computer.bundleDigest, expectedBundleDigest, "Computer uses another deployment bundle")
  assert.equal(session.bundleDigest, expectedBundleDigest, "Session uses another deployment bundle")
  return { computer, session }
}
