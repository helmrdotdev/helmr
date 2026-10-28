import { expect, test } from "bun:test"
import { verifyDeploymentPins } from "./deployment"

for (const promotedBefore of ["computer", "session", "neither"] as const) {
  test(`deployment pins reject promotion before ${promotedBefore}`, async () => {
    const checkedAtPreflight = "sha256:expected"
    const original = { id: "original", version: "1", createdAt: "2026-10-27T00:00:00Z", bundleDigest: checkedAtPreflight }
    const promoted = { ...original, id: "promoted", version: "2", bundleDigest: "sha256:other" }
    // Current-deployment preflight passed; a later creation can pin new source.
    const computerId = promotedBefore === "computer" ? promoted.id : original.id
    const sessionId = promotedBefore === "neither" ? original.id : promoted.id
    const reads: string[] = []
    const deployments = { async get(id: string) { reads.push(id); return id === original.id ? original : promoted } }
    const result = verifyDeploymentPins(deployments, computerId, sessionId, checkedAtPreflight)
    if (promotedBefore === "neither") {
      expect(await result).toEqual({ computer: original, session: original })
    } else {
      await expect(result).rejects.toThrow(`${promotedBefore === "computer" ? "Computer" : "Session"} uses another deployment bundle`)
    }
    expect(reads).toEqual([computerId, sessionId])
  })
}
