import { setTimeout as delay } from "node:timers/promises"
import { verify, assert, deadline } from "../../support/context"
import { hostObservation } from "../../support/host-observation"
import { replyFault } from "../../support/reply-fault"

// Deploy cases/control-plane-outage's minimal Computer, then run this preflight
// before restarting the fault proxy and running capture-abort with reply loss.
await verify("reply-relay", async ({ client, computer, marker, cleanup }) => {
  const shared = await computer("verification-outage")
  const ref = client.computers.ref(shared.id)
  const warm = await ref.exec({ command: ["true"], idempotencyKey: `${marker}:warm` })
  assert.equal((await warm.wait({ signal: deadline(300_000) })).exitCode, 0)
  const before = await hostObservation("computer-path", { computer_id: shared.id })
  const sources = before.leases.filter((lease: any) => lease.status === "active" && lease.fenced_at === null)
  assert.equal(sources.length, 1)
  const source = sources[0]
  await replyFault("POST", { mode: "passthrough", computer_id: shared.id, instance_id: source.computer_instance_id })
  cleanup(async () => { await replyFault("DELETE") })
  const command = await ref.exec({ command: ["sh", "-ceu", 'sleep 15; printf %s "$MARKER"'],
    env: { MARKER: marker }, idempotencyKey: `${marker}:relayed` })
  cleanup(async () => {
    if (["pending", "starting", "running"].includes((await command.retrieve()).status)) await command.cancel()
  })
  const signal = deadline(60_000)
  let during
  for (;;) {
    signal.throwIfAborted()
    during = await replyFault()
    assert.equal(during.failure, "")
    if (during.relay && during.relay.active_streams > 0 && during.relay.forwarded_streams > 0 &&
      (await command.retrieve()).status === "running") break
    await delay(200, undefined, { signal })
  }
  const restoredListener = await replyFault("DELETE")
  assert.equal(restoredListener.relay?.restored, true)
  assert(restoredListener.relay.active_streams > 0, "no established stream survived listener restoration")
  const result = await command.wait({ signal: deadline(60_000) })
  assert.equal(result.exitCode, 0)
  let output = ""
  for await (const event of command.streamLogs({}, { signal: deadline(30_000) })) {
    assert(event.kind !== "gap")
    if (event.stream === "stdout") output += new TextDecoder().decode(event.content)
  }
  assert.equal(output, marker)
  const after = await hostObservation("computer-path", { computer_id: shared.id })
  assert.equal(after.commands.find((c: any) => c.id === command.id)?.computer_lease_epoch, source.epoch)
  const final = await replyFault()
  assert.equal(final.failure, "")
  return { before, during, restoredListener, result, output, after, final }
})
