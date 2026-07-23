// runtime/typescript/src/inspect.ts
import { resolve as resolve2 } from "node:path";

// sdk/typescript/src/config.ts
var configBrand = Symbol.for("helmr.sdk.v0.config");
function inspectConfig(value) {
  if (typeof value !== "object" || value === null)
    return;
  if (!Object.hasOwn(value, configBrand))
    return;
  if (value[configBrand] !== true) {
    throw new Error("invalid defineConfig() private record");
  }
  const config = value;
  if (typeof config.project !== "string" || config.project.trim() === "" || hasControl(config.project) || !Array.isArray(config.dirs) || config.dirs.length === 0 || !Array.isArray(config.ignorePatterns)) {
    throw new Error("invalid defineConfig() private record");
  }
  for (const directory of config.dirs)
    validateDirectory(directory);
  for (const pattern of config.ignorePatterns)
    validateIgnorePattern(pattern);
  return value;
}
function validateDirectory(value) {
  if (typeof value !== "string" || !value.startsWith("./") || value.includes("\\") || value.includes("?") || value.includes("#") || hasControl(value)) {
    throw new Error("defineConfig({ dirs }) entries must be project-relative POSIX directories beginning ./");
  }
  if (value !== "./") {
    const segments = value.slice(2).split("/");
    if (segments.some((segment) => segment === "" || segment === "." || segment === "..")) {
      throw new Error("defineConfig({ dirs }) entries must be normalized project-relative paths");
    }
  }
  return value;
}
function validateIgnorePattern(value) {
  if (typeof value !== "string" || value === "" || value.startsWith("./") || value.startsWith("/") || value.endsWith("/") || value.includes("//") || value.includes("\\") || value.split("/").includes("..") || hasControl(value) || value.startsWith("!") || /[[\]{}]/.test(value) || /[?*+@!]\(/.test(value) || value.split("/").some((segment) => segment.includes("**") && segment !== "**")) {
    throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`);
  }
  return value;
}
function hasControl(value) {
  for (const character of value) {
    const code = character.codePointAt(0);
    if (code <= 31 || code >= 127 && code <= 159)
      return true;
  }
  return false;
}
// sdk/typescript/src/schema/payload.ts
var payloadSchemaValidationErrorBrand = Symbol.for("helmr.sdk.PayloadSchemaValidationError");

// sdk/typescript/src/internal/runtime.ts
var runtimeOperationsSymbol = Symbol.for("helmr.sdk.v0.runtime_operations");

// sdk/typescript/src/definitions.ts
var privateDefinitionBrand = Symbol.for("helmr.sdk.v0.definition");
var privateQueueBrand = Symbol.for("helmr.sdk.v0.queue");
// sdk/typescript/src/image.ts
var imageBrand = Symbol.for("helmr.sdk.v0.image");
var sourceFileBrand = Symbol.for("helmr.sdk.v0.source-file");
var sourceDirectoryBrand = Symbol.for("helmr.sdk.v0.source-directory");
class SourceFile {
  path;
  constructor(path) {
    this.path = path;
    Object.defineProperty(this, sourceFileBrand, { value: true });
    Object.freeze(this);
  }
}

class SourceDirectory {
  path;
  constructor(path) {
    this.path = path;
    Object.defineProperty(this, sourceDirectoryBrand, { value: true });
    Object.freeze(this);
  }
}
var source = Object.freeze({
  file(path) {
    return new SourceFile(path);
  },
  directory(path) {
    return new SourceDirectory(path);
  }
});
// sdk/typescript/src/workspace.ts
var workspaceDefinitionBrand = Symbol.for("helmr.sdk.v0.workspace");
function workspaceRef(address) {
  if ((("id" in address) && typeof address.id === "string") === (("key" in address) && typeof address.key === "string")) {
    throw new Error("workspace ref requires exactly one of id or key");
  }
  return createWorkspaceRef(address);
}
var workspaces = Object.freeze({
  ref: workspaceRef,
  list(_options) {
    return runtimeUnavailable("workspaces.list");
  }
});
function createWorkspaceRef(address) {
  const operations = {
    retrieve(_options) {
      return runtimeUnavailable("workspace.retrieve");
    },
    update(_options) {
      return runtimeUnavailable("workspace.update");
    },
    stop(_options) {
      return runtimeUnavailable("workspace.stop");
    },
    delete(_options) {
      return runtimeUnavailable("workspace.delete");
    }
  };
  return Object.freeze({ ...address, ...operations });
}
function runtimeUnavailable(operation) {
  throw new Error(`${operation} is unavailable without the Helmr managed runtime or authenticated client`);
}
// sdk/typescript/src/internal/jsoncanon.ts
var textDecoder = new TextDecoder("utf-8", { fatal: true });
var textEncoder = new TextEncoder;
// runtime/typescript/src/config.ts
import { lstat } from "node:fs/promises";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";

class MissingConfigError extends Error {
  constructor(path) {
    super(`missing helmr.config.ts at ${path}`);
    this.name = "MissingConfigError";
  }
}
async function loadConfig(root) {
  const path = resolve(root, "helmr.config.ts");
  let metadata;
  try {
    metadata = await lstat(path);
  } catch (error) {
    if (error.code === "ENOENT") {
      throw new MissingConfigError(path);
    }
    throw error;
  }
  if (!metadata.isFile()) {
    throw new Error("helmr.config.ts must be a regular file");
  }
  let namespace;
  try {
    const value = await import(pathToFileURL(path).href);
    if (typeof value !== "object" || value === null) {
      throw new Error("config did not evaluate to a module namespace");
    }
    namespace = value;
  } catch (error) {
    throw new Error("failed to evaluate helmr.config.ts", { cause: error });
  }
  const config = inspectConfig(namespace["default"]);
  if (config === undefined) {
    throw new Error("helmr.config.ts must default-export defineConfig()");
  }
  return config;
}

// runtime/typescript/src/inspect.ts
function cwdFrom(argv) {
  const index = argv.indexOf("--cwd");
  if (index < 0 || index + 1 >= argv.length) {
    throw new Error("--cwd is required");
  }
  return resolve2(argv[index + 1]);
}
async function main() {
  const config = await loadConfig(cwdFrom(process.argv.slice(2)));
  process.stdout.write(`${JSON.stringify({ project: config.project })}
`);
}
await main();
