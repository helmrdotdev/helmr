import { builder, defineConfig } from "@helmr/sdk"
import { setupScript } from "./environment/paths"

// Evaluated once on the invoking host, before any Linux environment exists.
// The helper import is extensionless TypeScript on purpose.
export default defineConfig({
  dirs: ["tasks"],
  build: {
    builder: builder()
      // Package setup is independent of the recipe inputs below. Reuse this
      // layer while still rerunning setup.sh and its stamp after an input edit.
      .run(["/bin/sh", "-ec", "apt-get update && apt-get install -y --no-install-recommends jq && rm -rf /var/lib/apt/lists/*"])
      .copy("recipe-input.txt", "/etc/helmr-recipe-input")
      // Sources are literal paths even where Docker would read a pattern:
      // these must not select the sibling literal1/name1.txt.
      .copy("environment/literal[1]/name[1].txt", "/opt/native-environment/literal-file.txt")
      .copy("environment/literal[1]", "/opt/native-environment/literal-directory")
      // "$" is literal too: with HOME=/root and TMPDIR=/tmp set during preparation,
      // substitution would read environment/root.txt and write under .../tmp.
      .copy("environment/$HOME.txt", "/opt/native-environment/$TMPDIR/dollar.txt")
      .copy(setupScript, "/opt/native-environment/setup.sh")
      .run(["/bin/sh", "/opt/native-environment/setup.sh"]),
  },
})
