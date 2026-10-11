import type { InputContent } from "../../../sdk/typescript/src/content"
import { readFile, realpath } from "node:fs/promises"
import { dirname, isAbsolute, relative, resolve } from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"
import type { AgentDefinition, TemplateDefinition, Json } from "@helmr/sdk"
import type { DefinitionIndex } from "@helmr/sdk/internal"

export type { DefinitionIndex } from "@helmr/sdk/internal"

function record(value: unknown, keys: readonly string[]): value is Record<string, unknown> {
  return !!value && typeof value === "object" && !Array.isArray(value) && Object.keys(value).length === keys.length && Object.keys(value).every(key => keys.includes(key))
}
function id(value: unknown): value is string {
  return typeof value === "string" && /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(value)
}
function locator(value: Record<string, unknown>): boolean {
  return typeof value["modulePath"] === "string" && /^helmr\/app\/entry-[0-9]+\.mjs$/.test(value["modulePath"]) && typeof value["exportName"] === "string" && new TextEncoder().encode(value["exportName"]).length >= 1 && new TextEncoder().encode(value["exportName"]).length <= 256 && !/[\p{Cc}\p{Cs}]/u.test(value["exportName"])
}
export function parseDefinitionIndex(value: unknown): DefinitionIndex {
  if (!record(value, ["apiVersion", "agents", "templates"]) || value["apiVersion"] !== "helmr.definition-index.v1" || !Array.isArray(value["agents"]) || !Array.isArray(value["templates"]) || value["agents"].length + value["templates"].length === 0 || value["agents"].length + value["templates"].length > 10_000) throw new Error("Invalid Definition index")
  const agents = new Set<string>(), templates = new Set<string>()
  for (const agent of value["agents"]) {
    if (!record(agent, ["id", "templateDefinitionId", "modulePath", "exportName"]) || !id(agent["id"]) || !id(agent["templateDefinitionId"]) || !locator(agent) || agents.has(agent["id"])) throw new Error("Invalid or duplicate Agent bundle locator")
    agents.add(agent["id"])
  }
  for (const computer of value["templates"]) {
    if (!record(computer, ["id", "modulePath", "exportName", "throughAgent"]) || !id(computer["id"]) || !locator(computer) || typeof computer["throughAgent"] !== "boolean" || templates.has(computer["id"])) throw new Error("Invalid or duplicate Computer bundle locator")
    if (computer["throughAgent"] && !value["agents"].some(agent => agent["modulePath"] === computer["modulePath"] && agent["exportName"] === computer["exportName"] && agent["templateDefinitionId"] === computer["id"])) throw new Error("Computer bundle locator has no matching Agent")
    templates.add(computer["id"])
  }
  if (value["agents"].some(agent => !templates.has(agent["templateDefinitionId"]))) throw new Error("Agent bundle locator has no matching Computer")
  return value as unknown as DefinitionIndex
}

// The managed Node preload enforces module provenance for the whole lifetime.
// Each Session imports only the selected, compiler-verified export. Node's
// module cache and the Session owner retain it through transport renewal/RAM.
export async function loadAgentBundle(indexURL: URL, agentId: string): Promise<AgentDefinition<InputContent, Json, unknown>> {
  const indexPath = fileURLToPath(indexURL)
  const index = parseDefinitionIndex(JSON.parse(await readFile(indexPath, "utf8")))
  const selected = index.agents.find(agent => agent.id === agentId)
  if (!selected) throw new Error("Agent is absent from the verified bundle")
  const namespace = await loadExport(indexPath, selected.modulePath)
  const definition = namespace[selected.exportName] as Partial<AgentDefinition> | undefined
  if (definition?.kind !== "agent" || definition.id !== selected.id || definition.template?.kind !== "template" || definition.template.id !== selected.templateDefinitionId || typeof definition.turn !== "function" || (definition.init !== undefined && typeof definition.init !== "function")) throw new Error("Agent export differs from the verified bundle")
  return definition as AgentDefinition<InputContent, Json, unknown>
}

async function loadExport(indexPath: string, selectedModulePath: string): Promise<Record<string, unknown>> {
  const root = await realpath(dirname(dirname(indexPath)))
  const modulePath = await realpath(resolve(root, selectedModulePath))
  const location = relative(root, modulePath)
  if (location === ".." || location.startsWith("../") || isAbsolute(location)) throw new Error("Agent module escapes the verified bundle")
  return await import(pathToFileURL(modulePath).href) as Record<string, unknown>
}

// Preparation imports the same bundled code as execution, including supporting
// modules and closures. Compilation records where to find it, not its source text.
export async function loadComputerBundle(indexURL: URL, templateDefinitionId: string): Promise<TemplateDefinition> {
  const indexPath = fileURLToPath(indexURL)
  const index = parseDefinitionIndex(JSON.parse(await readFile(indexPath, "utf8")))
  const selected = index.templates.find(computer => computer.id === templateDefinitionId)
  if (!selected) throw new Error("Computer is absent from the verified bundle")
  const namespace = await loadExport(indexPath, selected.modulePath)
  const exported = namespace[selected.exportName] as Partial<AgentDefinition> | Partial<TemplateDefinition> | undefined
  const definition = selected.throughAgent ? (exported as Partial<AgentDefinition> | undefined)?.template : exported as Partial<TemplateDefinition> | undefined
  if ((selected.throughAgent && (exported as Partial<AgentDefinition> | undefined)?.kind !== "agent") || definition?.kind !== "template" || definition.id !== templateDefinitionId || (definition.prepare !== undefined && typeof definition.prepare !== "function")) throw new Error("Computer export differs from the verified bundle")
  return definition as TemplateDefinition
}
