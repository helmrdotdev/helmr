// Invoked by the guestd launch test under the Runtime's Program flags: loads the
// project's task module through the Runtime's language adapter and calls one
// declared task's handler. The guest task protocol is not involved.
const definitions = await import("../tasks/agent.ts")
const brand = Symbol.for("helmr.sdk.v0.definition")
const id = process.argv[2]
const declared = Object.values(definitions).map(value => value?.[brand]).filter(definition => definition?.kind === "task")
const selected = declared.find(definition => definition.id === id)
if (!selected) {
  throw new Error(`task ${id} is not declared; found ${declared.map(definition => definition.id).join(", ")}`)
}
const output = await selected.handler(undefined, { run: { id: "guestd-launch-test" } })
process.stdout.write(`${JSON.stringify({ task: id, output })}\n`)
