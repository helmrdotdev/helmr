import { lstat, readFile, realpath, stat } from "node:fs/promises"
import { relative, resolve, sep } from "node:path"
import { parseTree, type Node, type ParseError } from "jsonc-parser/lib/esm/main.js"
import { compareUTF8 } from "./utf8"

export interface CompilePackage {
  readonly logicalRoot: string
  readonly resolvedRoot: string
}

// Return the full instance path, including all parent/store components. A
// package's export subdirectories do not create new installed instances.
export function installedPackageRoot(path: string): string | undefined {
  const parts = path.split("/")
  const index = parts.lastIndexOf("node_modules")
  if (index < 0 || !parts[index + 1]) return undefined
  const count = parts[index + 1]!.startsWith("@") ? 3 : 2
  if (parts.length < index + count) return undefined
  return parts.slice(0, index + count).join("/")
}

export function selectedPackage(path: string, roots: ReadonlySet<string>): boolean {
  const owner = installedPackageRoot(path)
  return owner !== undefined && roots.has(owner)
}

export async function resolveCompilePackages(root: string, selectors: readonly string[]): Promise<readonly CompilePackage[]> {
  root = await realpath(root)
  if (new Set(selectors).size !== selectors.length) {
    throw new Error("helmr.config.ts compilePackages contains duplicate selectors")
  }
  const packages = []
  for (const logicalRoot of [...selectors].sort(compareUTF8)) {
    try {
      if (!validPath(logicalRoot) || installedPackageRoot(logicalRoot) !== logicalRoot) {
        throw new Error("expected a clean project-relative installed package root, e.g. node_modules/@scope/package")
      }
      const target = await realpath(resolve(root, logicalRoot))
      const resolvedRoot = relative(root, target).split(sep).join("/")
      if (!validPath(resolvedRoot)) throw new Error("resolved root escapes project or uses a reserved path")
      if (!(await stat(target)).isDirectory()) throw new Error("selected root is not a directory")
      if (resolvedRoot.split("/").includes("node_modules") && installedPackageRoot(resolvedRoot) !== resolvedRoot) {
        throw new Error("resolved root is not an installed package root")
      }
      parseManifest(await regularManifest(resolve(target, "package.json")))
      packages.push({ logicalRoot, resolvedRoot })
    } catch (error) {
      throw new Error(`helmr.config.ts compilePackages selector ${JSON.stringify(logicalRoot)}: ${error instanceof Error ? error.message : String(error)}`)
    }
  }
  return packages
}

function validPath(path: string): boolean {
  const parts = path.split("/")
  return path.isWellFormed() && !/[\\\p{Cc}]/u.test(path) &&
    path !== "helmr" && !path.startsWith("helmr/") && parts.length <= 128 &&
    Buffer.byteLength(`/opt/helmr/program/${path}\0`) <= 4096 &&
    parts.every((part) => part !== "" && part !== "." && part !== ".." &&
      part !== ".helmr" && Buffer.byteLength(part) <= 255)
}

async function regularManifest(path: string): Promise<Buffer> {
  const metadata = await lstat(path)
  if (!metadata.isFile()) throw new Error(`package manifest must be a regular file: ${path}`)
  if (metadata.size > 16 << 20) throw new Error(`package manifest exceeds 16 MiB: ${path}`)
  return readFile(path)
}

function parseManifest(raw: Uint8Array): Record<string, unknown> {
  const text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(raw)
  const errors: ParseError[] = []
  const tree = parseTree(text, errors, { allowTrailingComma: false, disallowComments: true })
  if (errors.length || tree?.type !== "object") throw new Error("package.json must be a strict JSON object")
  function visit(node: Node): void {
    if ((node.type === "number" && !Number.isFinite(node.value)) ||
      (node.type === "string" && !(node.value as string).isWellFormed())) {
      throw new Error("package.json contains a non-finite number or invalid Unicode string")
    }
    if (node.type === "object") {
      const names = new Set<string>()
      for (const property of node.children ?? []) {
        const name = property.children![0]!.value as string
        if (names.has(name)) throw new Error(`package.json contains duplicate object key ${JSON.stringify(name)}`)
        names.add(name)
      }
    }
    for (const child of node.children ?? []) visit(child)
  }
  visit(tree)
  return JSON.parse(text) as Record<string, unknown>
}
