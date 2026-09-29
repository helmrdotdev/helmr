// runtime/typescript/src/module-policy.ts
import { lstatSync, realpathSync } from "node:fs";
import { isBuiltin, registerHooks } from "node:module";
import { dirname, isAbsolute, join, relative } from "node:path";
import { fileURLToPath } from "node:url";
function installModulePolicy(options) {
  const root = realpathSync(options.root);
  if (!lstatSync(join(root, "package.json")).isFile()) throw new Error("Program requires a regular root package.json");
  if (process.env["NODE_PATH"] || process.env["NODE_OPTIONS"]) throw new Error("managed modules require NODE_PATH and NODE_OPTIONS to be unset");
  if (!process.execArgv.includes("--no-global-search-paths")) throw new Error("managed modules require Node --no-global-search-paths");
  for (let directory = dirname(root); ; directory = dirname(directory)) {
    try {
      lstatSync(join(directory, "node_modules"));
      throw new Error(`managed module root has an ancestor node_modules: ${directory}`);
    } catch (error) {
      if (error.code !== "ENOENT") throw error;
    }
    if (dirname(directory) === directory) break;
  }
  const platform = new Set((options.platformEntries ?? []).map((path) => realpathSync(path)));
  const inside = (path) => {
    const value = relative(root, path);
    return value !== ".." && !value.startsWith("../") && !isAbsolute(value);
  };
  const file = (url) => realpathSync(fileURLToPath(url));
  function programFile(url) {
    const path = file(url);
    if (!inside(path)) throw new Error(`module must stay inside Program: ${fileURLToPath(url)}`);
    return path;
  }
  return registerHooks({
    resolve(specifier, context, next) {
      if (isBuiltin(specifier)) return next(specifier, context);
      const result = next(specifier, context);
      if (result.url.startsWith("file:")) {
        const path = file(result.url);
        if (inside(path)) return result;
        let parent;
        if (context.parentURL?.startsWith("file:")) {
          try {
            parent = file(context.parentURL);
          } catch {
          }
        }
        const trusted = context.parentURL === void 0 || parent !== void 0 && platform.has(parent);
        if (!(trusted && platform.has(path))) programFile(result.url);
      }
      return result;
    },
    load(url, context, next) {
      if (!url.startsWith("file:")) return next(url, context);
      const path = file(url);
      if (!platform.has(path)) {
        programFile(url);
        if (/\.(?:[cm]?ts|tsx|jsx)$/.test(path)) throw new Error(`Program requires generated JavaScript: ${path}`);
      }
      return next(url, context);
    }
  });
}

// runtime/typescript/src/module-preload.ts
installModulePolicy({
  root: "/opt/helmr/program",
  platformEntries: ["/opt/helmr/runtime/helmr/entry.mjs"]
});
