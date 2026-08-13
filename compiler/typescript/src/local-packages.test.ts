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
