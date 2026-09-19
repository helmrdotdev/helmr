import { canonicalizeJsonValue, type JsonValue, type RuntimeArchitecture } from "@helmr/sdk/internal"
import type { DiscoveryConfig } from "./config"
import { installModuleExecution, moduleExecutionIdentity } from "@helmr/module-execution"
import { createHash } from "node:crypto"
import { realpath } from "node:fs/promises"
import { resolve } from "node:path"
import { pathToFileURL } from "node:url"
import { discoverModules } from "./analysis"
import { analyze, type AnalysisExport } from "./compile"
import { compareUTF8 } from "./utf8"

export const COMPILER_API_VERSION = "helmr.compiler.v0" as const
export function compilerContract() {
  return { apiVersion: COMPILER_API_VERSION, language: moduleExecutionIdentity() }
}

export async function compileProgram(options: {
  architecture: RuntimeArchitecture
  config: DiscoveryConfig
  nodeVersion: string
  inputTreeDigest: string
  root: string
}) {
  if (process.versions.node !== options.nodeVersion) throw new Error(`Program Compiler Node version ${process.versions.node} does not match ${options.nodeVersion}`)
  if (!/^sha256:[0-9a-f]{64}$/.test(options.inputTreeDigest)) throw new Error("Program Compiler input tree digest is invalid")
  const root = await realpath(options.root)
  const language = moduleExecutionIdentity()
  const execution = installModuleExecution({ root })
  try {
    const modules = await discoverModules(root, options.config)
    if (modules.length === 0) throw new Error("configured dirs contain no declaration source modules")
    const exports: AnalysisExport[] = []
    for (const sourcePath of modules) {
      const namespace = await execution.importSourceExports(pathToFileURL(resolve(root, sourcePath)))
      for (const exportName of Object.keys(namespace).sort(compareUTF8)) {
        exports.push({ sourcePath, exportName, value: namespace[exportName] })
      }
    }
    const analysis = analyze({ architecture: options.architecture, exports })
    const configBytes = canonicalizeJsonValue(options.config as unknown as JsonValue)
    const result = {
      apiVersion: COMPILER_API_VERSION,
      language,
      nodeVersion: options.nodeVersion,
      config: { path: "helmr/config.json", digest: `sha256:${createHash("sha256").update(configBytes).digest("hex")}` },
      inputTreeDigest: options.inputTreeDigest,
      discoveryCandidates: modules,
      selections: analysis.declarationLocator.declarations,
    }
    return { analysis, modules, files: new Map([
      ["helmr/config.json", configBytes],
      ["helmr/compiler-result.json", canonicalizeJsonValue(result as unknown as JsonValue)],
    ]) }
  } finally { execution.dispose() }
}
