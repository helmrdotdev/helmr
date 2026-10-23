import { expect, test } from "bun:test"
import { cp, mkdtemp, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { fileURLToPath } from "node:url"
import { checkBundledGuides } from "./agent"

const guides = fileURLToPath(new URL("./guides", import.meta.url))

test("source bundle accepts the authored guide contents", async () => {
  expect(await checkBundledGuides(guides)).toEqual({
    name: "source-bundle",
    ok: true,
    detail: { hasGuideIndex: true, hasNixGuide: true },
  })
})

for (const [file, detail] of [
  ["INDEX.md", { hasGuideIndex: false, hasNixGuide: true }],
  ["nix-validation.md", { hasGuideIndex: true, hasNixGuide: false }],
] as const) {
  test(`source bundle rejects present but incorrect ${file}`, async () => {
    const directory = await mkdtemp(join(tmpdir(), "helmr-guide-check-"))
    try {
      await cp(guides, directory, { recursive: true })
      await writeFile(join(directory, file), "present but unrelated guide contents\n")
      expect(await checkBundledGuides(directory)).toEqual({
        name: "source-bundle",
        ok: false,
        detail,
      })
    } finally {
      await rm(directory, { recursive: true, force: true })
    }
  })
}
