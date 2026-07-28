// sdk/typescript/src/config.ts
var arrayIsArray = Array.isArray;
var arrayPrototype = Array.prototype;
var defineProperty = Object.defineProperty;
var objectPrototype = Object.prototype;
var getOwnPropertyDescriptor = Object.getOwnPropertyDescriptor;
var getOwnPropertyDescriptors = Object.getOwnPropertyDescriptors;
var getPrototypeOf = Object.getPrototypeOf;
var hasOwn = Object.hasOwn;
var freeze = Object.freeze;
var ownKeys = Reflect.ownKeys;
var startsWith = String.prototype.startsWith.call.bind(String.prototype.startsWith);
var endsWith = String.prototype.endsWith.call.bind(String.prototype.endsWith);
var includes = String.prototype.includes.call.bind(String.prototype.includes);
var split = String.prototype.split.call.bind(String.prototype.split);
var slice = String.prototype.slice.call.bind(String.prototype.slice);
var charCodeAt = String.prototype.charCodeAt.call.bind(String.prototype.charCodeAt);
var regexpTest = RegExp.prototype.test.call.bind(RegExp.prototype.test);
var utf8Encoder = new TextEncoder;
var encodeUTF8 = TextEncoder.prototype.encode.call.bind(TextEncoder.prototype.encode);
function inspectConfig(value) {
  if (typeof value !== "object" || value === null) {
    throw new Error("config must be an ordinary object");
  }
  return normalizeConfig(value);
}
function validateDirectory(value) {
  if (typeof value !== "string" || value === "" || hasUnpairedSurrogate(value) || startsWith(value, "/") || includes(value, "\\") || hasControl(value)) {
    throw new Error("config dirs entries must be non-empty root-relative POSIX paths");
  }
  const normalized = startsWith(value, "./") ? slice(value, 2) : value;
  const segments = split(normalized, "/");
  let invalidSegment = normalized === "";
  for (let index = 0;index < segments.length; index++) {
    const segment = segments[index];
    if (segment === "" || segment === "." || segment === "..") {
      invalidSegment = true;
      break;
    }
  }
  if (invalidSegment) {
    throw new Error("config dirs entries must be normalized root-relative paths");
  }
  return normalized;
}
function validateIgnorePattern(value) {
  if (typeof value !== "string" || value === "" || hasUnpairedSurrogate(value) || startsWith(value, "./") || startsWith(value, "/") || endsWith(value, "/") || includes(value, "//") || includes(value, "\\") || hasControl(value) || startsWith(value, "!") || regexpTest(/[[\]{}]/, value) || regexpTest(/[?*+@!]\(/, value)) {
    throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`);
  }
  const segments = split(value, "/");
  for (let index = 0;index < segments.length; index++) {
    const segment = segments[index];
    if (segment === ".." || includes(segment, "**") && segment !== "**") {
      throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`);
    }
  }
  return value;
}
function hasControl(value) {
  for (let index = 0;index < value.length; index++) {
    const code = charCodeAt(value, index);
    if (code <= 31 || code >= 127 && code <= 159)
      return true;
  }
  return false;
}
function hasUnpairedSurrogate(value) {
  for (let index = 0;index < value.length; index++) {
    const code = charCodeAt(value, index);
    if (code >= 56320 && code <= 57343)
      return true;
    if (code < 55296 || code > 56319)
      continue;
    index++;
    if (index === value.length)
      return true;
    const low = charCodeAt(value, index);
    if (low < 56320 || low > 57343)
      return true;
  }
  return false;
}
function normalizeConfig(value) {
  if (arrayIsArray(value) || getPrototypeOf(value) !== objectPrototype) {
    throw new Error("config must be an ordinary object");
  }
  const descriptors = getOwnPropertyDescriptors(value);
  const keys = ownKeys(value);
  let invalidKey = !hasOwn(descriptors, "dirs");
  for (let index = 0;index < keys.length; index++) {
    const key = keys[index];
    if (typeof key !== "string" || key !== "dirs" && key !== "ignorePatterns") {
      invalidKey = true;
      break;
    }
  }
  if (invalidKey) {
    throw new Error("config requires exactly dirs and optional ignorePatterns");
  }
  for (let index = 0;index < keys.length; index++) {
    const key = keys[index];
    if (typeof key !== "string") {
      throw new Error("config requires exactly dirs and optional ignorePatterns");
    }
    const descriptor = descriptors[key];
    if (descriptor === undefined || !descriptor.enumerable || !hasOwn(descriptor, "value")) {
      throw new Error("config properties must be enumerable data properties");
    }
  }
  const dirs = normalizeStringSet(descriptors["dirs"]?.value, "config dirs", validateDirectory, true);
  const ignorePatterns = normalizeStringSet(hasOwn(descriptors, "ignorePatterns") ? descriptors["ignorePatterns"]?.value : [], "config ignorePatterns", validateIgnorePattern, false);
  return freeze({
    dirs: freeze(dirs),
    ignorePatterns: freeze(ignorePatterns)
  });
}
function normalizeStringSet(value, name, normalize, nonempty) {
  if (!arrayIsArray(value) || getPrototypeOf(value) !== arrayPrototype) {
    throw new Error(`${name} must be an array`);
  }
  const keys = ownKeys(value);
  const lengthDescriptor = getOwnPropertyDescriptor(value, "length");
  const length = lengthDescriptor?.value;
  if (typeof length !== "number" || keys.length !== length + 1 || keys[length] !== "length") {
    throw new Error(`${name} must be a dense ordinary array`);
  }
  const normalized = [];
  for (let index = 0;index < length; index++) {
    const key = `${index}`;
    if (keys[index] !== key) {
      throw new Error(`${name} must be a dense ordinary array`);
    }
    const descriptor = getOwnPropertyDescriptor(value, key);
    if (descriptor === undefined || !descriptor.enumerable || !hasOwn(descriptor, "value")) {
      throw new Error(`${name} entries must be enumerable data properties`);
    }
    const current = normalize(descriptor.value);
    let insertion = normalized.length;
    while (insertion > 0 && compareUTF8(current, normalized[insertion - 1]) < 0) {
      setArrayIndex(normalized, insertion, normalized[insertion - 1]);
      insertion--;
    }
    setArrayIndex(normalized, insertion, current);
  }
  if (nonempty && length === 0) {
    throw new Error(`${name} must be non-empty`);
  }
  for (let index = 1;index < normalized.length; index++) {
    if (normalized[index] === normalized[index - 1]) {
      throw new Error(`${name} contains a duplicate entry`);
    }
  }
  return normalized;
}
function setArrayIndex(array, index, value) {
  defineProperty(array, `${index}`, {
    configurable: true,
    enumerable: true,
    value,
    writable: true
  });
}
function compareUTF8(left, right) {
  const leftBytes = encodeUTF8(utf8Encoder, left);
  const rightBytes = encodeUTF8(utf8Encoder, right);
  const length = leftBytes.length < rightBytes.length ? leftBytes.length : rightBytes.length;
  for (let index = 0;index < length; index++) {
    const difference = leftBytes[index] - rightBytes[index];
    if (difference !== 0)
      return difference;
  }
  return leftBytes.length - rightBytes.length;
}
// sdk/typescript/src/schema/payload.ts
var payloadSchemaValidationErrorBrand = Symbol.for("helmr.sdk.PayloadSchemaValidationError");

// sdk/typescript/src/internal/runtime.ts
var runtimeOperationsSymbol = Symbol.for("helmr.sdk.v0.runtime_operations");
function currentRuntimeOperations() {
  const operations = globalThis[runtimeOperationsSymbol];
  if (operations === undefined) {
    throw new Error("runtime operation is unavailable without the Helmr managed runtime");
  }
  return operations;
}

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
  validateWorkspaceAddress(address);
  return createWorkspaceRef(address);
}
var workspaces = Object.freeze({
  ref: workspaceRef
});
function createWorkspaceRef(address) {
  const immutableAddress = address.id !== undefined ? Object.freeze({ id: address.id }) : Object.freeze({ key: address.key });
  const files = Object.freeze({
    read(path, options) {
      return currentRuntimeOperations().workspaceFileRead(immutableAddress, path, options?.signal);
    },
    stat(path, options) {
      return currentRuntimeOperations().workspaceFileStat(immutableAddress, path, options?.signal);
    },
    list(path, query, options) {
      return currentRuntimeOperations().workspaceFileList(immutableAddress, path, query, options?.signal);
    }
  });
  const operations = {
    files,
    retrieve(options) {
      return currentRuntimeOperations().workspaceRetrieve(immutableAddress, options?.signal);
    },
    exec(request, options) {
      return currentRuntimeOperations().workspaceExec(immutableAddress, request, options?.signal);
    },
    delete(request, options) {
      return currentRuntimeOperations().workspaceDelete(immutableAddress, request, options?.signal);
    }
  };
  return Object.freeze({ ...immutableAddress, ...operations });
}
function validateWorkspaceAddress(address) {
  if ((("id" in address) && typeof address.id === "string") === (("key" in address) && typeof address.key === "string")) {
    throw new Error("Workspace ref requires exactly one of id or key");
  }
  if ("id" in address && address.id !== undefined) {
    workspacePublicID(address.id, "Workspace ID");
  } else if (address.key.length === 0) {
    throw new Error("Workspace key is required");
  }
}
function workspacePublicID(value, label) {
  if (typeof value !== "string" || !/^wsp_[a-z2-7]{26}$/.test(value)) {
    throw new Error(`${label} must be a canonical Workspace public ID`);
  }
  return value;
}
// sdk/typescript/src/internal/jsoncanon.ts
var textDecoder = new TextDecoder("utf-8", { fatal: true });
var textEncoder = new TextEncoder;
function canonicalizeJsonValue(value) {
  return textEncoder.encode(serialize(value, new Set));
}
function serialize(value, ancestors) {
  if (value === null || typeof value === "boolean") {
    return String(value);
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new Error("canonical JSON numbers must be finite IEEE 754 doubles");
    }
    return JSON.stringify(value);
  }
  if (typeof value === "string") {
    assertUnicodeString(value);
    return JSON.stringify(value);
  }
  if (typeof value !== "object") {
    throw new Error(`canonical JSON does not support ${typeof value}`);
  }
  if (ancestors.has(value)) {
    throw new Error("canonical JSON does not support cyclic values");
  }
  ancestors.add(value);
  try {
    if (Array.isArray(value)) {
      assertPlainArray(value);
      const items = value.map((item) => serialize(item, ancestors));
      return `[${items.join(",")}]`;
    }
    const objectValue = value;
    assertPlainObject(objectValue);
    const entries = Object.keys(objectValue).sort().map((key) => {
      assertUnicodeString(key);
      return `${JSON.stringify(key)}:${serialize(objectValue[key], ancestors)}`;
    });
    return `{${entries.join(",")}}`;
  } finally {
    ancestors.delete(value);
  }
}
function assertPlainArray(value) {
  const keys = Reflect.ownKeys(value);
  const expected = Array.from({ length: value.length }, (_, index) => String(index));
  expected.push("length");
  if (keys.length !== expected.length || keys.some((key, index) => key !== expected[index])) {
    throw new Error("canonical JSON arrays must be dense and have no extra properties");
  }
}
function assertPlainObject(value) {
  const prototype = Object.getPrototypeOf(value);
  if (prototype !== Object.prototype && prototype !== null) {
    throw new Error("canonical JSON objects must have a plain or null prototype");
  }
  for (const key of Reflect.ownKeys(value)) {
    if (typeof key !== "string") {
      throw new Error("canonical JSON objects cannot have symbol properties");
    }
    const descriptor = Object.getOwnPropertyDescriptor(value, key);
    if (!descriptor?.enumerable || !("value" in descriptor)) {
      throw new Error("canonical JSON object properties must be enumerable data properties");
    }
  }
}
function assertUnicodeString(value) {
  for (let index = 0;index < value.length; index++) {
    const unit = value.charCodeAt(index);
    if (unit >= 55296 && unit <= 56319) {
      const next = value.charCodeAt(index + 1);
      if (next < 56320 || next > 57343) {
        throw new Error("canonical JSON contains an unpaired high surrogate");
      }
      index++;
    } else if (unit >= 56320 && unit <= 57343) {
      throw new Error("canonical JSON contains an unpaired low surrogate");
    }
  }
}
// runtime/typescript/src/config-evaluator.ts
import { createWriteStream } from "node:fs";

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
  try {
    return inspectConfig(namespace["default"]);
  } catch (error) {
    throw new Error("helmr.config.ts must default-export a valid config object", {
      cause: error
    });
  }
}

// runtime/typescript/src/config-evaluator.ts
var maxConfigBytes = 1 << 20;
async function main() {
  if (process.argv.length !== 3 || process.argv[2] === undefined) {
    throw new Error("Config Evaluator requires exactly one Program root");
  }
  const body = canonicalizeJsonValue(await loadConfig(process.argv[2]));
  if (body.byteLength === 0 || body.byteLength > maxConfigBytes) {
    throw new Error("normalized config size is invalid");
  }
  const frame = new Uint8Array(4 + body.byteLength);
  new DataView(frame.buffer).setUint32(0, body.byteLength, false);
  frame.set(body, 4);
  const configured = process.env["HELMR_SUPERVISOR_FD"];
  const fd = configured === undefined ? 3 : Number(configured);
  if (!Number.isSafeInteger(fd) || fd < 3) {
    throw new Error("Config Evaluator result descriptor is invalid");
  }
  const output = createWriteStream("", { fd, autoClose: false });
  await new Promise((resolve2, reject) => {
    output.once("error", reject);
    output.end(frame, resolve2);
  });
}
await main();
