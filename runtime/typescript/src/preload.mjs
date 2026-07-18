import { readFileSync, realpathSync } from "node:fs";
import { registerHooks } from "node:module";
import { extname, isAbsolute, relative, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const programRoot = "/opt/helmr/program";
const moduleMapPath = `${programRoot}/helmr/modules.json`;

function inside(root, value) {
  const path = relative(root, value);
  return path === "" || (!path.startsWith("../") && path !== ".." && !isAbsolute(path));
}

function firstPartyPath(value) {
  return inside(programRoot, value) &&
    !inside(`${programRoot}/node_modules`, value);
}

function retryable(error) {
  return error?.code === "ERR_MODULE_NOT_FOUND" ||
    error?.code === "ERR_UNSUPPORTED_DIR_IMPORT" ||
    error?.code === "MODULE_NOT_FOUND";
}

function hasCondition(conditions, value) {
  if (typeof conditions?.has === "function") {
    return conditions.has(value);
  }
  return conditions?.includes(value) ?? false;
}

function candidateURL(specifier, parentURL) {
  if (specifier.startsWith("file:")) {
    const url = new URL(specifier);
    return url.search === "" && url.hash === "" ? url : null;
  }
  if (specifier.startsWith("./") || specifier.startsWith("../")) {
    if (parentURL === undefined) {
      return null;
    }
    const url = new URL(specifier, parentURL);
    return url.search === "" && url.hash === "" ? url : null;
  }
  if (specifier.startsWith("/")) {
    return pathToFileURL(specifier);
  }
  return null;
}

function extensionless(url) {
  const path = fileURLToPath(url);
  return !path.endsWith("/") && extname(path) === "";
}

function declarationPath(value) {
  return value.endsWith(".d.ts") ||
    value.endsWith(".d.mts") ||
    value.endsWith(".d.cts");
}

export function createHooks(moduleMap, io = {}) {
  const readFile = io.readFile ?? readFileSync;
  const realpath = io.realpath ?? realpathSync;
  const modules = new Map(
    moduleMap.modules.map((entry) => [
      resolve(programRoot, entry.path),
      {
        codePath: resolve(programRoot, entry.codePath),
        format: entry.format,
      },
    ]),
  );

  return {
    resolve(specifier, context, nextResolve) {
      try {
        return nextResolve(specifier, context);
      } catch (error) {
        if (!retryable(error)) {
          throw error;
        }
        const base = candidateURL(specifier, context.parentURL);
        if (base === null || !extensionless(base)) {
          throw error;
        }
        const basePath = fileURLToPath(base);
        if (!firstPartyPath(basePath)) {
          throw error;
        }
        const commonjs = hasCondition(context.conditions, "require");
        const extensions = commonjs
          ? [".ts", ".cts", ".js", ".cjs"]
          : [".ts", ".mts", ".js", ".mjs"];
        const candidates = [
          ...extensions.map((extension) => `${basePath}${extension}`),
          ...extensions.map((extension) => resolve(basePath, `index${extension}`)),
        ];
        for (const candidate of candidates) {
          try {
            const resolvedCandidate = commonjs
              ? candidate
              : pathToFileURL(candidate).href;
            return nextResolve(resolvedCandidate, context);
          } catch (candidateError) {
            if (!retryable(candidateError)) {
              throw candidateError;
            }
          }
        }
        throw error;
      }
    },

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
      const module = modules.get(physical);
      if (module === undefined) {
        return nextLoad(url, context);
      }
      return {
        format: module.format,
        shortCircuit: true,
        source: readFile(module.codePath),
      };
    },
  };
}

function installHooks() {
  const moduleMap = JSON.parse(readFileSync(moduleMapPath, "utf8"));
  if (moduleMap.formatVersion !== 0 ||
      moduleMap.transformer !== "helmr.typescript.v0" ||
      !Array.isArray(moduleMap.modules)) {
    throw new Error("invalid managed TypeScript module map");
  }
  registerHooks(createHooks(moduleMap));
}

installHooks();
