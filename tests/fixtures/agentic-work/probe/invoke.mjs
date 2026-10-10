// Invoked by the guestd launch test under the Runtime Program flags. It imports
// the selected Agent and exercises its tools directly, without Session protocol.
import { readFile } from "node:fs/promises"
const index = JSON.parse(await readFile(new URL("../helmr/definition-index.json", import.meta.url), "utf8"))
const id = process.argv[2]
const locator = index.agents.find(definition => definition.id === id)
if (!locator) throw new Error(`Agent ${id} is not declared`)
const selected = (await import(new URL(`../${locator.modulePath}`, import.meta.url).href))[locator.exportName]
const signal = new AbortController().signal
const output = await selected.turn({ id: "tool-probe", input: null, signal }, { signal })
process.stdout.write(`${JSON.stringify({ agent: id, output })}\n`)
