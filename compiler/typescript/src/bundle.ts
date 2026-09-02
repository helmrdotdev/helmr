import {
  canonicalizeJsonValue,
  type HelmrConfig,
  type JsonValue,
  type RuntimeArchitecture,
} from "@helmr/sdk/internal"
import {
  build,
  version as esbuildVersion,
  type BuildOptions,
  type Metafile,
  type Plugin,
} from "esbuild"
import { createHash } from "node:crypto"
import { createRequire } from "node:module"
import {
  mkdir,
  readFile,
  realpath,
  rm,
  stat,
  writeFile,
} from "node:fs/promises"
import { dirname, relative, resolve, sep } from "node:path"
import { pathToFileURL } from "node:url"
import {
  parse as parseJSONC,
  type ParseError,
} from "jsonc-parser/lib/esm/main.js"

import { discoverModules } from "./analysis"
import {
  analyze,
  analyzeProgramExports,
  type AnalysisExport,
  type AnalysisResult,
  type DeclarationLocator,
} from "./compile"
import {
  deriveLocalPackages,
  type LocalPackage,
} from "./local-packages"
import { compareUTF8 } from "./utf8"

export const COMPILER_API_VERSION = "helmr.compiler.v0" as const
export const ESBUILD_VERSION = "0.28.1" as const
export const RUNTIME_PROGRAM_ROOT = "/opt/helmr/program" as const

if (esbuildVersion !== ESBUILD_VERSION) {
  throw new Error(
    `esbuild version ${JSON.stringify(esbuildVersion)} does not match ${ESBUILD_VERSION}`,
  )
}

const declarationExtensions = [
  ".cjs",
  ".cts",
  ".js",
  ".jsx",
  ".mjs",
  ".mts",
  ".ts",
  ".tsx",
] as const

export interface ProgramCompilation {
  readonly analysis: AnalysisResult
  readonly files: ReadonlyMap<string, Uint8Array>
  readonly modules: readonly string[]
  readonly optionsDigest: string
}

interface BundledOutput {
  readonly code: Uint8Array
  readonly externalEdges: readonly ExternalEdge[]
  readonly localPackages: readonly LocalPackage[]
  readonly map: Uint8Array
  readonly metafile: Metafile
}

interface OutputFiles {
  readonly code: Uint8Array
  readonly localPackages: readonly LocalPackage[]
  readonly map: Uint8Array
}

interface FinalEntry {
  readonly entrySource: string
  readonly outfile: string
  readonly source: string
  readonly sourcefile: string
}

interface ExternalEdge {
  readonly importer: string
  readonly kind: string
  readonly logicalPath: string
  readonly resolvedPath: string
  readonly runtimePath: string
  readonly specifier: string
}

export function compilerContract(): JsonValue {
  return {
    apiVersion: COMPILER_API_VERSION,
    esbuildVersion: ESBUILD_VERSION,
    optionsContractDigest: compilerOptionsContractDigest(),
    output: {
      aggregate: "analysis-only",
      finalModules: "independent",
      sharedChunks: false,
      sourceMaps: "external",
    },
    source: {
      declarationExtensions: declarationExtensions,
      packageDependencies: "external",
      semantics: "pinned-esbuild",
      workspaceDependencies: "bundled",
    },
  }
}

export async function compileProgram(options: {
  readonly architecture: RuntimeArchitecture
  readonly config: HelmrConfig
  readonly nodeVersion: string
  readonly outputRoot: string
  readonly root: string
  readonly runtimeRoot?: string
}): Promise<ProgramCompilation> {
  const root = await realpath(options.root)
  const outputRoot = resolve(options.outputRoot)
  const canonicalLocalPackages = await deriveLocalPackages(root)
  const modules = await discoverModules(root, options.config)
  if (modules.length === 0) {
    throw new Error("configured dirs contain no declaration source modules")
  }
  const analysisRoot = resolve(outputRoot, "analysis")
  await mkdir(analysisRoot, { recursive: false })
  try {
    const aggregatePath = resolve(analysisRoot, "aggregate.mjs")
    const aggregate = await bundleEntry({
      root,
      entrySource: aggregateEntry(modules),
      sourcefile: "<helmr-analysis>",
      outfile: resolve(root, "helmr/analysis.mjs"),
      nodeVersion: options.nodeVersion,
      runtimeRoot: root,
      localPackages: canonicalLocalPackages,
    })
    await writeFile(aggregatePath, aggregate.code)
    const namespaces = await importAggregate(aggregatePath)
    const analyzed = analyze({
      architecture: options.architecture,
      exports: analysisExports(namespaces),
    })
    const declarationsBySource = new Map<string, string[]>()
    for (const declaration of analyzed.declarationLocator.declarations) {
      const exports = declarationsBySource.get(declaration.modulePath) ?? []
      exports.push(declaration.exportName)
      declarationsBySource.set(declaration.modulePath, exports)
    }
    const canonicalSourceGroups = [...declarationsBySource].sort(
      ([left], [right]) => compareUTF8(left, right),
    )
    const generated = new Map<string, string>()
    const files = new Map<string, Uint8Array>()
    const finalOutputs: Array<{
      readonly moduleDigest: string
      readonly modulePath: string
      readonly sourceMapDigest: string
      readonly sourceMapPath: string
      readonly sourcePath: string
    }> = []
    const metafiles = [aggregate.metafile]
    // Aggregate execution is producer-only analysis. Only final declaration
    // modules contribute external edges to the executable Program closure.
    const runtimeRoot = resolve(options.runtimeRoot ?? RUNTIME_PROGRAM_ROOT)
    const finalBundle = await bundleEntries({
      root,
      outputRoot: analysisRoot,
      entries: canonicalSourceGroups.map(([source, exportNames]) => ({
        source,
        entrySource: finalEntry(source, exportNames),
        sourcefile: `<helmr-final-${source}>`,
        outfile: resolve(analysisRoot, generatedModulePath(source)),
      })),
      nodeVersion: options.nodeVersion,
      runtimeRoot,
      localPackages: canonicalLocalPackages,
    })
    metafiles.push(finalBundle.metafile)
    for (const [source, compiled] of finalBundle.outputs) {
      const path = generatedModulePath(source)
      const sourceMap = normalizeSourceMap(
        root,
        resolve(analysisRoot, path),
        source,
        compiled.map,
        compiled.localPackages,
      )
      const analysisModulePath = resolve(analysisRoot, path)
      await writeFile(`${analysisModulePath}.map`, sourceMap)
      await verifyFinalModule(
        analysisModulePath,
        source,
        analyzed,
        options.architecture,
      )
      generated.set(source, path)
      files.set(path, compiled.code)
      files.set(`${path}.map`, sourceMap)
      finalOutputs.push({
        moduleDigest: `sha256:${sha256(compiled.code)}`,
        modulePath: path,
        sourceMapDigest: `sha256:${sha256(sourceMap)}`,
        sourceMapPath: `${path}.map`,
        sourcePath: source,
      })
    }
    finalOutputs.sort((left, right) =>
      compareUTF8(left.modulePath, right.modulePath)
    )
    const locator: DeclarationLocator = {
      declarations: analyzed.declarationLocator.declarations.map((item) => ({
        ...item,
        modulePath: requiredGeneratedPath(generated, item.modulePath),
      })),
      formatVersion: analyzed.declarationLocator.formatVersion,
    }
    const locatorBytes = canonicalizeJsonValue(locator as unknown as JsonValue)
    const analysis: AnalysisResult = Object.freeze({
      ...analyzed,
      declarationLocator: Object.freeze({
        ...locator,
        declarations: Object.freeze(locator.declarations),
      }),
      declarationLocatorBytes: locatorBytes,
    })
    const configBytes = canonicalizeJsonValue(
      options.config as unknown as JsonValue,
    )
    files.set("helmr/config.json", configBytes)
    const externalEdges = finalBundle.externalEdges
    const inputs = await compilerInputs(
      root,
      metafiles,
      canonicalLocalPackages,
      new Set(["<helmr-analysis>", ...finalBundle.virtualInputs]),
    )
    const localPackages = sortedLocalPackages(localPackagesForInputs(
      new Set(inputs.map((input) => input.path)),
      new Map(
        canonicalLocalPackages.map((item) => [item.installedRoot, item]),
      ),
    ))
    const tsconfigs = await compilerTSConfigs(root, inputs.map((item) => item.path))
    files.set(
      "helmr/compiler-result.json",
      canonicalizeJsonValue({
        compiler: compilerContract(),
        config: {
          digest: `sha256:${sha256(configBytes)}`,
          path: "helmr/config.json",
        },
        execution: {
          nodeVersion: options.nodeVersion,
          optionsDigest: compilerOptionsDigest(options.nodeVersion),
        },
        discoveryCandidates: modules,
        externalEdges,
        inputs,
        localPackages,
        outputs: finalOutputs,
        selections: analyzed.declarationLocator.declarations
          .map((item) => ({
            declaredId: item.declaredId,
            exportName: item.exportName,
            kind: item.kind,
            slot: item.slot,
            sourcePath: item.modulePath,
          })),
        tsconfigs,
      } as unknown as JsonValue),
    )
    return Object.freeze({
      analysis,
      files,
      modules: Object.freeze(modules),
      optionsDigest: compilerOptionsDigest(options.nodeVersion),
    })
  } finally {
    await rm(analysisRoot, { force: true, recursive: true })
  }
}

export async function compileConfig(options: {
  readonly nodeVersion: string
  readonly outputRoot: string
  readonly root: string
}): Promise<{
  readonly path: string
  readonly cleanup: () => Promise<void>
}> {
  const root = await realpath(options.root)
  const entry = resolve(root, "helmr.config.ts")
  const localPackages = await deriveLocalPackages(root)
  const outputRoot = resolve(options.outputRoot, "config")
  await mkdir(outputRoot, { recursive: false })
  try {
    const output = resolve(outputRoot, "config-evaluation.mjs")
    const compiled = await bundleFile({
      root,
      entry,
      nodeVersion: options.nodeVersion,
      outfile: output,
      runtimeRoot: root,
      localPackages,
    })
    await writeFile(output, compiled.code)
    return {
      path: output,
      cleanup: async () => {
        await rm(outputRoot, { force: true, recursive: true })
      },
    }
  } catch (error) {
    await rm(outputRoot, { force: true, recursive: true })
    throw error
  }
}

export function compilerOptionsContractDigest(): string {
  return compilerOptionsDigestForTarget("exact-managed-node")
}

export function compilerOptionsDigest(nodeVersion: string): string {
  return compilerOptionsDigestForTarget(esbuildNodeTarget(nodeVersion))
}

function compilerOptionsDigestForTarget(target: string): string {
  const canonical = canonicalizeJsonValue({
    apiVersion: COMPILER_API_VERSION,
    banner: 'import { createRequire as __helmrCreateRequire } from "node:module"; const require = __helmrCreateRequire(import.meta.url);',
    bundle: true,
    esbuildVersion: ESBUILD_VERSION,
    format: "esm",
    finalEntryGraph: "multi-entry",
    legalComments: "none",
    metafile: true,
    packages: "bundle",
    platform: "node",
    preserveSymlinks: false,
    sourceMap: "external",
    sourceMapSources: "absolute-program-urls",
    sourcesContent: false,
    splitting: false,
    treeShaking: true,
    declarationExtensions,
    sourceSemantics: "pinned-esbuild",
    target,
  } as unknown as JsonValue)
  return `sha256:${sha256(canonical)}`
}

async function bundleFile(options: {
  readonly root: string
  readonly entry: string
  readonly nodeVersion: string
  readonly outfile: string
  readonly runtimeRoot: string
  readonly localPackages: readonly LocalPackage[]
}): Promise<BundledOutput> {
  const localPackages = new Map(
    options.localPackages.map((item) => [item.installedRoot, item]),
  )
  const externalEdges: ExternalEdge[] = []
  const result = await build({
    ...baseOptions(
      options.root,
      options.runtimeRoot,
      options.nodeVersion,
      localPackages,
      externalEdges,
    ),
    entryPoints: [options.entry],
    outfile: options.outfile,
  })
  const output = {
    ...singleOutputFiles(requireOutputFiles(result.outputFiles), options.outfile),
    localPackages: sortedLocalPackages(localPackages),
    externalEdges: sortedExternalEdges(externalEdges),
    metafile: requiredMetafile(result.metafile),
  }
  return output
}

async function bundleEntry(options: {
  readonly root: string
  readonly entrySource: string
  readonly nodeVersion: string
  readonly sourcefile: string
  readonly outfile: string
  readonly runtimeRoot: string
  readonly localPackages: readonly LocalPackage[]
}): Promise<BundledOutput> {
  const localPackages = new Map(
    options.localPackages.map((item) => [item.installedRoot, item]),
  )
  const externalEdges: ExternalEdge[] = []
  const result = await build({
    ...baseOptions(
      options.root,
      options.runtimeRoot,
      options.nodeVersion,
      localPackages,
      externalEdges,
    ),
    outfile: options.outfile,
    stdin: {
      contents: options.entrySource,
      loader: "js",
      resolveDir: options.root,
      sourcefile: options.sourcefile,
    },
  })
  const output = {
    ...singleOutputFiles(requireOutputFiles(result.outputFiles), options.outfile),
    localPackages: sortedLocalPackages(localPackages),
    externalEdges: sortedExternalEdges(externalEdges),
    metafile: requiredMetafile(result.metafile),
  }
  return output
}

async function bundleEntries(options: {
  readonly root: string
  readonly outputRoot: string
  readonly entries: readonly FinalEntry[]
  readonly nodeVersion: string
  readonly runtimeRoot: string
  readonly localPackages: readonly LocalPackage[]
}): Promise<{
  readonly externalEdges: readonly ExternalEdge[]
  readonly metafile: Metafile
  readonly outputs: ReadonlyMap<string, OutputFiles>
  readonly virtualInputs: ReadonlySet<string>
}> {
  const localPackages = new Map(
    options.localPackages.map((item) => [item.installedRoot, item]),
  )
  const externalEdges: ExternalEdge[] = []
  const entries = new Map(
    options.entries.map((entry) => [entry.sourcefile, entry]),
  )
  const result = await build({
    ...baseOptions(
      options.root,
      options.runtimeRoot,
      options.nodeVersion,
      localPackages,
      externalEdges,
      [finalEntryPlugin(options.root, entries)],
    ),
    entryPoints: options.entries.map((entry) => ({
      in: entry.sourcefile,
      out: projectPath(options.outputRoot, entry.outfile).slice(0, -4),
    })),
    outdir: options.outputRoot,
    outExtension: { ".js": ".mjs" },
    write: true,
  })
  const metafile = requiredMetafile(result.metafile)
  const outputPaths = new Set(
    Object.keys(metafile.outputs).map((path) => resolve(options.root, path)),
  )
  if (outputPaths.size !== options.entries.length * 2) {
    throw new Error("esbuild output topology does not match the v0 contract")
  }
  const externalEdgesByImporter = new Map<string, ExternalEdge[]>()
  for (const edge of externalEdges) {
    const importerEdges = externalEdgesByImporter.get(edge.importer) ?? []
    importerEdges.push(edge)
    externalEdgesByImporter.set(edge.importer, importerEdges)
  }
  const outputs = new Map<string, OutputFiles>()
  const externalEdgeGroups: Array<readonly ExternalEdge[]> = []
  for (const entry of options.entries) {
    if (
      !outputPaths.has(entry.outfile) ||
      !outputPaths.has(`${entry.outfile}.map`)
    ) {
      throw new Error("esbuild output topology does not match the v0 contract")
    }
    const output = requiredMetafileOutput(options.root, metafile, entry.outfile)
    const inputPaths = outputInputPaths(options.root, output.inputs)
    externalEdgeGroups.push(
      externalEdgesForOutput(output, inputPaths, externalEdgesByImporter),
    )
    outputs.set(entry.source, {
      code: await readFile(entry.outfile),
      localPackages: sortedLocalPackages(
        localPackagesForInputs(inputPaths, localPackages),
      ),
      map: await readFile(`${entry.outfile}.map`),
    })
  }
  return {
    externalEdges: mergeExternalEdges(externalEdgeGroups),
    metafile,
    outputs,
    virtualInputs: new Set(
      options.entries.map((entry) => `helmr-final:${entry.sourcefile}`),
    ),
  }
}

function baseOptions(
  root: string,
  runtimeRoot: string,
  nodeVersion: string,
  localPackages: Map<string, LocalPackage>,
  externalEdges: ExternalEdge[],
  plugins: readonly Plugin[] = [],
): BuildOptions {
  return {
    absWorkingDir: root,
    bundle: true,
    format: "esm",
    legalComments: "none",
    logLevel: "silent",
    metafile: true,
    packages: "bundle",
    platform: "node",
    plugins: [...plugins, dependencyBoundary(
      root,
      runtimeRoot,
      localPackages,
      externalEdges,
    )],
    banner: {
      js: 'import { createRequire as __helmrCreateRequire } from "node:module"; const require = __helmrCreateRequire(import.meta.url);',
    },
    sourcesContent: false,
    sourcemap: "external",
    splitting: false,
    target: esbuildNodeTarget(nodeVersion),
    treeShaking: true,
    write: false,
  }
}

async function verifyFinalModule(
  path: string,
  source: string,
  aggregate: AnalysisResult,
  architecture: RuntimeArchitecture,
): Promise<void> {
  const namespace = await importModuleNamespace(path)
  const expected = aggregate.declarationLocator.declarations.filter(
    (item) => item.modulePath === source,
  )
  const exports: AnalysisExport[] = expected.map((item) => {
    if (!Object.prototype.hasOwnProperty.call(namespace, item.exportName)) {
      throw new Error(
        `final module ${JSON.stringify(source)} is missing export ${JSON.stringify(item.exportName)}`,
      )
    }
    return {
      exportName: item.exportName,
      modulePath: source,
      value: namespace[item.exportName],
    }
  })
  const verified = analyzeProgramExports({ architecture, exports })
  const actual = canonicalizeJsonValue({
    declarations: verified.declarationLocator.declarations,
    programDeclarations: verified.programDeclarations,
  } as unknown as JsonValue)
  const wanted = canonicalizeJsonValue({
    declarations: expected,
    programDeclarations: aggregate.programDeclarations.filter((item) =>
      expected.some(
        (located) =>
          located.kind === item.kind &&
          located.declaredId === item.declaredId,
      )
    ),
  } as unknown as JsonValue)
  if (!Buffer.from(actual).equals(Buffer.from(wanted))) {
    throw new Error(
      `final module ${JSON.stringify(source)} does not match aggregate analysis`,
    )
  }
}

async function importModuleNamespace(
  path: string,
): Promise<Record<string, unknown>> {
  const value: unknown = await import(
    `${pathToFileURL(path).href}?digest=${sha256(await readFile(path))}`
  )
  if (typeof value !== "object" || value === null) {
    throw new Error("generated final module has no ESM namespace")
  }
  return value as Record<string, unknown>
}

function esbuildNodeTarget(nodeVersion: string): string {
  if (
    !/^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$/.test(
      nodeVersion,
    )
  ) {
    throw new Error("Compiler Node version must be an exact canonical SemVer")
  }
  return `node${nodeVersion}`
}

function finalEntryPlugin(
  root: string,
  entries: ReadonlyMap<string, FinalEntry>,
): Plugin {
  return {
    name: "helmr-final-entry",
    setup(build) {
      build.onResolve({ filter: /^<helmr-final-/ }, (args) => {
        return { namespace: "helmr-final", path: args.path }
      })
      build.onLoad({ filter: /.*/, namespace: "helmr-final" }, (args) => {
        const entry = entries.get(args.path)
        if (entry === undefined) {
          return { errors: [{ text: `unknown final entry ${args.path}` }] }
        }
        return {
          contents: entry.entrySource,
          loader: "js",
          resolveDir: root,
        }
      })
    },
  }
}

function requiredMetafileOutput(
  root: string,
  metafile: Metafile,
  outfile: string,
): Metafile["outputs"][string] {
  const matches = Object.entries(metafile.outputs).filter(([path]) =>
    resolve(root, path) === outfile
  )
  if (matches.length !== 1) {
    throw new Error("esbuild metafile output does not match the v0 topology")
  }
  return matches[0]![1]
}

function outputInputPaths(
  root: string,
  inputs: Metafile["outputs"][string]["inputs"],
): ReadonlySet<string> {
  return new Set(
    Object.entries(inputs)
      .filter(([, input]) => input.bytesInOutput > 0)
      .map(([path]) => projectPath(root, resolve(root, path))),
  )
}

function localPackagesForInputs(
  inputs: ReadonlySet<string>,
  localPackages: ReadonlyMap<string, LocalPackage>,
): ReadonlyMap<string, LocalPackage> {
  const paths = [...inputs]
  return new Map(
    [...localPackages].filter(([installedRoot]) =>
      paths.some((path) =>
        path === installedRoot || path.startsWith(`${installedRoot}/`)
      )
    ),
  )
}

function externalEdgesForOutput(
  output: Metafile["outputs"][string],
  inputs: ReadonlySet<string>,
  externalEdgesByImporter: ReadonlyMap<string, readonly ExternalEdge[]>,
): readonly ExternalEdge[] {
  const emitted = new Set(
    output.imports
      .filter((item) => item.external)
      .map((item) => `${item.kind}\0${item.path}`),
  )
  const externalEdges: ExternalEdge[] = []
  for (const input of inputs) {
    for (const edge of externalEdgesByImporter.get(input) ?? []) {
      if (emitted.has(`${edge.kind}\0${edge.runtimePath}`)) {
        externalEdges.push(edge)
      }
    }
  }
  return externalEdges
}

function dependencyBoundary(
  root: string,
  runtimeRoot: string,
  localPackages: Map<string, LocalPackage>,
  externalEdges: ExternalEdge[],
): Plugin {
  const canonicalRoot = resolve(root)
  return {
    name: "helmr-dependency-boundary",
    setup(build) {
      build.onResolve({ filter: /.*/ }, async (args) => {
        if (
          args.pluginData === resolvedByBoundary ||
          args.path.startsWith("node:")
        ) {
          return undefined
        }
        const result = await build.resolve(args.path, {
          importer: args.importer,
          kind: args.kind,
          namespace: args.namespace,
          pluginData: resolvedByBoundary,
          resolveDir: args.resolveDir,
          with: args.with,
        })
        if (result.errors.length !== 0 || result.external) return result
        if (result.path === "") return result
        const logicalPath = projectPath(canonicalRoot, resolve(result.path))
        const path = await realpath(result.path)
        const resolvedPath = projectPath(canonicalRoot, path)
        if (!inside(relative(canonicalRoot, path))) {
          return {
            errors: [{
              text: `resolved path escapes submitted source: ${args.path}`,
            }],
          }
        }
        if (localPackageForPath(logicalPath, localPackages) !== undefined ||
          localPackageForPath(resolvedPath, localPackages) !== undefined) {
          return { path }
        }
        if (hasNodeModules(resolvedPath)) {
          const runtimePath = resolve(runtimeRoot, logicalPath)
          externalEdges.push({
            importer: args.importer === ""
              ? args.importer
              : projectPath(canonicalRoot, resolve(args.importer)),
            kind: args.kind,
            logicalPath,
            resolvedPath,
            runtimePath,
            specifier: args.path,
          })
          const target = resolve(runtimeRoot, logicalPath)
          return {
            external: true,
            path: target,
          }
        }
        return { path }
      })
    },
  }
}

const resolvedByBoundary = Object.freeze({})


function localPackageForPath(
  path: string,
  localPackages: ReadonlyMap<string, LocalPackage>,
): LocalPackage | undefined {
  for (const localPackage of localPackages.values()) {
    if (
      path === localPackage.installedRoot ||
      path.startsWith(`${localPackage.installedRoot}/`)
    ) {
      return localPackage
    }
  }
  return undefined
}

function sortedLocalPackages(
  localPackages: ReadonlyMap<string, LocalPackage>,
): readonly LocalPackage[] {
  return [...localPackages.values()].sort((left, right) =>
    compareUTF8(left.installedRoot, right.installedRoot)
  )
}

function sortedExternalEdges(
  edges: readonly ExternalEdge[],
): readonly ExternalEdge[] {
  const unique = new Map(
    edges.map((edge) => [externalEdgeKey(edge), edge]),
  )
  return [...unique.values()].sort((left, right) =>
    compareUTF8(externalEdgeKey(left), externalEdgeKey(right))
  )
}

function mergeExternalEdges(
  groups: readonly (readonly ExternalEdge[])[],
): readonly ExternalEdge[] {
  return sortedExternalEdges(groups.flat())
}

function externalEdgeKey(edge: ExternalEdge): string {
  return [
    edge.importer,
    edge.specifier,
    edge.kind,
    edge.logicalPath,
    edge.resolvedPath,
    edge.runtimePath,
  ].join("\0")
}

function aggregateEntry(modules: readonly string[]): string {
  const imports = modules.map(
    (path, index) =>
      `import * as module${index} from ${JSON.stringify(`./${path}`)};`,
  )
  const entries = modules.map(
    (path, index) =>
      `{ modulePath: ${JSON.stringify(path)}, namespace: module${index} }`,
  )
  return `${imports.join("\n")}\nexport default [${entries.join(",")}];\n`
}

function finalEntry(source: string, exportNames: readonly string[]): string {
  const bindings = exportNames.map(
    (name, index) =>
      `const helmrExport${index} = source[${JSON.stringify(name)}];`,
  )
  const exports = exportNames.map(
    (name, index) =>
      `helmrExport${index} as ${JSON.stringify(name)}`,
  )
  return [
    `import * as source from ${JSON.stringify(`./${source}`)};`,
    ...bindings,
    `export { ${exports.join(", ")} };`,
    "",
  ].join("\n")
}

function generatedModulePath(source: string): string {
  const directory = dirname(source)
  const prefix = directory === "." ? "" : `${directory}/`
  return `${prefix}.helmr/modules/${sha256(source)}.mjs`
}

async function importAggregate(
  path: string,
): Promise<readonly {
  readonly modulePath: string
  readonly namespace: Record<string, unknown>
}[]> {
  const value: unknown = await import(
    `${pathToFileURL(path).href}?digest=${sha256(await readFile(path))}`
  )
  if (
    typeof value !== "object" ||
    value === null ||
    !Array.isArray((value as Record<string, unknown>)["default"])
  ) {
    throw new Error("analysis bundle did not export module namespaces")
  }
  return (value as { default: readonly {
    modulePath: string
    namespace: Record<string, unknown>
  }[] }).default
}

function analysisExports(
  modules: readonly {
    readonly modulePath: string
    readonly namespace: Record<string, unknown>
  }[],
): AnalysisExport[] {
  return modules.flatMap(({ modulePath, namespace }) =>
    Object.getOwnPropertyNames(namespace)
      .sort(compareUTF8)
      .map((exportName) => ({
        exportName,
        modulePath,
        value: namespace[exportName],
      }))
  )
}

function normalizeSourceMap(
  root: string,
  outfile: string,
  source: string,
  raw: Uint8Array,
  localPackages: readonly LocalPackage[],
): Uint8Array {
  const value: unknown = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(raw))
  if (typeof value !== "object" || value === null) {
    throw new Error("esbuild source map root is not an object")
  }
  const map = value as Record<string, unknown>
  if (
    map["version"] !== 3 ||
    !Array.isArray(map["sources"]) ||
    !map["sources"].every((item) => typeof item === "string") ||
    typeof map["mappings"] !== "string" ||
    !Array.isArray(map["names"])
  ) {
    throw new Error("esbuild source map does not match the v0 topology")
  }
  const sources = (map["sources"] as string[]).map((item) => {
    const decoded = decodeURIComponent(item)
    if (decoded === `helmr-final:<helmr-final-${source}>`) {
      return programSourceURL(source)
    }
    const absolute = resolve(dirname(outfile), decoded)
    const path = projectPath(root, absolute)
    if (
      !inside(relative(root, absolute)) ||
      (hasNodeModules(path) &&
        localPackageForPath(
          path,
          new Map(localPackages.map((item) => [item.installedRoot, item])),
        ) === undefined)
    ) {
      throw new Error(`source map source escapes first-party Program files: ${item}`)
    }
    return programSourceURL(path)
  })
  return canonicalizeJsonValue({
    mappings: map["mappings"],
    names: map["names"],
    sources,
    version: 3,
  } as JsonValue)
}

function programSourceURL(path: string): string {
  return pathToFileURL(resolve(RUNTIME_PROGRAM_ROOT, path)).href
}

function singleOutputFiles(
  files: readonly { readonly path: string; readonly contents: Uint8Array }[],
  outfile: string,
): { readonly code: Uint8Array; readonly map: Uint8Array } {
  const code = files.find((file) => file.path === outfile)
  const map = files.find((file) => file.path === `${outfile}.map`)
  if (code === undefined || map === undefined || files.length !== 2) {
    throw new Error("esbuild output topology does not match the v0 contract")
  }
  return { code: code.contents, map: map.contents }
}

function requireOutputFiles<T>(
  files: readonly T[] | undefined,
): readonly T[] {
  if (files === undefined) throw new Error("esbuild returned no output files")
  return files
}

function requiredMetafile(metafile: Metafile | undefined): Metafile {
  if (metafile === undefined) throw new Error("esbuild returned no metafile")
  return metafile
}

async function compilerInputs(
  root: string,
  metafiles: readonly Metafile[],
  localPackages: readonly LocalPackage[],
  virtualInputs: ReadonlySet<string>,
): Promise<readonly { readonly digest: string; readonly path: string }[]> {
  const paths = new Set<string>()
  for (const metafile of metafiles) {
    for (const input of Object.keys(metafile.inputs)) {
      if (virtualInputs.has(input)) continue
      const absolute = await realpath(resolve(root, input))
      const path = projectPath(root, absolute)
      if (!inside(relative(root, absolute))) continue
      if (
        hasNodeModules(path) &&
        !localPackages.some((item) =>
          path === item.installedRoot ||
          path.startsWith(`${item.installedRoot}/`)
        )
      ) {
        continue
      }
      paths.add(path)
    }
  }
  const sorted = [...paths].sort(compareUTF8)
  return Promise.all(sorted.map(async (path) => ({
    digest: `sha256:${sha256(await readFile(resolve(root, path)))}`,
    path,
  })))
}

async function compilerTSConfigs(
  root: string,
  inputs: readonly string[],
): Promise<readonly { readonly digest: string; readonly path: string }[]> {
  const roots = new Set<string>()
  for (const input of inputs) {
    let directory = dirname(resolve(root, input))
    for (;;) {
      const candidate = resolve(directory, "tsconfig.json")
      try {
        await readFile(candidate)
        roots.add(candidate)
        break
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
      }
      if (directory === root) break
      const parent = dirname(directory)
      if (!inside(relative(root, parent))) break
      directory = parent
    }
  }
  const configs = new Map<string, string>()
  const pending = [...roots]
  while (pending.length !== 0) {
    const candidate = await realpath(pending.shift()!)
    const path = projectPath(root, candidate)
    if (!inside(path)) {
      throw new Error(`tsconfig path escapes project: ${candidate}`)
    }
    if (configs.has(path)) continue
    const contents = await readFile(candidate)
    configs.set(path, `sha256:${sha256(contents)}`)
    const errors: ParseError[] = []
    const document: unknown = parseJSONC(contents.toString("utf8"), errors, {
      allowTrailingComma: true,
      disallowComments: false,
    })
    if (
      errors.length !== 0 ||
      typeof document !== "object" ||
      document === null ||
      Array.isArray(document)
    ) {
      throw new Error(`tsconfig ${JSON.stringify(path)} is not valid JSONC`)
    }
    const extended = (document as Record<string, unknown>)["extends"]
    const values = typeof extended === "string"
      ? [extended]
      : Array.isArray(extended) &&
          extended.every((value) => typeof value === "string")
      ? extended as string[]
      : extended === undefined
      ? []
      : (() => {
        throw new Error(`tsconfig ${JSON.stringify(path)} has invalid extends`)
      })()
    for (const specifier of values) {
      pending.push(await resolveTSConfigExtends(candidate, specifier))
    }
  }
  return [...configs]
    .map(([path, digest]) => ({ digest, path }))
    .sort((left, right) => compareUTF8(left.path, right.path))
}

async function resolveTSConfigExtends(
  configPath: string,
  specifier: string,
): Promise<string> {
  const directory = dirname(configPath)
  if (
    specifier.startsWith(".") ||
    specifier.startsWith("/") ||
    /^[A-Za-z]:[\\/]/.test(specifier)
  ) {
    return requiredConfigPath(resolve(directory, specifier))
  }
  const require = createRequire(pathToFileURL(configPath))
  for (const candidate of [specifier, `${specifier}/tsconfig.json`]) {
    try {
      return require.resolve(candidate)
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "MODULE_NOT_FOUND") {
        throw error
      }
    }
  }
  throw new Error(
    `tsconfig ${JSON.stringify(configPath)} cannot resolve extends ${JSON.stringify(specifier)}`,
  )
}

async function requiredConfigPath(candidate: string): Promise<string> {
  for (const path of [
    candidate,
    `${candidate}.json`,
    resolve(candidate, "tsconfig.json"),
  ]) {
    try {
      if ((await stat(path)).isFile()) return path
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
    }
  }
  throw new Error(`extended tsconfig ${JSON.stringify(candidate)} does not exist`)
}

function projectPath(root: string, value: string): string {
  return relative(root, value).split(sep).join("/")
}

function requiredGeneratedPath(
  generated: ReadonlyMap<string, string>,
  source: string,
): string {
  const path = generated.get(source)
  if (path === undefined) throw new Error(`missing final module for ${source}`)
  return path
}

function sha256(value: string | Uint8Array): string {
  return createHash("sha256").update(value).digest("hex")
}

function hasNodeModules(path: string): boolean {
  return path.split(sep).includes("node_modules")
}

function inside(path: string): boolean {
  return path === "" ||
    (path !== ".." && !path.startsWith(`..${sep}`) && !path.startsWith("/"))
}
