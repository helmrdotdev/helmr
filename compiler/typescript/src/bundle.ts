import { createHash } from "node:crypto"
import { readFileSync } from "node:fs"
import { createRequire } from "node:module"
import { build, version as esbuildVersion } from "esbuild"
import { canonicalizeJsonValue, type JsonValue } from "@helmr/sdk/internal"
import { copyFile, glob, lstat, mkdir, readFile, realpath, writeFile } from "node:fs/promises"
import { dirname, isAbsolute, relative, resolve } from "node:path"
import npa from "npm-package-arg"
import { satisfies, valid } from "semver"
import { parse as parseYaml } from "yaml"
import { discoverModules } from "./analysis"
import type { DiscoveryConfig } from "./config"

export const BUNDLE_API_VERSION = "helmr.bundle.v0" as const
export interface BundleManifest {
  readonly apiVersion: typeof BUNDLE_API_VERSION
  readonly entries: readonly { readonly sourcePath: string; readonly modulePath: string }[]
}

export function bundleIdentity() {
  const require = createRequire(import.meta.url)
  const digest = (name: string) => `sha256:${createHash("sha256").update(readFileSync(name === "esbuild" ? require.resolve(name) : createRequire(require.resolve("esbuild")).resolve(name))).digest("hex")}`
  return { apiVersion: BUNDLE_API_VERSION, esbuildVersion, apiDigest: digest("esbuild"), binaryDigest: digest(`@esbuild/${process.platform}-${process.arch}/bin/esbuild`) }
}

// Runs only in the restricted compiler filesystem. Configuration reads are owned
// by upstream esbuild; the enclosing build stage supplies their input boundary.
export async function bundleProgram(options: {
  root: string
  output: string
  config: DiscoveryConfig
  nodeVersion: string
}): Promise<BundleManifest> {
  const root = await realpath(options.root)
  const output = resolve(options.output)
  if (contained(root, output)) throw new Error("bundle output must be outside captured inputs")
  const modules = await discoverModules(root, options.config)
  if (modules.length === 0) throw new Error("configured dirs contain no declaration source modules")
  const sourcePackage = await readObject(resolve(root, "package.json"))
  const dependencies = await runtimeDependencies(root, sourcePackage, options.config.external)
  const payload = resolve(output, "payload")
  const outdir = resolve(payload, "helmr/app")
  await mkdir(outdir, { recursive: true })
  const result = await build({
    absWorkingDir: root,
    entryPoints: modules.map((sourcePath, index) => ({ in: sourcePath, out: `entry-${index}` })),
    bundle: true,
    packages: "bundle",
    external: [...options.config.external],
    platform: "node",
    format: "esm",
    target: `node${options.nodeVersion}`,
    splitting: true,
    sourcemap: "linked",
    sourcesContent: true,
    outdir,
    outExtension: { ".js": ".mjs" },
    entryNames: "[name]",
    chunkNames: "chunks/[name]-[hash]",
    metafile: true,
    write: false,
    logOverride: { "tsconfig.json": "error" },
    banner: { js: 'import { createRequire as __helmrCreateRequire } from "node:module"; const require = __helmrCreateRequire(import.meta.url);' },
  })
  const configPath = await realpath(resolve(root, "helmr.config.ts")).catch(error => {
    if (error.code === "ENOENT") return undefined
    throw error
  })
  for (const input of Object.keys(result.metafile.inputs)) {
    const path = await realpath(resolve(root, input)).catch(error => {
      if (error.code !== "ENOENT" || !/[?#]/.test(input)) throw error
      return realpath(resolve(root, input.split(/[?#]/, 1)[0]!))
    })
    if (!contained(root, path)) throw new Error(`bundle input escapes captured project: ${input}`)
    if (path === configPath) throw new Error("helmr.config.ts is build-only and cannot be imported by Program modules")
  }
  for (const file of result.outputFiles) {
    if (!contained(outdir, file.path)) throw new Error("generated file escapes bundle output")
    await mkdir(dirname(file.path), { recursive: true })
    await writeFile(file.path, file.contents, { flag: "wx" })
  }
  const entries = modules.map((sourcePath, index) => ({ sourcePath, modulePath: `helmr/app/entry-${index}.mjs` }))
  for (const name of options.config.external) {
    const manifest = await readObject(await projectFile(root, `node_modules/${name}/package.json`))
    for (const peer of Object.keys(object(manifest["peerDependencies"] ?? {}, "peerDependencies"))) {
      if (Object.keys(result.metafile.inputs).some(path => path.includes(`node_modules/${peer}/`))) {
        process.stderr.write(`Package ${name} shares bundled peer ${peer}; add ${peer} to build.external if they must share one runtime instance.\n`)
      }
    }
  }
  await copyAssets(root, payload, options.config.assets)
  const runtimePackage: Record<string, unknown> = { private: true, dependencies }
  for (const key of ["name", "type", "imports", "exports"]) {
    if (Object.hasOwn(sourcePackage, key)) runtimePackage[key] = sourcePackage[key]
  }
  await writeJSON(resolve(payload, "package.json"), runtimePackage)
  await mkdir(resolve(output, "install"))
  await writeJSON(resolve(output, "install/package.json"), {
    private: true, dependencies,
    ...(sourcePackage["overrides"] === undefined ? {} : { overrides: sourcePackage["overrides"] }),
  })
  const manifest: BundleManifest = { apiVersion: BUNDLE_API_VERSION, entries }
  await writeJSON(resolve(output, "bundle.json"), manifest)
  return manifest
}

async function runtimeDependencies(root: string, source: Record<string, unknown>, names: readonly string[]) {
  const dependencies: Record<string, string> = {}
  if (names.length === 0) return dependencies
  for (const key of ["resolutions", "patchedDependencies"]) {
    if (source[key] !== undefined) throw new Error(`build.external does not support ${key}`)
  }
  const pnpm = object(source["pnpm"] ?? {}, "pnpm")
  for (const key of ["overrides", "patchedDependencies"]) {
    if (pnpm[key] !== undefined) throw new Error(`build.external does not support pnpm.${key}`)
  }
  for (const name of [".pnpmfile.cjs", ".pnpmfile.js"]) {
    if (await optionalFile(root, name) !== undefined) throw new Error(`build.external does not support ${name}`)
  }
  const workspace = await optionalFile(root, "pnpm-workspace.yaml")
  if (workspace !== undefined) {
    const settings = object(parseYaml(workspace) ?? {}, "pnpm-workspace.yaml")
    if (settings["overrides"] !== undefined || settings["patchedDependencies"] !== undefined) throw new Error("build.external does not support workspace overrides or patches")
  }
  // Registry authentication is not inferred from project files or forwarded to
  // generated metadata. Refuse explicit non-public registry configuration.
  const npmrc = await optionalFile(root, ".npmrc")
  if (npmrc !== undefined && npmrc.split(/\r?\n/).some(line => /^\s*(?:@[^:]+:)?registry\s*=/.test(line) && !/^\s*(?:@[^:]+:)?registry\s*=\s*https:\/\/registry\.npmjs\.org\/?\s*$/.test(line))) {
    throw new Error("build.external requires the supported public npm registry")
  }
  for (const name of names) {
    const declarations = ["dependencies", "devDependencies", "optionalDependencies"].flatMap(key => {
      const values = object(source[key] ?? {}, key)
      return Object.hasOwn(values, name) ? [values[name]] : []
    })
    if (declarations.length !== 1 || typeof declarations[0] !== "string") throw new Error(`external package ${name} must have one root dependency declaration`)
    const spec = npa.resolve(name, declarations[0], root)
    if (spec.type !== "version" && spec.type !== "range") throw new Error(`external package ${name} must use a registry semver declaration`)
    const installed = await readObject(await projectFile(root, `node_modules/${name}/package.json`))
    const version = installed["version"]
    if (installed["name"] !== name || typeof version !== "string" || valid(version) === null || !satisfies(version, declarations[0])) throw new Error(`installed external ${name} does not match its root declaration`)
    dependencies[name] = version
  }
  return dependencies
}

async function copyAssets(root: string, output: string, patterns: readonly string[]) {
  const selected = new Set<string>()
  for (const pattern of patterns) {
    let matches = 0
    for await (const path of glob(pattern, { cwd: root })) {
      const info = await lstat(resolve(root, path))
      if (info.isDirectory()) continue
      await projectFile(root, path, true)
      if (path === "package.json" || path === "package-lock.json" || path.split("/").some(part => part === "node_modules") || path.split("/")[0] === "helmr") throw new Error(`asset collides with managed Program files: ${path}`)
      matches++
      selected.add(path)
    }
    if (matches === 0) throw new Error(`asset pattern ${JSON.stringify(pattern)} matches no files in captured inputs; check its path and .helmrignore`)
  }
  for (const path of [...selected].sort()) {
    const target = resolve(output, path)
    if (!contained(output, target)) throw new Error(`asset escapes Program: ${path}`)
    await mkdir(dirname(target), { recursive: true })
    await copyFile(resolve(root, path), target)
  }
}

export function contained(root: string, path: string): boolean {
  const value = relative(root, path)
  return value !== ".." && !value.startsWith("../") && !isAbsolute(value)
}
async function projectFile(root: string, name: string, rejectLinks = false): Promise<string> {
  const path = resolve(root, name)
  if (!contained(root, path)) throw new Error(`file escapes captured project: ${name}`)
  if (rejectLinks) {
    let prefix = root
    for (const part of relative(root, path).split("/")) {
      prefix = resolve(prefix, part)
      if ((await lstat(prefix)).isSymbolicLink()) throw new Error(`asset cannot cross a symbolic link: ${name}`)
    }
  }
  const canonical = await realpath(path)
  if (!contained(root, canonical) || !(await lstat(canonical)).isFile()) throw new Error(`file must be a contained regular file: ${name}`)
  return canonical
}
async function optionalFile(root: string, name: string): Promise<string | undefined> {
  try { return await readFile(await projectFile(root, name), "utf8") }
  catch (error) { if ((error as NodeJS.ErrnoException).code === "ENOENT") return undefined; throw error }
}
function object(value: unknown, label: string): Record<string, unknown> {
  if (value === null || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label} must be an object`)
  return value as Record<string, unknown>
}
async function readObject(path: string) { return object(JSON.parse(await readFile(path, "utf8")), path) }
async function writeJSON(path: string, value: unknown) { await writeFile(path, canonicalizeJsonValue(value as JsonValue), { flag: "wx" }) }
