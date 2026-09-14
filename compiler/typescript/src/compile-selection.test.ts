import { afterEach, expect, test } from "bun:test"
import { mkdir, mkdtemp, rm, symlink, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { resolve } from "node:path"
import { installedPackageRoot, resolveCompilePackages, selectedPackage } from "./compile-selection"

const roots: string[] = []
afterEach(async () => { await Promise.all(roots.splice(0).map((root) => rm(root, { recursive: true, force: true }))) })
async function fixture(document: unknown = {}) {
  const root = await mkdtemp(resolve(tmpdir(), "compile-selection-"))
  roots.push(root)
  await writeFile(resolve(root, "package.json"), JSON.stringify(document))
  return root
}
async function pkg(root: string, path: string) {
  await mkdir(resolve(root, path), { recursive: true })
  await writeFile(resolve(root, path, "package.json"), '{"name":"different-from-alias"}')
}

test.each([
  ["tasks/a.ts", undefined],
  ["node_modules/a/lib/a.ts", "node_modules/a"],
  ["node_modules/a/node_modules/@s/b/lib/a.ts", "node_modules/a/node_modules/@s/b"],
  ["node_modules/.pnpm/a@1/node_modules/a/dist/index.ts", "node_modules/.pnpm/a@1/node_modules/a"],
  ["node_modules/@s/a/node_modules/b/a.ts", "node_modules/@s/a/node_modules/b"],
])("installed instance for %s", (path, wanted) => {
  expect(installedPackageRoot(path!)).toBe(wanted)
})

test("parent selection does not authorize nested dependencies", () => {
  const roots = new Set(["node_modules/a"])
  expect(selectedPackage("node_modules/a/dist/a.ts", roots)).toBe(true)
  expect(selectedPackage("node_modules/a/node_modules/b/index.ts", roots)).toBe(false)
  expect(selectedPackage("node_modules/ab/index.ts", roots)).toBe(false)
})

test("root metadata has no compile authority", async () => {
  const root = await fixture({ optionalDependencies: { missing: "file:missing" }, helmr: {compilePackages: ["node_modules/missing"]} })
  expect(await resolveCompilePackages(root, [])).toEqual([])
})

test.each(["../node_modules/a", "/node_modules/a", "node_modules/a/", "node_modules/a/lib", "node_modules//a", "node_modules/./a", "node_modules/a\\b", "node_modules/a\u0000", "helmr/node_modules/a", "node_modules/a/.helmr/node_modules/b", "node_modules/a\ud800"])("rejects malformed selector %s", async (selector) => {
  await expect(resolveCompilePackages(await fixture(), [selector])).rejects.toThrow()
})

test("retains aliases and unused assertions, canonicalizes isolated installed roots", async () => {
  const target = "node_modules/.bun/real@1/node_modules/real"
  const root = await fixture({ helmr: { compilePackages: ["node_modules/z", "node_modules/a"] } })
  await pkg(root, target)
  await symlink(".bun/real@1/node_modules/real", resolve(root, "node_modules/a"))
  await symlink(".bun/real@1/node_modules/real", resolve(root, "node_modules/z"))
  expect(await resolveCompilePackages(root, ["node_modules/z", "node_modules/a"])).toEqual([
    { logicalRoot: "node_modules/a", resolvedRoot: target },
    { logicalRoot: "node_modules/z", resolvedRoot: target },
  ])
})

test("missing selected roots and missing selected manifests fail", async () => {
  const root = await fixture({ helmr: { compilePackages: ["node_modules/a"] } })
  await expect(resolveCompilePackages(root, ["node_modules/a"])).rejects.toThrow('selector "node_modules/a"')
  await mkdir(resolve(root, "node_modules/a"), { recursive: true })
  await expect(resolveCompilePackages(root, ["node_modules/a"])).rejects.toThrow("package.json")
})

test("rejects selected root escapes, non-root targets and manifest symlinks", async () => {
  const root = await fixture({ helmr: { compilePackages: ["node_modules/a"] } })
  const outside = await fixture()
  await mkdir(resolve(root, "node_modules"))
  await symlink(outside, resolve(root, "node_modules/a"))
  await expect(resolveCompilePackages(root, ["node_modules/a"])).rejects.toThrow("escapes project")
  await rm(resolve(root, "node_modules/a"))
  await pkg(root, "node_modules/real/dist")
  await symlink("real/dist", resolve(root, "node_modules/a"))
  await expect(resolveCompilePackages(root, ["node_modules/a"])).rejects.toThrow("not an installed package root")
  await rm(resolve(root, "node_modules/a"))
  await mkdir(resolve(root, "node_modules/a"))
  await symlink("../../package.json", resolve(root, "node_modules/a/package.json"))
  await expect(resolveCompilePackages(root, ["node_modules/a"])).rejects.toThrow("regular file")
})

test.each(['{"unused":1e400}', '{"unused":"\\ud800"}', '\ufeff{}', '{"helmr":{},"helmr":{}}', '{"helmr":{"compilePackages":[],"compilePackages":[]}}', '{"helmr":{},}', '{/*comment*/"helmr":{}}'])
("rejects ambiguous/non-JSON manifest %s", async (raw) => {
  const root = await fixture()
  await pkg(root, "node_modules/a")
  await writeFile(resolve(root, "node_modules/a/package.json"), raw)
  await expect(resolveCompilePackages(root, ["node_modules/a"])).rejects.toThrow()
})
