import { cleanupChildren } from "./cleanup"
import { verify, assert, assertEqual, completedResult, waitAsk } from "../../support/context"
await verify("helpers", async ({ client, marker, objects, computer, cleanup, startAgent }) => {
  const target = await computer("helmr-helper-target-smoke", "target")
  for (const mode of ["spawn-success", "same-computer", "spawn-failure", "start-independent"] as const) {
    const caller = await computer("helmr-helper-caller-smoke", mode)
    const parent = await startAgent("helper-smoke", {
      computer: caller,
      input: { mode, marker, childComputerId: target.id },
      idempotencyKey: `helper:${mode}:${marker}`,
    })
    cleanup(() => cleanupChildren(client, parent.session.id, [caller.id, target.id], objects.session_ids))
    const result = await completedResult(parent.turn)
    assert(result !== null && typeof result === "object" && !Array.isArray(result))
    const output = result as Record<string, unknown>
    assertEqual(output.mode, mode, "Helper mode changed")
    assertEqual(output.marker, marker, "Helper marker changed")
    assert(typeof output.childTurnId === "string" && typeof output.childSessionId === "string", "Missing helper identity")
    objects.turn_ids.push(output.childTurnId)
    objects.session_ids.push(output.childSessionId)
    const child = client.sessions.get(output.childSessionId)
    const childState = await child.retrieve()
    assertEqual(childState.computerId, mode === "same-computer" ? caller.id : target.id, "Helper placement changed")
    assertEqual(childState.parentSessionId, mode === "start-independent" ? null : parent.session.id, "Helper ownership changed")
    assertEqual(childState.requesterSessionId, parent.session.id, "Helper requester was lost")
    if (mode === "same-computer") assertEqual(output.sameComputerMarkerObserved, true, "Parent lost helper writes")
    if (mode === "spawn-failure") assert(output.childFailure !== null, "Helper failure was lost")
    if (mode === "start-independent") {
      const question = await waitAsk(child.turn(output.childTurnId))
      objects.ask_ids.push(question.id)
      await parent.session.cancel({ idempotencyKey: `stop-parent:${marker}` })
      assertEqual((await child.turn(output.childTurnId).asks.get(question.id)).status, "pending", "Parent cancellation cancelled its independent helper")
      await child.turn(output.childTurnId).asks.respond(question.id, { answer: "Finish", responseId: `finish-helper:${marker}` })
      const result = await completedResult(child.turn(output.childTurnId))
      assert(result !== null && typeof result === "object" && "marker" in result && result.marker === marker)
    }
  }
  return { sameComputer: true, differentComputer: true, helperFailure: true, independentOutlivesParent: true }
})
