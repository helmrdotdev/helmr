import { createHash } from "node:crypto"
import { existsSync, readFileSync, statSync } from "node:fs"
import { isBuiltin, registerHooks } from "node:module"
import type { ResolveHookContext, ResolveHookSync, ResolveFnOutput } from "node:module"
import { dirname, extname, join, resolve } from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"
import ts from "typescript"
import { typescriptVersion } from "./runtime-dependencies"
import { SourceAuthority, diagnosticError, isContained } from "./authority"

export const languageVersion = "helmr.module-execution.v0" as const
export { typescriptVersion } from "./runtime-dependencies"
export const sourceExtensions = [".js", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts", ".jsx"] as const
const typed = (path: string) => /\.(?:[cm]?ts|tsx|jsx)$/.test(path)
const absent = (error: unknown) => error instanceof Error && "code" in error &&
  (error.code === "ERR_MODULE_NOT_FOUND" || error.code === "MODULE_NOT_FOUND" || error.code === "ERR_UNSUPPORTED_DIR_IMPORT")

export interface ModuleExecutionOptions {
  root: string
  // The verified platform installation; only the fixed bootstrap files below
  // can load outside Program, and only from a platform parent/Node entry.
  platformRoot?: string
}

export function installModuleExecution(options: ModuleExecutionOptions): ModuleExecution {
  if (activeExecution !== undefined) throw new Error("Helmr module execution is already installed")
  if (ts.version !== typescriptVersion) throw new Error(`Helmr requires TypeScript ${typescriptVersion}, got ${ts.version}`)
  const authority = new SourceAuthority(options.root)
  authority.assertEntry()
  const configPath = join(authority.root, "helmr.config.ts")
  const rootConfig = existsSync(configPath) ? authority.file(configPath) : undefined
  const platformFiles = new Set(options.platformRoot === undefined ? [] : [
    resolve(options.platformRoot, "helmr/entry.mjs"),
    resolve(options.platformRoot, "moduleexecution/loader.mjs"),
    resolve(options.platformRoot, "moduleexecution/typescript.cjs"),
  ])
  const source = (path: string) => {
    const canonical = authority.file(path)
    // helmr.config.ts is evaluated once on the build host; managed modules never import it.
    if (canonical === rootConfig) {
      throw new Error(`helmr.config.ts is build-only and cannot be imported by Program modules: ${path}`)
    }
    return canonical
  }

  type Next = Parameters<ResolveHookSync>[2]
  function requestURL(specifier: string, context: ResolveHookContext): URL {
    if (context.conditions.includes("require") && !specifier.startsWith("file:")) {
      // require treats #, % and ? as literal filename characters. Decode only
      // the parent URL, then resolve the requested filesystem path.
      const directory = context.parentURL === undefined ? authority.root : dirname(fileURLToPath(context.parentURL))
      return pathToFileURL(resolve(directory, specifier))
    }
    return new URL(specifier, context.parentURL)
  }

  function candidate(specifier: string, url: URL, context: ResolveHookContext, next: Next): ResolveFnOutput {
    const path = authority.contained(fileURLToPath(url))
    try {
      return next(specifier, context)
    } catch (error) {
      if (!absent(error)) throw error
      // If a package directory exists, its broken main/exports/config is a real
      // resolution failure. Only native ESM's unsupported directory case may
      // fall through to an index, and never past a package.json.
      if (existsSync(path)) {
        if (!statSync(path).isDirectory() || existsSync(authority.contained(join(path, "package.json")))) throw error
      }
      const extension = extname(path)
      const candidates: string[] = []
      const replacement = { ".js": ".ts", ".mjs": ".mts", ".cjs": ".cts" }[extension]
      if (replacement !== undefined) candidates.push(path.slice(0, -extension.length) + replacement)
      if (extension === "") {
        candidates.push(...sourceExtensions.map((extension) => path + extension))
        candidates.push(...sourceExtensions.map((extension) => join(path, "index" + extension)))
      }
      for (const value of candidates) {
        authority.contained(value)
        if (!existsSync(value) || !statSync(value).isFile()) continue
        source(value)
        const target = pathToFileURL(value)
        target.search = url.search
        target.hash = url.hash
        // Node's require resolver accepts filesystem paths, not file: URL
        // specifiers. ESM keeps URL query/fragment identity.
        return next(context.conditions.includes("require") ? value : target.href, context)
      }
      throw error
    }
  }

  const formats = new Map<string, string | undefined>()
  const hooks = registerHooks({
    resolve(specifier, context, next) {
      if (isBuiltin(specifier)) return next(specifier, context)
      const parent = context.parentURL?.startsWith("file:") ? fileURLToPath(context.parentURL) : undefined
      const customer = parent !== undefined && isContained(authority.root, parent)
      const trusted = context.parentURL === undefined || (parent !== undefined && platformFiles.has(parent))
      let result: ResolveFnOutput | undefined
      if (customer && bareAlias(specifier)) {
        for (const path of aliases(specifier, authority.config(parent))) {
          authority.contained(path)
          try {
            const url = pathToFileURL(path)
            result = candidate(context.conditions.includes("require") ? path : url.href, url, context, next)
            break
          } catch (error) {
            // An existing candidate's invalid package target is not an absent
            // alias. Keep that failure instead of picking another package.
            if (!absent(error) || existsSync(path)) throw error
          }
        }
      }
      if (result === undefined) {
        if (specifier.startsWith(".") || specifier.startsWith("/") || specifier.startsWith("file:")) {
          const url = requestURL(specifier, context)
          const target = fileURLToPath(url)
          if (!trusted || !platformFiles.has(target)) authority.contained(target)
          result = customer ? candidate(specifier, url, context, next) : next(specifier, context)
        } else result = next(specifier, context)
      }
      if (result.url.startsWith("file:")) {
        const path = fileURLToPath(result.url)
        if (!trusted || !platformFiles.has(path)) source(path)
      }
      return result
    },
    load(url, context, next) {
      if (!url.startsWith("file:")) return next(url, context)
      const path = fileURLToPath(url)
      if (platformFiles.has(path)) return next(url, context)
      const canonical = source(path)
      if (!typed(canonical)) {
        const loaded = next(url, context)
        formats.set(url, loaded.format ?? undefined)
        return loaded
      }
      if (/\.d\.(?:[cm]?ts)$/.test(canonical)) throw new Error(`Type declarations are not executable Program source: ${canonical}`)
      const format = authority.format(canonical)
      formats.set(url, format)
      const result = ts.transpileModule(readFileSync(canonical, "utf8"), {
        fileName: canonical,
        compilerOptions: emitOptions(authority.config(canonical), format),
        reportDiagnostics: true,
      })
      const errors = result.diagnostics?.filter((diagnostic) => diagnostic.category === ts.DiagnosticCategory.Error) ?? []
      if (errors.length > 0) throw diagnosticError(errors)
      return { format, source: result.outputText, shortCircuit: true }
    },
  })

  const execution: ModuleExecution = {
    root: authority.root,
    rootConfig,
    configReads: authority.configReads,
    dispose: () => { hooks.deregister(); if (activeExecution === execution) activeExecution = undefined },
    async importSourceExports(url: URL): Promise<Record<string, unknown>> {
      source(fileURLToPath(url))
      const namespace = await import(url.href) as Record<string, unknown>
      if (formats.get(url.href) !== "commonjs") return namespace
      const value = namespace["default"]
      if (value !== null && (typeof value === "object" || typeof value === "function")) {
        const exports = value as Record<string, unknown>
        return { ...exports, default: exports["__esModule"] === true ? exports["default"] : value }
      }
      return { default: value }
    },
  }
  activeExecution = execution
  return execution
}

function bareAlias(specifier: string): boolean {
  return !specifier.startsWith(".") && !specifier.startsWith("/") && !specifier.startsWith("#") && !specifier.includes(":")
}

function aliases(specifier: string, options: ts.CompilerOptions): string[] {
  const paths = options.paths ?? {}
  let key = Object.hasOwn(paths, specifier) ? specifier : undefined
  let wildcard = ""
  if (key === undefined) {
    const keys = Object.keys(paths).filter((value) => value.includes("*")).sort((a, b) => b.indexOf("*") - a.indexOf("*") || b.length - a.length)
    for (const value of keys) {
      const [prefix = "", suffix = ""] = value.split("*")
      if (!specifier.startsWith(prefix) || !specifier.endsWith(suffix) || specifier.length < prefix.length + suffix.length) continue
      key = value
      wildcard = specifier.slice(prefix.length, specifier.length - suffix.length)
      break
    }
  }
  const base = options.baseUrl ?? options["pathsBasePath"]
  if (key !== undefined && typeof base === "string") return paths[key]!.map((value) => resolve(base, value.replace("*", wildcard)))
  return key === undefined && options.baseUrl !== undefined ? [resolve(options.baseUrl, specifier)] : []
}

function emitOptions(options: ts.CompilerOptions, format: "module" | "commonjs"): ts.CompilerOptions {
  const lowering = Object.fromEntries(Object.entries({
    jsxFactory: options.jsxFactory, jsxFragmentFactory: options.jsxFragmentFactory,
    jsxImportSource: options.jsxImportSource, useDefineForClassFields: options.useDefineForClassFields,
    experimentalDecorators: options.experimentalDecorators, emitDecoratorMetadata: options.emitDecoratorMetadata,
    alwaysStrict: options.alwaysStrict, importHelpers: options.importHelpers,
    noEmitHelpers: options.noEmitHelpers, removeComments: options.removeComments,
  }).filter(([, value]) => value !== undefined))
  return {
    ...lowering,
    // Only lowering options apply; project-output/typechecking settings do not.
    target: Math.min(options.target ?? ts.ScriptTarget.ES2022, ts.ScriptTarget.ES2022),
    module: format === "module" ? ts.ModuleKind.ESNext : ts.ModuleKind.CommonJS,
    jsx: options.jsx === ts.JsxEmit.Preserve || options.jsx === ts.JsxEmit.ReactNative
      ? ts.JsxEmit.React : options.jsx ?? ts.JsxEmit.React,
    esModuleInterop: options.esModuleInterop ?? true,
    verbatimModuleSyntax: options.verbatimModuleSyntax ?? false,
    inlineSourceMap: true,
    inlineSources: true,
    sourceRoot: "",
    ignoreDeprecations: "6.0",
  }
}

export interface ModuleExecution {
  root: string
  rootConfig: string | undefined
  configReads: ReadonlyMap<string, string>
  dispose(): void
  importSourceExports(url: URL): Promise<Record<string, unknown>>
}
let activeExecution: ModuleExecution | undefined
export function importSourceExports(url: URL): Promise<Record<string, unknown>> {
  if (activeExecution === undefined) throw new Error("Helmr module execution has not been installed")
  return activeExecution.importSourceExports(url)
}
export function moduleExecutionIdentity() {
  const digest = (url: URL) => `sha256:${createHash("sha256").update(readFileSync(url)).digest("hex")}`
  return { apiVersion: languageVersion, adapterDigest: digest(new URL(import.meta.url)), typescriptDigest: digest(new URL("./typescript.cjs", import.meta.url)), typescriptVersion }
}
