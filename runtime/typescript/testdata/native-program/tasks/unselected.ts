import { task } from "@helmr/sdk"

// Analysis visits every declaration; running another declaration must not import
// this independent entry. This also makes the fixture exercise shared chunks.
if (!process.argv.includes("--analyze")) {
  throw new Error("unselected Program entry was imported during execution")
}

export const unselected = task({ id: "unselected", run: () => "unused" })
