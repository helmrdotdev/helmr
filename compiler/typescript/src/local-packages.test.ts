import { afterEach, describe, expect, test } from "bun:test"
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, resolve } from "node:path"

import { deriveLocalPackages } from "./local-packages"

const cleanup: string[] = []

afterEach(async () => {
  await Promise.all(cleanup.splice(0).map((path) => rm(path, { force: true, recursive: true })))
})

describe("installed-tree local packages", () => {
  test("discovers a copied workspace without package-manager identity", async () => {
    const root = await mkdtemp(resolve(tmpdir(), "helmr-local-packages-"))
    cleanup.push(root)
    await source(root, "package.json", JSON.stringify({
      name: "root",
      private: true,
      type: "module",
    }))
    await source(root, "pnpm-workspace.yaml", "this file is producer metadata and is not parsed\n")
    const manifest = JSON.stringify({ name: "@example/workspace", type: "module" })
    await source(root, "packages/workspace/package.json", manifest)
    await source(root, "packages/workspace/index.ts", "export const value = 1\n")
    await mkdir(resolve(root, "node_modules/@example"), { recursive: true })
    await symlink(
      resolve(root, "packages/workspace"),
      resolve(root, "node_modules/@example/workspace"),
      "dir",
    )
    await source(
      root,
      "node_modules/.pnpm/registry@1/node_modules/registry/package.json",
      JSON.stringify({ name: "registry", type: "module" }),
    )
    await symlink(
      resolve(root, "node_modules/.pnpm/registry@1/node_modules/registry"),
      resolve(root, "node_modules/registry"),
      "dir",
    )

    await expect(deriveLocalPackages(root)).resolves.toEqual([{
      installedRoot: "node_modules/@example/workspace",
      name: "@example/workspace",
      sourceRoot: "packages/workspace",
    }])
  })

  test.each([".tgz", ".tar.gz"])("leaves file: %s archives to the installed dependency graph", async (suffix) => {
    const root = await fixture(`file:vendor/package${suffix}`)
    await source(root, `vendor/package${suffix}`, "package-manager-owned archive")
    await expect(deriveLocalPackages(root)).resolves.toEqual([])
  })

  test.each(["file:vendor/package.txt", "link:vendor/package.tgz"])("rejects unsupported non-directory %s targets", async (specifier) => {
    const root = await fixture(specifier)
    await source(root, specifier.slice(specifier.indexOf(":") + 1), "not a source directory")
    await expect(deriveLocalPackages(root)).rejects.toThrow("not a directory")
  })

  test("rejects a missing archive", async () => {
    const root = await fixture("file:vendor/missing.tgz")
    await expect(deriveLocalPackages(root)).rejects.toThrow("ENOENT")
  })

  test.each(["file:", "link:"])("keeps %s directory boundaries and follows actual target type", async (protocol) => {
    const root = await fixture(`${protocol}packages/local.tgz`)
    const manifest = JSON.stringify({ name: "local" })
    await source(root, "packages/local.tgz/package.json", manifest)
    await source(root, "node_modules/local/package.json", manifest)
    await expect(deriveLocalPackages(root)).resolves.toEqual([{
      installedRoot: "node_modules/local", name: "local", sourceRoot: "packages/local.tgz",
    }])
    const outside = await mkdtemp(resolve(tmpdir(), "helmr-local-outside-"))
    cleanup.push(outside)
    await source(outside, "package.json", manifest)
    await rm(resolve(root, "packages/local.tgz"), { recursive: true })
    await symlink(outside, resolve(root, "packages/local.tgz"), "dir")
    await expect(deriveLocalPackages(root)).rejects.toThrow("escapes project source")
  })

  test("rejects an archive symlink escaping project source", async () => {
    const root = await fixture("file:vendor/package.tgz")
    const outside = await mkdtemp(resolve(tmpdir(), "helmr-local-outside-"))
    cleanup.push(outside)
    await source(outside, "package.tgz", "archive")
    await mkdir(resolve(root, "vendor"))
    await symlink(resolve(outside, "package.tgz"), resolve(root, "vendor/package.tgz"))
    await expect(deriveLocalPackages(root)).rejects.toThrow("escapes project source")
  })

  test("accepts a package.json workspace installed as a copy", async () => {
    const root = await mkdtemp(resolve(tmpdir(), "helmr-local-packages-"))
    cleanup.push(root)
    await source(root, "package.json", JSON.stringify({
      name: "root",
      private: true,
      type: "module",
      workspaces: ["packages/*"],
    }))
    const manifest = JSON.stringify({ name: "@example/workspace", type: "module" })
    await source(root, "packages/workspace/package.json", manifest)
    await source(root, "node_modules/@example/workspace/package.json", manifest)

    await expect(deriveLocalPackages(root)).resolves.toHaveLength(1)
  })
})

async function source(root: string, path: string, body: string): Promise<void> {
  const target = resolve(root, path)
  await mkdir(dirname(target), { recursive: true })
  await writeFile(target, body)
}

async function fixture(specifier: string): Promise<string> {
  const root = await mkdtemp(resolve(tmpdir(), "helmr-local-packages-"))
  cleanup.push(root)
  await source(root, "package.json", JSON.stringify({ dependencies: { local: specifier } }))
  return root
}
