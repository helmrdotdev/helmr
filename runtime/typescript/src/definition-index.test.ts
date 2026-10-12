import assert from "node:assert/strict"
import test from "node:test"
import { mkdtemp, mkdir, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { pathToFileURL } from "node:url"
import { loadAgentBundle, parseDefinitionIndex } from "./definition-index"

const computerLocator = { id: "computer", modulePath: "helmr/app/entry-0.mjs", exportName: "selected", throughAgent: true }
const locator = { id: "agent", templateDefinitionId: "computer", modulePath: "helmr/app/entry-0.mjs", exportName: "selected" }
test("Agent locators reject traversal and duplicate identity before importing", () => {
  for (const agents of [[locator, locator], [{ ...locator, modulePath: "../other.mjs" }], [{ ...locator, exportName: "" }]]) {
    assert.throws(() => parseDefinitionIndex({ apiVersion: "helmr.definition-index.v1", agents, templates: [computerLocator] }), /locator/)
  }
  assert.throws(() => parseDefinitionIndex({ apiVersion: "helmr.bundle.v0", agents: [locator], templates: [computerLocator] }), /index/)
})

test("Session loads only its selected compiled export and verifies its identity", async () => {
  const root = await mkdtemp(join(tmpdir(), "helmr-definition-index-"))
  try {
    await mkdir(join(root,"helmr/app"), { recursive: true })
    const index = pathToFileURL(join(root,"helmr/definition-index.json"))
    await writeFile(index, JSON.stringify({ apiVersion: "helmr.definition-index.v1", agents: [locator, { ...locator, id: "unused", modulePath: "helmr/app/entry-1.mjs" }], templates: [computerLocator] }))
    await writeFile(join(root,locator.modulePath), 'export const selected = {kind:"agent",id:"agent",template:{kind:"template",id:"computer"},turn:()=>42};')
    await writeFile(join(root,"helmr/app/entry-1.mjs"), 'throw new Error("unselected module was imported");')
    const definition = await loadAgentBundle(index,"agent")
    assert.equal(await definition.turn(undefined!,undefined!),42)
    await assert.rejects(loadAgentBundle(index,"absent"), /absent/)
    await writeFile(index, JSON.stringify({ apiVersion: "helmr.definition-index.v1", agents: [{ ...locator, templateDefinitionId: "wrong" }], templates: [{ ...computerLocator, id: "wrong" }] }))
    await assert.rejects(loadAgentBundle(index,"agent"), /differs/)
  } finally { await rm(root,{recursive:true,force:true}) }
})

test("compiler locators load inline preparation closures and the matching Agent", async () => {
  const { analyze } = await import("../../../compiler/typescript/src/compile")
  const { agent, template } = await import("../../../sdk/typescript/src/agent")
  const { image } = await import("../../../sdk/typescript/src/image")
  const { loadComputerBundle } = await import("./definition-index")
  const root = await mkdtemp(join(tmpdir(), "helmr-compiled-agent-"))
  try {
    await mkdir(join(root, "helmr/app"), { recursive: true })
    const declaration = agent({ id: "agent", template: template({ id: "computer", image: image("base").from("ubuntu:24.04"), resources: { cpu: 1, memory: "1GiB" }, prepare: () => {} }), turn: () => 42 })
    const analysis = analyze({ architecture: "x86_64", exports: [{ ...locator, value: declaration }] })
    const index = pathToFileURL(join(root, "helmr/definition-index.json"))
    await writeFile(index, analysis.definitionIndexBytes)
    await writeFile(join(root, "helmr/app/support.mjs"), 'export const command = "prepare-project";')
    await writeFile(join(root, locator.modulePath), 'import { command } from "./support.mjs"; export const selected={kind:"agent",id:"agent",template:{kind:"template",id:"computer",prepare:build=>build.exec(command)},turn:()=>42};')
    const prepared = await loadComputerBundle(index, "computer")
    const commands: unknown[] = []
    await prepared.prepare!({ signal: new AbortController().signal, exec: async command => { commands.push(command) } })
    assert.deepEqual(commands, ["prepare-project"])
    assert.equal((await loadAgentBundle(index, "agent")).template, prepared)
    await assert.rejects(loadComputerBundle(index, "other"), /absent/)
    const bad = JSON.parse(new TextDecoder().decode(analysis.definitionIndexBytes))
    bad.templates[0].modulePath = "../escape.mjs"
    await writeFile(index, JSON.stringify(bad))
    await assert.rejects(loadComputerBundle(index, "computer"), /locator/)
  } finally { await rm(root, { recursive: true, force: true }) }
})


test("both loaders validate the complete index and Computer placement", async () => {
  const { loadComputerBundle } = await import("./definition-index")
  const root = await mkdtemp(join(tmpdir(), "helmr-computer-bundle-"))
  try {
    await mkdir(join(root, "helmr/app"), { recursive: true })
    const index = pathToFileURL(join(root, "helmr/definition-index.json"))
    const valid = { apiVersion: "helmr.definition-index.v1", agents: [locator], templates: [computerLocator] }
    for (const computers of [[computerLocator, computerLocator], [{ ...computerLocator, throughAgent: "true" }], [{ ...computerLocator, id: "different" }], [{ ...computerLocator, exportName: "missing" }]]) {
      await writeFile(index, JSON.stringify({ ...valid, templates: computers }))
      await assert.rejects(loadAgentBundle(index, "agent"), /locator/)
      await assert.rejects(loadComputerBundle(index, "computer"), /locator/)
    }
    await writeFile(index, JSON.stringify({ ...valid, templates: [{ ...computerLocator, throughAgent: false }] }))
    await writeFile(join(root, locator.modulePath), 'export const selected={kind:"template",id:"computer",prepare:()=>{}};')
    assert.equal((await loadComputerBundle(index, "computer")).id, "computer")
    await writeFile(index, JSON.stringify(valid))
    await assert.rejects(loadComputerBundle(index, "computer"), /differs/)
    await writeFile(index, JSON.stringify({ ...valid, agents: [{ ...locator, modulePath: "helmr/app/entry-1.mjs" }], templates: [{ ...computerLocator, modulePath: "helmr/app/entry-1.mjs" }] }))
    await writeFile(join(root, "helmr/app/entry-1.mjs"), 'export const selected={kind:"agent",id:"agent",template:{kind:"template",id:"different"}};')
    await assert.rejects(loadComputerBundle(index, "computer"), /differs/)
  } finally { await rm(root, { recursive: true, force: true }) }
})
