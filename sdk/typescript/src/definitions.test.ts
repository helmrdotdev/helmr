import { describe, expect, test } from "bun:test"
import { inspectImage } from "./internal"
import { image, source } from "./index"

describe("image source declarations", () => {
  test("accepts only SDK-created source selectors", () => {
    const file = source.file("./package.json")
    const directory = source.directory("./src")
    const copied = image("source-copy")
      .copy(file, "/app/package.json")
      .copy(directory, "/app/src")
    expect(inspectImage(copied)?.steps).toEqual([
      {
        kind: "copy_source_file",
        destination: "/app/package.json",
        source: file,
      },
      {
        kind: "copy_source_directory",
        destination: "/app/src",
        source: directory,
      },
    ])
    expect(() =>
      image("forged-source").copy(
        { path: "./package.json" } as never,
        "/app/package.json",
      ),
    ).toThrow("source.file() or source.directory()")
  })

  test("rejects copy calls that do not name a source and then a destination", () => {
    const file = source.file("./package.json")
    const copy = image("copy-order").copy as (...args: unknown[]) => unknown
    const call =
      (...args: unknown[]) =>
      () =>
        copy.apply(image("copy-order"), args)
    expect(call("/app/package.json", file)).toThrow("as its first argument")
    expect(call(file)).toThrow("destination path as its second argument")
    expect(call(file, file)).toThrow("destination path as its second argument")
    expect(call(file, "/app/package.json", "/app/extra")).toThrow(
      "only a source and a destination",
    )
  })

})
