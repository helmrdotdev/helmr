import { realpathSync } from "node:fs";
import { registerHooks } from "node:module";
import { isAbsolute, relative, sep } from "node:path";
import { fileURLToPath } from "node:url";

const programRoot = "/opt/helmr/program";

function inside(root, value) {
  const path = relative(root, value);
  return path === "" || (!path.startsWith("../") && path !== ".." && !isAbsolute(path));
}

function firstPartyPath(value) {
  if (!inside(programRoot, value)) {
    return false;
  }
  const path = relative(programRoot, value);
  return !path.split(sep).includes("node_modules");
}

function declarationPath(value) {
  return value.endsWith(".d.ts") ||
    value.endsWith(".d.mts") ||
    value.endsWith(".d.cts");
}

export function createHooks(io = {}) {
  const realpath = io.realpath ?? realpathSync;

  return {
    load(url, context, nextLoad) {
      if (!url.startsWith("file:")) {
        return nextLoad(url, context);
      }
      let physical;
      try {
        physical = realpath(fileURLToPath(url));
      } catch {
        return nextLoad(url, context);
      }
      if (!firstPartyPath(physical)) {
        return nextLoad(url, context);
      }
      if (declarationPath(physical)) {
        const error = new Error(`declaration-only TypeScript module is not executable: ${url}`);
        error.code = "ERR_HELMR_DECLARATION_MODULE";
        throw error;
      }
      return nextLoad(url, context);
    },
  };
}

registerHooks(createHooks());
