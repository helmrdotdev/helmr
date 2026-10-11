import { verify, assertEqual, waitOutput, waitTurn, completedResult } from "../../support/context"

await verify("cancel-peer", async ({ marker, computer, startAgent }) => {
  const shared = await computer("helmr-helper-target-smoke", "cancel-peer")
  const start = async (suffix: string, holdSeconds: number) => {
    const started = await startAgent("helper-smoke-child", {
      computer: shared, input: { marker: `${marker}:${suffix}`, holdSeconds },
      idempotencyKey: `cancel-peer:${marker}:${suffix}`,
    })
    await waitOutput(started.session, started.turn, value => value !== null && typeof value === "object" && "phase" in value && value.phase === "helper-started")
    return started
  }
  const target = await start("target", 240)
  const peer = await start("peer", 45)
  await target.session.cancel({ idempotencyKey: `cancel:${marker}` })
  await waitTurn(target.turn, ["cancelled"], 120_000)
  assertEqual(await completedResult(peer.turn, 120_000), {
    marker: `${marker}:peer`, childTurnId: peer.turn.id, childSessionId: peer.session.id,
  }, "Cancelling a Session stopped its peer")
  return { cancelledSession: target.session.id, peerSession: peer.session.id, sharedComputer: shared.id }
})
