// Invoked by the guestd launch test under the Runtime's Program flags: loads the
// project's generated task module and calls one
// declared task's handler. The guest task protocol is not involved.
import { readFile } from "node:fs/promises"
const index = JSON.parse(await readFile(new URL("../helmr/declarations.json", import.meta.url), "utf8"))
const namespaces = await Promise.all([...new Set(index.declarations.filter(item => item.kind === "task").map(item => item.locator.modulePath))]
  .map(path => import(new URL(`../${path}`, import.meta.url).href)))
const definitions = namespaces.flatMap(namespace => Object.values(namespace))
const brand = Symbol.for("helmr.sdk.v0.definition")
const id = process.argv[2]
const declared = definitions.map(value => value?.[brand]).filter(definition => definition?.kind === "task")
const selected = declared.find(definition => definition.id === id)
if (!selected) {
  throw new Error(`task ${id} is not declared; found ${declared.map(definition => definition.id).join(", ")}`)
}
const output = await selected.handler(undefined, { run: { id: "guestd-launch-test" } })
process.stdout.write(`${JSON.stringify({ task: id, output })}\n`)
