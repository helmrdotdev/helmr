import { canonicalizeJsonValue, type JsonValue, type RuntimeArchitecture } from "@helmr/sdk/internal"
import type { DiscoveryConfig } from "./config"
import { createHash } from "node:crypto"
import { readFile, realpath } from "node:fs/promises"
import { resolve } from "node:path"
import { pathToFileURL } from "node:url"
import { installModulePolicy } from "../../../runtime/typescript/src/module-policy"
import { BUNDLE_API_VERSION, bundleIdentity, type BundleManifest } from "./bundle"
import { analyze, type AnalysisExport } from "./compile"
import { compareUTF8 } from "./utf8"

export const COMPILER_API_VERSION = "helmr.compiler.v0" as const
export function compilerContract() {
  return { apiVersion: COMPILER_API_VERSION, bundler: bundleIdentity() }
}

export async function compileProgram(options: {
  architecture: RuntimeArchitecture
  config: DiscoveryConfig
  nodeVersion: string
  payloadDigest: string
  bundlePath: string
  root: string
}) {
  if (process.versions.node !== options.nodeVersion) throw new Error(`Program Compiler Node version ${process.versions.node} does not match ${options.nodeVersion}`)
  if (!/^sha256:[0-9a-f]{64}$/.test(options.payloadDigest)) throw new Error("Program payload digest is invalid")
  const root = await realpath(options.root)
  const bundle = JSON.parse(await readFile(options.bundlePath, "utf8")) as BundleManifest
  if (bundle.apiVersion !== BUNDLE_API_VERSION || !Array.isArray(bundle.entries) || bundle.entries.length === 0) throw new Error("bundle manifest is invalid")
  const modules = bundle.entries.map(entry => entry.modulePath)
  if (new Set(modules).size !== modules.length || modules.some(path => !/^helmr\/app\/entry-[0-9]+\.mjs$/.test(path))) throw new Error("bundle entries are invalid")
  const contract = compilerContract()
  const policy = installModulePolicy({ root })
  try {
    const exports: AnalysisExport[] = []
    for (const modulePath of modules) {
      const namespace = await import(pathToFileURL(resolve(root, modulePath)).href) as Record<string, unknown>
      for (const exportName of Object.keys(namespace).sort(compareUTF8)) {
        exports.push({ modulePath, exportName, value: namespace[exportName] })
      }
    }
    const analysis = analyze({ architecture: options.architecture, exports })
    const configBytes = canonicalizeJsonValue(options.config as unknown as JsonValue)
    const result = {
      ...contract,
      nodeVersion: options.nodeVersion,
      config: { path: "helmr/config.json", digest: `sha256:${createHash("sha256").update(configBytes).digest("hex")}` },
      payloadDigest: options.payloadDigest,
      modules: [...modules].sort(compareUTF8),
      selections: analysis.declarationLocator.declarations,
    }
    return { analysis, modules, files: new Map([
      ["helmr/config.json", configBytes],
      ["helmr/compiler-result.json", canonicalizeJsonValue(result as unknown as JsonValue)],
    ]) }
  } finally { policy.deregister() }
}
