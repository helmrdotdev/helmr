import { lstat, readFile, readdir, realpath, stat } from "node:fs/promises"
import { dirname, relative, resolve, sep } from "node:path"

import { glob } from "tinyglobby"
import { compareUTF8 } from "./utf8"
export interface LocalPackage {
  readonly installedRoot: string
  readonly name: string
  readonly sourceRoot: string
}

export async function deriveLocalPackages(
  root: string,
): Promise<readonly LocalPackage[]> {
  const canonicalRoot = await realpath(root)
  const sourceRoots = new Set<string>([""])
  const rootManifest = await packageManifest(canonicalRoot)
  const patterns = workspacePatterns(rootManifest)
  if (patterns.length !== 0) {
    const manifests = await glob(
      patterns.map((pattern) => `${stripTrailingSlash(pattern)}/package.json`),
      {
        absolute: false,
        cwd: canonicalRoot,
        followSymbolicLinks: false,
        ignore: ["**/node_modules/**", "helmr/**"],
        onlyFiles: true,
      },
    )
    for (const manifest of manifests) {
      sourceRoots.add(projectPath(canonicalRoot, dirname(resolve(canonicalRoot, manifest))))
    }
  }
  const pending = [...sourceRoots]
  const inspected = new Set<string>()
  while (pending.length !== 0) {
    const sourceRoot = pending.shift()!
    if (inspected.has(sourceRoot)) continue
    inspected.add(sourceRoot)
    const manifest = await packageManifest(resolve(canonicalRoot, sourceRoot))
    const targets = localDependencyTargets(manifest).map((dependency) => ({
      label: dependency,
      path: resolve(canonicalRoot, sourceRoot, dependency),
    }))
    targets.push(...await linkedLocalPackageTargets(canonicalRoot, sourceRoot))
    for (const targetInput of targets) {
      const target = await realpath(targetInput.path)
      if (!(await stat(target)).isDirectory()) {
        throw new Error(`local package target ${JSON.stringify(targetInput.label)} is not a directory`)
      }
      const path = projectPath(canonicalRoot, target)
      if (!inside(path) || hasNodeModules(path) || path.startsWith("helmr/")) {
        throw new Error(`local package target ${JSON.stringify(targetInput.label)} escapes project source`)
      }
      if (!sourceRoots.has(path)) {
        sourceRoots.add(path)
        pending.push(path)
      }
    }
  }

  const byName = new Map<string, string>()
  for (const sourceRoot of [...sourceRoots].sort(compareUTF8)) {
    const manifest = await packageManifest(resolve(canonicalRoot, sourceRoot))
    const name = manifest["name"]
    if (sourceRoot === "" && typeof name !== "string") continue
    if (typeof name !== "string" || name === "") {
      throw new Error(`local package ${JSON.stringify(sourceRoot)} has no name`)
    }
    const previous = byName.get(name)
    if (previous !== undefined && previous !== sourceRoot) {
      throw new Error(`local package name ${JSON.stringify(name)} is ambiguous`)
    }
    byName.set(name, sourceRoot)
  }

  const packages = new Map<string, LocalPackage>()
  for (const [name, sourceRoot] of byName) {
    for (const importerRoot of sourceRoots) {
      let directory = resolve(canonicalRoot, importerRoot)
      for (;;) {
        const installedRoot = resolve(directory, "node_modules", name)
        try {
          const installedManifest = await packageManifest(installedRoot)
          if (installedManifest["name"] !== name) {
            throw new Error(
              `installed local package ${JSON.stringify(installedRoot)} has the wrong name`,
            )
          }
          const installed = projectPath(canonicalRoot, installedRoot)
          if (!inside(installed) || !hasNodeModules(installed)) {
            throw new Error(`installed local package ${JSON.stringify(name)} escapes project`)
          }
          packages.set(installed, { installedRoot: installed, name, sourceRoot })
        } catch (error) {
          if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error
        }
        if (directory === canonicalRoot) break
        const parent = dirname(directory)
        if (!inside(relative(canonicalRoot, parent))) break
        directory = parent
      }
    }
  }
  return [...packages.values()].sort((left, right) =>
    compareUTF8(left.installedRoot, right.installedRoot)
  )
}

async function linkedLocalPackageTargets(
  root: string,
  importerRoot: string,
): Promise<Array<{ readonly label: string; readonly path: string }>> {
  const modules = resolve(root, importerRoot, "node_modules")
  let entries
  try {
    entries = await readdir(modules, { withFileTypes: true })
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return []
    throw error
  }
  const candidates: Array<{ readonly label: string; readonly path: string }> = []
  for (const entry of entries.sort((left, right) => compareUTF8(left.name, right.name))) {
    const path = resolve(modules, entry.name)
    if (entry.name.startsWith("@") && entry.isDirectory()) {
      const scoped = await readdir(path, { withFileTypes: true })
      for (const child of scoped.sort((left, right) => compareUTF8(left.name, right.name))) {
        const childPath = resolve(path, child.name)
        if ((await lstat(childPath)).isSymbolicLink()) {
          await addLinkedLocalPackageTarget(
            candidates,
            root,
            `${entry.name}/${child.name}`,
            childPath,
          )
        }
      }
      continue
    }
    if ((await lstat(path)).isSymbolicLink()) {
      await addLinkedLocalPackageTarget(candidates, root, entry.name, path)
    }
  }
  return candidates
}

async function addLinkedLocalPackageTarget(
  candidates: Array<{ readonly label: string; readonly path: string }>,
  root: string,
  label: string,
  path: string,
): Promise<void> {
  const target = projectPath(root, await realpath(path))
  if (inside(target) && !hasNodeModules(target) && !target.startsWith("helmr/")) {
    candidates.push({ label, path })
  }
}

function workspacePatterns(manifest: Record<string, unknown>): string[] {
  const workspaces = manifest["workspaces"]
  if (workspaces === undefined) return []
  if (Array.isArray(workspaces)) {
    return stringArray(workspaces, "package.json workspaces")
  }
  if (typeof workspaces === "object" && workspaces !== null) {
    return stringArray(
      (workspaces as Record<string, unknown>)["packages"],
      "package.json workspaces.packages",
    )
  }
  throw new Error("package.json workspaces must be an array or object")
}

function stringArray(value: unknown, name: string): string[] {
  if (
    !Array.isArray(value) ||
    value.some((item) => typeof item !== "string" || item === "")
  ) {
    throw new Error(`${name} must be an array of non-empty strings`)
  }
  return [...new Set(value as string[])].sort(compareUTF8)
}

function localDependencyTargets(manifest: Record<string, unknown>): string[] {
  const targets = new Set<string>()
  for (
    const field of [
      "dependencies",
      "devDependencies",
      "optionalDependencies",
      "peerDependencies",
    ]
  ) {
    const dependencies = manifest[field]
    if (
      typeof dependencies !== "object" ||
      dependencies === null ||
      Array.isArray(dependencies)
    ) {
      continue
    }
    for (const value of Object.values(dependencies)) {
      if (
        typeof value === "string" &&
        (value.startsWith("file:") || value.startsWith("link:"))
      ) {
        targets.add(value.slice(value.indexOf(":") + 1))
      }
    }
  }
  return [...targets].sort(compareUTF8)
}

async function packageManifest(root: string): Promise<Record<string, unknown>> {
  const value: unknown = JSON.parse(
    await readFile(resolve(root, "package.json"), "utf8"),
  )
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`package manifest at ${JSON.stringify(root)} is not an object`)
  }
  return value as Record<string, unknown>
}

function stripTrailingSlash(value: string): string {
  return value.replace(/\/+$/, "")
}

function projectPath(root: string, value: string): string {
  return relative(root, value).split(sep).join("/")
}

function hasNodeModules(path: string): boolean {
  return path.split("/").includes("node_modules")
}

function inside(path: string): boolean {
  return path === "" ||
    (path !== ".." && !path.startsWith("../") && !path.startsWith("/"))
}
