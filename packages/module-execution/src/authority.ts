import { createHash } from "node:crypto"
import { existsSync, lstatSync, readFileSync, realpathSync, statSync } from "node:fs"
import { dirname, isAbsolute, join, relative, resolve } from "node:path"
import ts from "typescript"

export function isContained(root: string, path: string): boolean {
  const value = relative(root, path)
  return value !== ".." && !value.startsWith(`../`) && !isAbsolute(value)
}

export class SourceAuthority {
  readonly root: string
  readonly configReads = new Map<string, string>()
  private readonly options = new Map<string, ts.CompilerOptions>()

  constructor(root: string) {
    this.root = realpathSync(root)
  }

  // Check the existing prefix too: a missing file below an escaping symlink
  // must not turn an authority error into alias/extension fallback.
  contained(path: string): string {
    path = resolve(path)
    if (!isContained(this.root, path)) this.escape(path)
    let prefix = path
    while (!existsSync(prefix)) {
      try {
        if (lstatSync(prefix).isSymbolicLink()) this.escape(prefix)
      } catch (error) {
        if (!missing(error)) throw error
      }
      const parent = dirname(prefix)
      if (parent === prefix) break
      prefix = parent
    }
    if (!isContained(this.root, realpathSync(prefix))) this.escape(path)
    return path
  }

  file(path: string): string {
    return realpathSync(this.contained(path))
  }

  private escape(path: string): never {
    throw new Error(`Helmr module source must stay inside Program ${this.root}: ${path}`)
  }

  assertEntry(): void {
    let value: unknown
    try {
      const manifest = this.file(join(this.root, "package.json"))
      if (!statSync(manifest).isFile()) throw new Error("not a regular file")
      value = JSON.parse(readFileSync(manifest, "utf8"))
    } catch (cause) {
      throw new Error("Helmr Program requires a contained regular root package.json", { cause })
    }
    if (value === null || typeof value !== "object" || Array.isArray(value)) {
      throw new Error("Helmr Program root package.json must contain a JSON object")
    }
    for (let directory = dirname(this.root); ; directory = dirname(directory)) {
      const candidate = join(directory, "node_modules")
      try {
        lstatSync(candidate)
        throw new Error(`Helmr managed module root has an ancestor node_modules search location: ${candidate}; use an image/project layout without this ancestor entry`)
      } catch (error) {
        if (!missing(error)) throw error
      }
      if (dirname(directory) === directory) break
    }
    if (process.env["NODE_PATH"] || process.env["NODE_OPTIONS"]) {
      throw new Error("Helmr managed modules require NODE_PATH and NODE_OPTIONS to be unset")
    }
    if (!process.execArgv.includes("--no-global-search-paths")) {
      throw new Error("Helmr managed modules require Node --no-global-search-paths")
    }
  }

  format(path: string): "module" | "commonjs" {
    if (/\.(?:mjs|mts)$/.test(path)) return "module"
    if (/\.(?:cjs|cts)$/.test(path)) return "commonjs"
    for (let directory = dirname(path); isContained(this.root, directory); directory = dirname(directory)) {
      if (directory.endsWith("/node_modules")) break
      const manifest = join(directory, "package.json")
      this.contained(manifest)
      if (existsSync(manifest)) {
        const value = JSON.parse(readFileSync(this.file(manifest), "utf8"))
        return value?.type === "module" ? "module" : "commonjs"
      }
      if (directory === this.root) break
    }
    return "commonjs"
  }

  config(path: string): ts.CompilerOptions {
    let config: string | undefined
    for (let directory = dirname(this.file(path)); ; directory = dirname(directory)) {
      const candidate = this.contained(join(directory, "tsconfig.json"))
      if (existsSync(candidate) && statSync(candidate).isFile()) {
        config = candidate
        break
      }
      if (directory === this.root) return {}
    }
    const cached = this.options.get(config)
    if (cached !== undefined) return cached
    // TypeScript owns JSONC/extends/package resolution. Every public host read
    // is contained before the parser receives bytes. No include-file discovery.
    const allowed = (path: string): string | undefined =>
      isContained(this.root, resolve(path)) ? this.contained(path) : undefined
    const host: ts.ParseConfigFileHost = {
      useCaseSensitiveFileNames: true,
      getCurrentDirectory: () => this.root,
      readDirectory: () => [],
      onUnRecoverableConfigFileDiagnostic: (diagnostic) => { throw diagnosticError([diagnostic]) },
      fileExists: (path) => {
        const value = allowed(path)
        return value !== undefined && existsSync(value) && statSync(value).isFile()
      },
      directoryExists: (path) => {
        const value = allowed(path)
        return value !== undefined && existsSync(value) && statSync(value).isDirectory()
      },
      realpath: (path) => {
        const value = allowed(path)
        return value === undefined ? path : realpathSync(value)
      },
      readFile: (path) => {
        const value = allowed(path)
        if (value === undefined || !existsSync(value)) return undefined
        const raw = readFileSync(value, "utf8")
        const syntax = (ts.parseJsonText(value, raw) as ts.JsonSourceFile & { parseDiagnostics: readonly ts.Diagnostic[] }).parseDiagnostics
        if (syntax.length > 0) throw diagnosticError(syntax)
        this.configReads.set(realpathSync(value), `sha256:${createHash("sha256").update(raw).digest("hex")}`)
        return raw
      },
    }
    const parsed = ts.getParsedCommandLineOfConfigFile(config, {}, host)
    if (parsed === undefined) throw new Error(`Cannot read TypeScript config ${config}`)
    const errors = parsed.errors.filter((diagnostic) => diagnostic.code !== 18003)
    if (errors.length > 0) throw diagnosticError(errors)
    this.options.set(config, parsed.options)
    return parsed.options
  }
}

export function missing(error: unknown): boolean {
  return error instanceof Error && "code" in error &&
    (error.code === "ENOENT" || error.code === "ENOTDIR")
}

export function diagnosticError(diagnostics: readonly ts.Diagnostic[]): Error {
  return new Error(ts.formatDiagnostics(diagnostics, {
    getCanonicalFileName: (path) => path,
    getCurrentDirectory: () => "",
    getNewLine: () => "\n",
  }).trim())
}
