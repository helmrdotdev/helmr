// sdk/typescript/src/internal/utf8.ts
var encoder = new TextEncoder;
var encode = TextEncoder.prototype.encode.call.bind(TextEncoder.prototype.encode);
var charCodeAt = String.prototype.charCodeAt.call.bind(String.prototype.charCodeAt);
function compareUTF8(left, right) {
  const leftBytes = encode(encoder, left);
  const rightBytes = encode(encoder, right);
  const length = leftBytes.length < rightBytes.length ? leftBytes.length : rightBytes.length;
  for (let index = 0;index < length; index++) {
    const difference = leftBytes[index] - rightBytes[index];
    if (difference !== 0)
      return difference;
  }
  return leftBytes.length - rightBytes.length;
}
function hasOnlyUnicodeScalarValues(value) {
  for (let index = 0;index < value.length; index++) {
    const unit = charCodeAt(value, index);
    if (unit >= 55296 && unit <= 56319) {
      if (index + 1 === value.length)
        return false;
      const next = charCodeAt(value, index + 1);
      if (next < 56320 || next > 57343)
        return false;
      index++;
    } else if (unit >= 56320 && unit <= 57343) {
      return false;
    }
  }
  return true;
}
function assertUnicodeString(value) {
  if (!hasOnlyUnicodeScalarValues(value)) {
    throw new Error("canonical JSON contains an unpaired surrogate");
  }
}

// sdk/typescript/src/config.ts
var encoder2 = new TextEncoder;
var encode2 = TextEncoder.prototype.encode.call.bind(TextEncoder.prototype.encode);
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
var charCodeAt2 = String.prototype.charCodeAt.call.bind(String.prototype.charCodeAt);
var regexpTest = RegExp.prototype.test.call.bind(RegExp.prototype.test);
function inspectConfig(value) {
  if (typeof value !== "object" || value === null) {
    throw new Error("config must be an ordinary object");
  }
  return normalizeConfig(value);
}
function matchesIgnorePattern(pattern, path) {
  const patternSegments = split(pattern, "/");
  const pathSegments = split(path, "/");
  const matches = (patternIndex, pathIndex) => {
    if (patternIndex === patternSegments.length) {
      return pathIndex === pathSegments.length;
    }
    const segment = patternSegments[patternIndex];
    if (segment === "**") {
      if (patternIndex === patternSegments.length - 1) {
        return pathIndex < pathSegments.length;
      }
      for (let candidate = pathIndex;candidate <= pathSegments.length; candidate++) {
        if (matches(patternIndex + 1, candidate))
          return true;
      }
      return false;
    }
    return pathIndex < pathSegments.length && matchesSegment(segment, pathSegments[pathIndex]) && matches(patternIndex + 1, pathIndex + 1);
  };
  return matches(0, 0);
}
function validateDirectory(value) {
  if (typeof value !== "string" || value === "" || !hasOnlyUnicodeScalarValues(value) || startsWith(value, "/") || includes(value, "\\") || hasControl(value)) {
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
function validateCompilePackage(value) {
  if (typeof value !== "string" || !hasOnlyUnicodeScalarValues(value) || hasControl(value) || includes(value, "\\")) {
    throw new Error("config compilePackages entries must be clean installed package roots");
  }
  const parts = split(value, "/");
  let slot = -1;
  let invalidPart = false;
  for (let index = 0;index < parts.length; index++) {
    const part = parts[index];
    if (part === "node_modules")
      slot = index;
    if (part === "" || part === "." || part === ".." || part === ".helmr" || encode2(encoder2, part).length > 255)
      invalidPart = true;
  }
  const end = slot + (startsWith(parts[slot + 1] ?? "", "@") ? 3 : 2);
  if (slot < 0 || end !== parts.length || parts.length > 128 || value === "helmr" || startsWith(value, "helmr/") || encode2(encoder2, `/opt/helmr/program/${value}\x00`).length > 4096 || invalidPart) {
    throw new Error("config compilePackages entries must be clean installed package roots, e.g. node_modules/@scope/package");
  }
  return value;
}
function validateIgnorePattern(value) {
  if (typeof value !== "string" || value === "" || !hasOnlyUnicodeScalarValues(value) || startsWith(value, "./") || startsWith(value, "/") || endsWith(value, "/") || includes(value, "//") || includes(value, "\\") || hasControl(value) || startsWith(value, "!") || regexpTest(/[[\]{}]/, value) || regexpTest(/[?*+@!]\(/, value)) {
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
function matchesSegment(pattern, value) {
  const patternCharacters = codePoints(pattern);
  const valueCharacters = codePoints(value);
  let patternIndex = 0;
  let valueIndex = 0;
  let star = -1;
  let starValue = -1;
  while (valueIndex < valueCharacters.length) {
    const token = patternCharacters[patternIndex];
    if (token === "?" || token === valueCharacters[valueIndex]) {
      patternIndex++;
      valueIndex++;
      continue;
    }
    if (token === "*") {
      star = patternIndex++;
      starValue = valueIndex;
      continue;
    }
    if (star !== -1) {
      patternIndex = star + 1;
      valueIndex = ++starValue;
      continue;
    }
    return false;
  }
  while (patternCharacters[patternIndex] === "*")
    patternIndex++;
  return patternIndex === patternCharacters.length;
}
function hasControl(value) {
  for (let index = 0;index < value.length; index++) {
    const code = charCodeAt2(value, index);
    if (code <= 31 || code >= 127 && code <= 159)
      return true;
  }
  return false;
}
function codePoints(value) {
  const result = [];
  for (let index = 0;index < value.length; ) {
    const first = charCodeAt2(value, index);
    const width = first >= 55296 && first <= 56319 ? 2 : 1;
    setArrayIndex(result, result.length, slice(value, index, index + width));
    index += width;
  }
  return result;
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
    if (typeof key !== "string" || key !== "dirs" && key !== "ignorePatterns" && key !== "compilePackages") {
      invalidKey = true;
      break;
    }
  }
  if (invalidKey) {
    throw new Error("config requires dirs and optional ignorePatterns and compilePackages");
  }
  for (let index = 0;index < keys.length; index++) {
    const key = keys[index];
    if (typeof key !== "string") {
      throw new Error("config requires dirs and optional ignorePatterns and compilePackages");
    }
    const descriptor = descriptors[key];
    if (descriptor === undefined || !descriptor.enumerable || !hasOwn(descriptor, "value")) {
      throw new Error("config properties must be enumerable data properties");
    }
  }
  const dirs = normalizeStringSet(descriptors["dirs"]?.value, "config dirs", validateDirectory, true);
  const ignorePatterns = normalizeStringSet(hasOwn(descriptors, "ignorePatterns") ? descriptors["ignorePatterns"]?.value : [], "config ignorePatterns", validateIgnorePattern, false);
  const compilePackages = normalizeStringSet(hasOwn(descriptors, "compilePackages") ? descriptors["compilePackages"]?.value : [], "config compilePackages", validateCompilePackage, false);
  return freeze({
    compilePackages: freeze(compilePackages),
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
// sdk/typescript/src/schema/payload.ts
var payloadSchemaValidationErrorBrand = Symbol.for("helmr.sdk.PayloadSchemaValidationError");
function assertPayloadSchema(value, label = "payload") {
  if (value === undefined) {
    return;
  }
  assertStandardSchema(value, label);
}
function assertStandardSchema(value, label = "schema") {
  if (value === null || typeof value !== "object" && typeof value !== "function") {
    throw new Error(`${label} must implement the Standard Schema v1 interface`);
  }
  const standard = value["~standard"];
  if (standard === null || typeof standard !== "object") {
    throw new Error(`${label} must implement the Standard Schema v1 interface`);
  }
  const record = standard;
  if (record["version"] !== 1 || typeof record["validate"] !== "function") {
    throw new Error(`${label} must implement the Standard Schema v1 interface`);
  }
}

// sdk/typescript/src/schema/task.ts
var TASK_ID_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$";
var TASK_ID_MAX_LENGTH = 128;
var QUEUE_NAME_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._/-]{0,255}$";
var QUEUE_NAME_MAX_LENGTH = 256;

class TaskIdError extends Error {
  name = "TaskIdError";
  value;
  constructor(value) {
    super(`task id must match ${TASK_ID_PATTERN}: ${JSON.stringify(value)}`);
    this.value = value;
  }
}
function validateTaskId(value) {
  if (!isValidTaskId(value)) {
    throw new TaskIdError(value);
  }
}
function isValidTaskId(value) {
  if (value.length === 0 || value.length > TASK_ID_MAX_LENGTH) {
    return false;
  }
  const first = value.charCodeAt(0);
  if (!isAsciiAlnum(first)) {
    return false;
  }
  for (let index = 1;index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (!(isAsciiAlnum(code) || code === 46 || code === 95 || code === 45)) {
      return false;
    }
  }
  return true;
}

class TaskQueueNameError extends Error {
  name = "TaskQueueNameError";
  value;
  constructor(value) {
    super(`queue name must match ${QUEUE_NAME_PATTERN}: ${JSON.stringify(value)}`);
    this.value = value;
  }
}

class TaskQueueConcurrencyLimitError extends Error {
  name = "TaskQueueConcurrencyLimitError";
  value;
  constructor(value) {
    super("queue concurrencyLimit must be a positive integer");
    this.value = value;
  }
}
function validateQueueName(value) {
  if (!isValidQueueName(value)) {
    throw new TaskQueueNameError(value);
  }
}
function isValidQueueName(value) {
  if (value.length === 0 || value.length > QUEUE_NAME_MAX_LENGTH) {
    return false;
  }
  const first = value.charCodeAt(0);
  if (!isAsciiAlnum(first)) {
    return false;
  }
  for (let index = 1;index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (!(isAsciiAlnum(code) || code === 46 || code === 95 || code === 45 || code === 47)) {
      return false;
    }
  }
  return true;
}
function validateOptionalQueueConcurrencyLimit(value) {
  if (value === undefined || value === null) {
    return;
  }
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) {
    return;
  }
  throw new TaskQueueConcurrencyLimitError(value);
}
function isAsciiAlnum(code) {
  return code >= 48 && code <= 57 || code >= 65 && code <= 90 || code >= 97 && code <= 122;
}

// sdk/typescript/src/internal/runtime.ts
var runtimeOperationsSymbol = Symbol.for("helmr.sdk.v0.runtime_operations");
function currentRuntimeOperations() {
  const operations = globalThis[runtimeOperationsSymbol];
  if (operations === undefined) {
    throw new Error("runtime operation is unavailable without the Helmr managed runtime");
  }
  return operations;
}

// sdk/typescript/src/internal/id.ts
var uuidV7Pattern = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
function resourceID(value, label) {
  if (typeof value !== "string" || !uuidV7Pattern.test(value)) {
    throw new Error(`${label} must be a canonical UUIDv7`);
  }
  return value;
}

// sdk/typescript/src/session.ts
function createRuntimeSessionRef(id) {
  const sessionID = resourceID(id, "Session ID");
  return Object.freeze({
    id: sessionID,
    input: Object.freeze({
      send(input, request, options) {
        return currentRuntimeOperations().actorInputSend(sessionID, input, request, options?.signal);
      }
    }),
    output: Object.freeze({
      list(query, options) {
        return currentRuntimeOperations().sessionOutputPage(sessionID, query, options?.signal);
      }
    }),
    retrieve(options = {}) {
      return currentRuntimeOperations().sessionRetrieve(sessionID, options.signal);
    },
    close(request, options) {
      return currentRuntimeOperations().sessionClose(sessionID, request, options?.signal);
    }
  });
}
var sessions = Object.freeze({
  ref(id) {
    return createRuntimeSessionRef(id);
  }
});

// sdk/typescript/src/definitions.ts
var privateDefinitionBrand = Symbol.for("helmr.sdk.v0.definition");
var privateQueueBrand = Symbol.for("helmr.sdk.v0.queue");
function inspectDefinition(value) {
  if (typeof value !== "object" && typeof value !== "function" || value === null) {
    return;
  }
  if (!Object.hasOwn(value, privateDefinitionBrand))
    return;
  const definition = value[privateDefinitionBrand];
  if (!isInternalDefinition(definition)) {
    throw new Error("invalid private definition record");
  }
  return definition;
}
function isQueue(value) {
  if (typeof value !== "object" || value === null)
    return false;
  if (!Object.hasOwn(value, privateQueueBrand))
    return false;
  if (value[privateQueueBrand] !== true) {
    throw new Error("invalid private queue record");
  }
  const queue = value;
  if (typeof queue.name !== "string") {
    throw new Error("invalid private queue record");
  }
  validateQueueName(queue.name);
  validateOptionalQueueConcurrencyLimit(queue.concurrencyLimit);
  return true;
}
function isInternalDefinition(value) {
  if (typeof value !== "object" || value === null)
    return false;
  const definition = value;
  if (typeof definition.id !== "string")
    return false;
  validateTaskId(definition.id);
  switch (definition.kind) {
    case "task":
      if (typeof definition.handler !== "function" || typeof definition.hasPayload !== "boolean") {
        return false;
      }
      if (definition.hasPayload) {
        assertPayloadSchema(definition.payloadSchema, `task ${JSON.stringify(definition.id)} payload`);
      } else if (Object.hasOwn(definition, "payloadSchema")) {
        return false;
      }
      return true;
    case "actor":
      return typeof definition.handler === "function";
    default:
      return false;
  }
}
// sdk/typescript/src/image.ts
var imageBrand = Symbol.for("helmr.sdk.v0.image");
var sourceFileBrand = Symbol.for("helmr.sdk.v0.source-file");
var sourceDirectoryBrand = Symbol.for("helmr.sdk.v0.source-directory");
class SourceFileValue {
  path;
  constructor(path) {
    this.path = path;
    Object.defineProperty(this, sourceFileBrand, { value: true });
    Object.freeze(this);
  }
}

class SourceDirectoryValue {
  path;
  constructor(path) {
    this.path = path;
    Object.defineProperty(this, sourceDirectoryBrand, { value: true });
    Object.freeze(this);
  }
}
var source = Object.freeze({
  file(path) {
    return new SourceFileValue(path);
  },
  directory(path) {
    return new SourceDirectoryValue(path);
  }
});
function inspectImage(value) {
  if (typeof value !== "object" || value === null || value[imageBrand] !== true) {
    return;
  }
  const imageValue = value;
  return { key: imageValue.key, steps: imageValue.steps };
}
// sdk/typescript/src/workspace.ts
var sandboxDefinitionBrand = Symbol.for("helmr.sdk.v0.sandbox");
var workspaceAddressBrand = Symbol.for("helmr.sdk.v0.workspace-address");
var workspaces = Object.freeze({
  ref: createWorkspaceRef
});
function inspectSandboxDefinition(value) {
  if (typeof value !== "object" || value === null)
    return;
  if (!Object.hasOwn(value, sandboxDefinitionBrand))
    return;
  if (value[sandboxDefinitionBrand] !== true) {
    throw new Error("invalid private Sandbox record");
  }
  const internal = value.internal;
  if (typeof internal !== "object" || internal === null || internal.kind !== "sandbox" || typeof internal.id !== "string" || typeof internal.image !== "object" || internal.image === null || typeof internal.resources !== "object" || internal.resources === null) {
    throw new Error("invalid private Sandbox record");
  }
  validateTaskId(internal.id);
  return internal;
}
function createWorkspaceRef(id) {
  const workspaceID = resourceID(id, "Workspace ID");
  const operations = {
    retrieve(options) {
      return currentRuntimeOperations().workspaceRetrieve(workspaceID, options?.signal);
    },
    exec(request, options) {
      return currentRuntimeOperations().workspaceExec(workspaceID, request, options?.signal);
    },
    delete(request, options) {
      return currentRuntimeOperations().workspaceDelete(workspaceID, request, options?.signal);
    }
  };
  return brandWorkspaceAddress({ id: workspaceID, ...operations });
}
function brandWorkspaceAddress(value) {
  resourceID(value.id, "Workspace ID");
  return freezeWorkspaceAddress(value);
}
function freezeWorkspaceAddress(value) {
  Object.defineProperty(value, workspaceAddressBrand, { value: true });
  return Object.freeze(value);
}
// sdk/typescript/src/internal/jsoncanon.ts
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
// compiler/typescript/src/program-compiler.ts
import { createWriteStream } from "node:fs";
import { mkdir as mkdir2, readFile as readFile3, writeFile as writeFile2 } from "node:fs/promises";
import { dirname as dirname2, resolve as resolve4 } from "node:path";

// compiler/typescript/src/analysis.ts
import { lstat, readdir, realpath } from "node:fs/promises";
import { relative, resolve, sep } from "node:path";

// compiler/typescript/src/utf8.ts
function compareUTF82(left, right) {
  return Buffer.compare(Buffer.from(left), Buffer.from(right));
}
function hasOnlyUnicodeScalarValues2(value) {
  for (let index = 0;index < value.length; index++) {
    const unit = value.charCodeAt(index);
    if (unit >= 55296 && unit <= 56319) {
      if (index + 1 === value.length)
        return false;
      const next = value.charCodeAt(index + 1);
      if (next < 56320 || next > 57343)
        return false;
      index++;
    } else if (unit >= 56320 && unit <= 57343) {
      return false;
    }
  }
  return true;
}

// compiler/typescript/src/analysis.ts
var executableExtension = /\.(?:cjs|cts|js|jsx|mjs|mts|ts|tsx)$/;
var textDecoder = new TextDecoder("utf-8", { fatal: true });
var maxVerificationFailureMessageBytes = 16 << 10;
var VERIFICATION_RESULT_FORMAT_VERSION = 0;
function successfulVerificationResult(analysis) {
  const files = [{
    path: "helmr/build-plan.json",
    content: decodeGeneratedFile(analysis.buildPlanBytes)
  }];
  if (analysis.programDeclarations.length > 0) {
    files.push({
      path: "helmr/analysis-locators.json",
      content: decodeGeneratedFile(analysis.declarationLocatorBytes)
    }, {
      path: "helmr/entry.mjs",
      content: decodeGeneratedFile(analysis.entrypointBytes)
    });
  }
  return Object.freeze({
    formatVersion: VERIFICATION_RESULT_FORMAT_VERSION,
    outcome: "succeeded",
    declarations: analysis.programDeclarations,
    files: Object.freeze(files.map((file) => Object.freeze(file)))
  });
}
function failedVerificationResult(message) {
  const encoded = new TextEncoder().encode(message);
  if (encoded.length === 0 || encoded.length > maxVerificationFailureMessageBytes || message.trim() === "") {
    throw new Error(`verification failure message must be nonblank UTF-8 of at most ${maxVerificationFailureMessageBytes} bytes`);
  }
  return Object.freeze({
    formatVersion: VERIFICATION_RESULT_FORMAT_VERSION,
    outcome: "failed",
    error: Object.freeze({
      reason: "verification_failed",
      message
    })
  });
}
function encodeVerificationResultFrame(result) {
  const body = canonicalizeJsonValue(result);
  const frame = new Uint8Array(4 + body.length);
  new DataView(frame.buffer).setUint32(0, body.length, false);
  frame.set(body, 4);
  return frame;
}
async function discoverModules(root, config) {
  const canonicalRoot = await realpath(root);
  await rejectReservedRoot(canonicalRoot);
  const candidates = new Set;
  for (const configured of config.dirs) {
    const directory = resolve(canonicalRoot, configured);
    if (!inside(canonicalRoot, directory)) {
      throw new Error(`configured dir escapes the project root: ${configured}`);
    }
    const relativeDirectory = projectPath(canonicalRoot, directory);
    if (hasComponent(relativeDirectory, "node_modules")) {
      throw new Error(`configured dir enters the dependency namespace: ${configured}`);
    }
    if (hasComponent(relativeDirectory, ".helmr")) {
      throw new Error(`configured dir enters reserved Platform output: ${configured}`);
    }
    await requireUnlinkedDirectory(canonicalRoot, directory, configured);
    await appendCandidates(canonicalRoot, directory, candidates);
  }
  const modules = [...candidates].filter((path) => !config.ignorePatterns.some((pattern) => matchesIgnorePattern(pattern, path)));
  modules.sort(compareUTF82);
  return modules;
}
async function appendCandidates(root, directory, candidates) {
  const entries = await readdir(directory, { withFileTypes: true });
  entries.sort((left, right) => compareUTF82(left.name, right.name));
  for (const entry of entries) {
    const absolute = resolve(directory, entry.name);
    const path = projectPath(root, absolute);
    if (hasComponent(path, "node_modules"))
      continue;
    if (entry.name === ".helmr") {
      throw new Error(`declaration tree contains reserved Platform output: ${path}`);
    }
    const metadata = await lstat(absolute);
    if (metadata.isSymbolicLink())
      continue;
    if (metadata.isDirectory()) {
      await appendCandidates(root, absolute, candidates);
      continue;
    }
    if (metadata.isFile() && executableExtension.test(path) && !isDeclarationOnly(path) && path !== "helmr.config.ts") {
      candidates.add(path);
      continue;
    }
    if (!metadata.isFile()) {
      throw new Error(`unsupported declaration tree entry: ${path}`);
    }
  }
}
function isDeclarationOnly(path) {
  return path.endsWith(".d.ts") || path.endsWith(".d.mts") || path.endsWith(".d.cts");
}
async function rejectReservedRoot(root) {
  try {
    await lstat(resolve(root, "helmr"));
  } catch (error) {
    if (error.code === "ENOENT")
      return;
    throw error;
  }
  throw new Error("project root helmr/ is reserved for Platform output");
}
async function requireUnlinkedDirectory(root, directory, configured) {
  let metadata;
  try {
    metadata = await lstat(directory);
  } catch (error) {
    if (error.code === "ENOENT") {
      throw new Error(`configured dir does not exist: ${configured}`);
    }
    throw error;
  }
  if (!metadata.isDirectory()) {
    throw new Error(`configured dir is not a regular directory: ${configured}`);
  }
  if (await realpath(directory) !== directory || !inside(root, directory)) {
    throw new Error(`configured dir traverses a symbolic link: ${configured}`);
  }
}
function projectPath(root, value) {
  return relative(root, value).split(sep).join("/");
}
function inside(root, value) {
  const path = relative(root, value);
  return path === "" || !path.startsWith(`..${sep}`) && path !== ".." && !path.startsWith("/");
}
function hasComponent(path, component) {
  return path.split("/").includes(component);
}
function decodeGeneratedFile(value) {
  try {
    return textDecoder.decode(value);
  } catch {
    throw new Error("generated analysis file is not valid UTF-8");
  }
}

// compiler/typescript/src/bundle.ts
import {
  build as build2,
  version as esbuildVersion
} from "esbuild";
import { createHash } from "node:crypto";
import { createRequire } from "node:module";
import {
  lstat as lstat3,
  mkdir,
  readFile as readFile2,
  realpath as realpath3,
  rm,
  stat as stat2,
  writeFile
} from "node:fs/promises";
import { dirname, relative as relative3, resolve as resolve3, sep as sep3 } from "node:path";
import { pathToFileURL } from "node:url";

// node_modules/.bun/jsonc-parser@3.3.1/node_modules/jsonc-parser/lib/esm/impl/scanner.js
function createScanner(text, ignoreTrivia = false) {
  const len = text.length;
  let pos = 0, value = "", tokenOffset = 0, token = 16, lineNumber = 0, lineStartOffset = 0, tokenLineStartOffset = 0, prevTokenLineStartOffset = 0, scanError = 0;
  function scanHexDigits(count, exact) {
    let digits = 0;
    let value2 = 0;
    while (digits < count || !exact) {
      let ch = text.charCodeAt(pos);
      if (ch >= 48 && ch <= 57) {
        value2 = value2 * 16 + ch - 48;
      } else if (ch >= 65 && ch <= 70) {
        value2 = value2 * 16 + ch - 65 + 10;
      } else if (ch >= 97 && ch <= 102) {
        value2 = value2 * 16 + ch - 97 + 10;
      } else {
        break;
      }
      pos++;
      digits++;
    }
    if (digits < count) {
      value2 = -1;
    }
    return value2;
  }
  function setPosition(newPosition) {
    pos = newPosition;
    value = "";
    tokenOffset = 0;
    token = 16;
    scanError = 0;
  }
  function scanNumber() {
    let start = pos;
    if (text.charCodeAt(pos) === 48) {
      pos++;
    } else {
      pos++;
      while (pos < text.length && isDigit(text.charCodeAt(pos))) {
        pos++;
      }
    }
    if (pos < text.length && text.charCodeAt(pos) === 46) {
      pos++;
      if (pos < text.length && isDigit(text.charCodeAt(pos))) {
        pos++;
        while (pos < text.length && isDigit(text.charCodeAt(pos))) {
          pos++;
        }
      } else {
        scanError = 3;
        return text.substring(start, pos);
      }
    }
    let end = pos;
    if (pos < text.length && (text.charCodeAt(pos) === 69 || text.charCodeAt(pos) === 101)) {
      pos++;
      if (pos < text.length && text.charCodeAt(pos) === 43 || text.charCodeAt(pos) === 45) {
        pos++;
      }
      if (pos < text.length && isDigit(text.charCodeAt(pos))) {
        pos++;
        while (pos < text.length && isDigit(text.charCodeAt(pos))) {
          pos++;
        }
        end = pos;
      } else {
        scanError = 3;
      }
    }
    return text.substring(start, end);
  }
  function scanString() {
    let result = "", start = pos;
    while (true) {
      if (pos >= len) {
        result += text.substring(start, pos);
        scanError = 2;
        break;
      }
      const ch = text.charCodeAt(pos);
      if (ch === 34) {
        result += text.substring(start, pos);
        pos++;
        break;
      }
      if (ch === 92) {
        result += text.substring(start, pos);
        pos++;
        if (pos >= len) {
          scanError = 2;
          break;
        }
        const ch2 = text.charCodeAt(pos++);
        switch (ch2) {
          case 34:
            result += '"';
            break;
          case 92:
            result += "\\";
            break;
          case 47:
            result += "/";
            break;
          case 98:
            result += "\b";
            break;
          case 102:
            result += "\f";
            break;
          case 110:
            result += `
`;
            break;
          case 114:
            result += "\r";
            break;
          case 116:
            result += "\t";
            break;
          case 117:
            const ch3 = scanHexDigits(4, true);
            if (ch3 >= 0) {
              result += String.fromCharCode(ch3);
            } else {
              scanError = 4;
            }
            break;
          default:
            scanError = 5;
        }
        start = pos;
        continue;
      }
      if (ch >= 0 && ch <= 31) {
        if (isLineBreak(ch)) {
          result += text.substring(start, pos);
          scanError = 2;
          break;
        } else {
          scanError = 6;
        }
      }
      pos++;
    }
    return result;
  }
  function scanNext() {
    value = "";
    scanError = 0;
    tokenOffset = pos;
    lineStartOffset = lineNumber;
    prevTokenLineStartOffset = tokenLineStartOffset;
    if (pos >= len) {
      tokenOffset = len;
      return token = 17;
    }
    let code = text.charCodeAt(pos);
    if (isWhiteSpace(code)) {
      do {
        pos++;
        value += String.fromCharCode(code);
        code = text.charCodeAt(pos);
      } while (isWhiteSpace(code));
      return token = 15;
    }
    if (isLineBreak(code)) {
      pos++;
      value += String.fromCharCode(code);
      if (code === 13 && text.charCodeAt(pos) === 10) {
        pos++;
        value += `
`;
      }
      lineNumber++;
      tokenLineStartOffset = pos;
      return token = 14;
    }
    switch (code) {
      case 123:
        pos++;
        return token = 1;
      case 125:
        pos++;
        return token = 2;
      case 91:
        pos++;
        return token = 3;
      case 93:
        pos++;
        return token = 4;
      case 58:
        pos++;
        return token = 6;
      case 44:
        pos++;
        return token = 5;
      case 34:
        pos++;
        value = scanString();
        return token = 10;
      case 47:
        const start = pos - 1;
        if (text.charCodeAt(pos + 1) === 47) {
          pos += 2;
          while (pos < len) {
            if (isLineBreak(text.charCodeAt(pos))) {
              break;
            }
            pos++;
          }
          value = text.substring(start, pos);
          return token = 12;
        }
        if (text.charCodeAt(pos + 1) === 42) {
          pos += 2;
          const safeLength = len - 1;
          let commentClosed = false;
          while (pos < safeLength) {
            const ch = text.charCodeAt(pos);
            if (ch === 42 && text.charCodeAt(pos + 1) === 47) {
              pos += 2;
              commentClosed = true;
              break;
            }
            pos++;
            if (isLineBreak(ch)) {
              if (ch === 13 && text.charCodeAt(pos) === 10) {
                pos++;
              }
              lineNumber++;
              tokenLineStartOffset = pos;
            }
          }
          if (!commentClosed) {
            pos++;
            scanError = 1;
          }
          value = text.substring(start, pos);
          return token = 13;
        }
        value += String.fromCharCode(code);
        pos++;
        return token = 16;
      case 45:
        value += String.fromCharCode(code);
        pos++;
        if (pos === len || !isDigit(text.charCodeAt(pos))) {
          return token = 16;
        }
      case 48:
      case 49:
      case 50:
      case 51:
      case 52:
      case 53:
      case 54:
      case 55:
      case 56:
      case 57:
        value += scanNumber();
        return token = 11;
      default:
        while (pos < len && isUnknownContentCharacter(code)) {
          pos++;
          code = text.charCodeAt(pos);
        }
        if (tokenOffset !== pos) {
          value = text.substring(tokenOffset, pos);
          switch (value) {
            case "true":
              return token = 8;
            case "false":
              return token = 9;
            case "null":
              return token = 7;
          }
          return token = 16;
        }
        value += String.fromCharCode(code);
        pos++;
        return token = 16;
    }
  }
  function isUnknownContentCharacter(code) {
    if (isWhiteSpace(code) || isLineBreak(code)) {
      return false;
    }
    switch (code) {
      case 125:
      case 93:
      case 123:
      case 91:
      case 34:
      case 58:
      case 44:
      case 47:
        return false;
    }
    return true;
  }
  function scanNextNonTrivia() {
    let result;
    do {
      result = scanNext();
    } while (result >= 12 && result <= 15);
    return result;
  }
  return {
    setPosition,
    getPosition: () => pos,
    scan: ignoreTrivia ? scanNextNonTrivia : scanNext,
    getToken: () => token,
    getTokenValue: () => value,
    getTokenOffset: () => tokenOffset,
    getTokenLength: () => pos - tokenOffset,
    getTokenStartLine: () => lineStartOffset,
    getTokenStartCharacter: () => tokenOffset - prevTokenLineStartOffset,
    getTokenError: () => scanError
  };
}
function isWhiteSpace(ch) {
  return ch === 32 || ch === 9;
}
function isLineBreak(ch) {
  return ch === 10 || ch === 13;
}
function isDigit(ch) {
  return ch >= 48 && ch <= 57;
}
var CharacterCodes;
(function(CharacterCodes2) {
  CharacterCodes2[CharacterCodes2["lineFeed"] = 10] = "lineFeed";
  CharacterCodes2[CharacterCodes2["carriageReturn"] = 13] = "carriageReturn";
  CharacterCodes2[CharacterCodes2["space"] = 32] = "space";
  CharacterCodes2[CharacterCodes2["_0"] = 48] = "_0";
  CharacterCodes2[CharacterCodes2["_1"] = 49] = "_1";
  CharacterCodes2[CharacterCodes2["_2"] = 50] = "_2";
  CharacterCodes2[CharacterCodes2["_3"] = 51] = "_3";
  CharacterCodes2[CharacterCodes2["_4"] = 52] = "_4";
  CharacterCodes2[CharacterCodes2["_5"] = 53] = "_5";
  CharacterCodes2[CharacterCodes2["_6"] = 54] = "_6";
  CharacterCodes2[CharacterCodes2["_7"] = 55] = "_7";
  CharacterCodes2[CharacterCodes2["_8"] = 56] = "_8";
  CharacterCodes2[CharacterCodes2["_9"] = 57] = "_9";
  CharacterCodes2[CharacterCodes2["a"] = 97] = "a";
  CharacterCodes2[CharacterCodes2["b"] = 98] = "b";
  CharacterCodes2[CharacterCodes2["c"] = 99] = "c";
  CharacterCodes2[CharacterCodes2["d"] = 100] = "d";
  CharacterCodes2[CharacterCodes2["e"] = 101] = "e";
  CharacterCodes2[CharacterCodes2["f"] = 102] = "f";
  CharacterCodes2[CharacterCodes2["g"] = 103] = "g";
  CharacterCodes2[CharacterCodes2["h"] = 104] = "h";
  CharacterCodes2[CharacterCodes2["i"] = 105] = "i";
  CharacterCodes2[CharacterCodes2["j"] = 106] = "j";
  CharacterCodes2[CharacterCodes2["k"] = 107] = "k";
  CharacterCodes2[CharacterCodes2["l"] = 108] = "l";
  CharacterCodes2[CharacterCodes2["m"] = 109] = "m";
  CharacterCodes2[CharacterCodes2["n"] = 110] = "n";
  CharacterCodes2[CharacterCodes2["o"] = 111] = "o";
  CharacterCodes2[CharacterCodes2["p"] = 112] = "p";
  CharacterCodes2[CharacterCodes2["q"] = 113] = "q";
  CharacterCodes2[CharacterCodes2["r"] = 114] = "r";
  CharacterCodes2[CharacterCodes2["s"] = 115] = "s";
  CharacterCodes2[CharacterCodes2["t"] = 116] = "t";
  CharacterCodes2[CharacterCodes2["u"] = 117] = "u";
  CharacterCodes2[CharacterCodes2["v"] = 118] = "v";
  CharacterCodes2[CharacterCodes2["w"] = 119] = "w";
  CharacterCodes2[CharacterCodes2["x"] = 120] = "x";
  CharacterCodes2[CharacterCodes2["y"] = 121] = "y";
  CharacterCodes2[CharacterCodes2["z"] = 122] = "z";
  CharacterCodes2[CharacterCodes2["A"] = 65] = "A";
  CharacterCodes2[CharacterCodes2["B"] = 66] = "B";
  CharacterCodes2[CharacterCodes2["C"] = 67] = "C";
  CharacterCodes2[CharacterCodes2["D"] = 68] = "D";
  CharacterCodes2[CharacterCodes2["E"] = 69] = "E";
  CharacterCodes2[CharacterCodes2["F"] = 70] = "F";
  CharacterCodes2[CharacterCodes2["G"] = 71] = "G";
  CharacterCodes2[CharacterCodes2["H"] = 72] = "H";
  CharacterCodes2[CharacterCodes2["I"] = 73] = "I";
  CharacterCodes2[CharacterCodes2["J"] = 74] = "J";
  CharacterCodes2[CharacterCodes2["K"] = 75] = "K";
  CharacterCodes2[CharacterCodes2["L"] = 76] = "L";
  CharacterCodes2[CharacterCodes2["M"] = 77] = "M";
  CharacterCodes2[CharacterCodes2["N"] = 78] = "N";
  CharacterCodes2[CharacterCodes2["O"] = 79] = "O";
  CharacterCodes2[CharacterCodes2["P"] = 80] = "P";
  CharacterCodes2[CharacterCodes2["Q"] = 81] = "Q";
  CharacterCodes2[CharacterCodes2["R"] = 82] = "R";
  CharacterCodes2[CharacterCodes2["S"] = 83] = "S";
  CharacterCodes2[CharacterCodes2["T"] = 84] = "T";
  CharacterCodes2[CharacterCodes2["U"] = 85] = "U";
  CharacterCodes2[CharacterCodes2["V"] = 86] = "V";
  CharacterCodes2[CharacterCodes2["W"] = 87] = "W";
  CharacterCodes2[CharacterCodes2["X"] = 88] = "X";
  CharacterCodes2[CharacterCodes2["Y"] = 89] = "Y";
  CharacterCodes2[CharacterCodes2["Z"] = 90] = "Z";
  CharacterCodes2[CharacterCodes2["asterisk"] = 42] = "asterisk";
  CharacterCodes2[CharacterCodes2["backslash"] = 92] = "backslash";
  CharacterCodes2[CharacterCodes2["closeBrace"] = 125] = "closeBrace";
  CharacterCodes2[CharacterCodes2["closeBracket"] = 93] = "closeBracket";
  CharacterCodes2[CharacterCodes2["colon"] = 58] = "colon";
  CharacterCodes2[CharacterCodes2["comma"] = 44] = "comma";
  CharacterCodes2[CharacterCodes2["dot"] = 46] = "dot";
  CharacterCodes2[CharacterCodes2["doubleQuote"] = 34] = "doubleQuote";
  CharacterCodes2[CharacterCodes2["minus"] = 45] = "minus";
  CharacterCodes2[CharacterCodes2["openBrace"] = 123] = "openBrace";
  CharacterCodes2[CharacterCodes2["openBracket"] = 91] = "openBracket";
  CharacterCodes2[CharacterCodes2["plus"] = 43] = "plus";
  CharacterCodes2[CharacterCodes2["slash"] = 47] = "slash";
  CharacterCodes2[CharacterCodes2["formFeed"] = 12] = "formFeed";
  CharacterCodes2[CharacterCodes2["tab"] = 9] = "tab";
})(CharacterCodes || (CharacterCodes = {}));

// node_modules/.bun/jsonc-parser@3.3.1/node_modules/jsonc-parser/lib/esm/impl/string-intern.js
var cachedSpaces = new Array(20).fill(0).map((_, index) => {
  return " ".repeat(index);
});
var maxCachedValues = 200;
var cachedBreakLinesWithSpaces = {
  " ": {
    "\n": new Array(maxCachedValues).fill(0).map((_, index) => {
      return `
` + " ".repeat(index);
    }),
    "\r": new Array(maxCachedValues).fill(0).map((_, index) => {
      return "\r" + " ".repeat(index);
    }),
    "\r\n": new Array(maxCachedValues).fill(0).map((_, index) => {
      return `\r
` + " ".repeat(index);
    })
  },
  "\t": {
    "\n": new Array(maxCachedValues).fill(0).map((_, index) => {
      return `
` + "\t".repeat(index);
    }),
    "\r": new Array(maxCachedValues).fill(0).map((_, index) => {
      return "\r" + "\t".repeat(index);
    }),
    "\r\n": new Array(maxCachedValues).fill(0).map((_, index) => {
      return `\r
` + "\t".repeat(index);
    })
  }
};

// node_modules/.bun/jsonc-parser@3.3.1/node_modules/jsonc-parser/lib/esm/impl/parser.js
var ParseOptions;
(function(ParseOptions2) {
  ParseOptions2.DEFAULT = {
    allowTrailingComma: false
  };
})(ParseOptions || (ParseOptions = {}));
function parse(text, errors = [], options = ParseOptions.DEFAULT) {
  let currentProperty = null;
  let currentParent = [];
  const previousParents = [];
  function onValue(value) {
    if (Array.isArray(currentParent)) {
      currentParent.push(value);
    } else if (currentProperty !== null) {
      currentParent[currentProperty] = value;
    }
  }
  const visitor = {
    onObjectBegin: () => {
      const object = {};
      onValue(object);
      previousParents.push(currentParent);
      currentParent = object;
      currentProperty = null;
    },
    onObjectProperty: (name) => {
      currentProperty = name;
    },
    onObjectEnd: () => {
      currentParent = previousParents.pop();
    },
    onArrayBegin: () => {
      const array = [];
      onValue(array);
      previousParents.push(currentParent);
      currentParent = array;
      currentProperty = null;
    },
    onArrayEnd: () => {
      currentParent = previousParents.pop();
    },
    onLiteralValue: onValue,
    onError: (error, offset, length) => {
      errors.push({ error, offset, length });
    }
  };
  visit(text, visitor, options);
  return currentParent[0];
}
function parseTree(text, errors = [], options = ParseOptions.DEFAULT) {
  let currentParent = { type: "array", offset: -1, length: -1, children: [], parent: undefined };
  function ensurePropertyComplete(endOffset) {
    if (currentParent.type === "property") {
      currentParent.length = endOffset - currentParent.offset;
      currentParent = currentParent.parent;
    }
  }
  function onValue(valueNode) {
    currentParent.children.push(valueNode);
    return valueNode;
  }
  const visitor = {
    onObjectBegin: (offset) => {
      currentParent = onValue({ type: "object", offset, length: -1, parent: currentParent, children: [] });
    },
    onObjectProperty: (name, offset, length) => {
      currentParent = onValue({ type: "property", offset, length: -1, parent: currentParent, children: [] });
      currentParent.children.push({ type: "string", value: name, offset, length, parent: currentParent });
    },
    onObjectEnd: (offset, length) => {
      ensurePropertyComplete(offset + length);
      currentParent.length = offset + length - currentParent.offset;
      currentParent = currentParent.parent;
      ensurePropertyComplete(offset + length);
    },
    onArrayBegin: (offset, length) => {
      currentParent = onValue({ type: "array", offset, length: -1, parent: currentParent, children: [] });
    },
    onArrayEnd: (offset, length) => {
      currentParent.length = offset + length - currentParent.offset;
      currentParent = currentParent.parent;
      ensurePropertyComplete(offset + length);
    },
    onLiteralValue: (value, offset, length) => {
      onValue({ type: getNodeType(value), offset, length, parent: currentParent, value });
      ensurePropertyComplete(offset + length);
    },
    onSeparator: (sep2, offset, length) => {
      if (currentParent.type === "property") {
        if (sep2 === ":") {
          currentParent.colonOffset = offset;
        } else if (sep2 === ",") {
          ensurePropertyComplete(offset);
        }
      }
    },
    onError: (error, offset, length) => {
      errors.push({ error, offset, length });
    }
  };
  visit(text, visitor, options);
  const result = currentParent.children[0];
  if (result) {
    delete result.parent;
  }
  return result;
}
function visit(text, visitor, options = ParseOptions.DEFAULT) {
  const _scanner = createScanner(text, false);
  const _jsonPath = [];
  let suppressedCallbacks = 0;
  function toNoArgVisit(visitFunction) {
    return visitFunction ? () => suppressedCallbacks === 0 && visitFunction(_scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter()) : () => true;
  }
  function toOneArgVisit(visitFunction) {
    return visitFunction ? (arg) => suppressedCallbacks === 0 && visitFunction(arg, _scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter()) : () => true;
  }
  function toOneArgVisitWithPath(visitFunction) {
    return visitFunction ? (arg) => suppressedCallbacks === 0 && visitFunction(arg, _scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter(), () => _jsonPath.slice()) : () => true;
  }
  function toBeginVisit(visitFunction) {
    return visitFunction ? () => {
      if (suppressedCallbacks > 0) {
        suppressedCallbacks++;
      } else {
        let cbReturn = visitFunction(_scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter(), () => _jsonPath.slice());
        if (cbReturn === false) {
          suppressedCallbacks = 1;
        }
      }
    } : () => true;
  }
  function toEndVisit(visitFunction) {
    return visitFunction ? () => {
      if (suppressedCallbacks > 0) {
        suppressedCallbacks--;
      }
      if (suppressedCallbacks === 0) {
        visitFunction(_scanner.getTokenOffset(), _scanner.getTokenLength(), _scanner.getTokenStartLine(), _scanner.getTokenStartCharacter());
      }
    } : () => true;
  }
  const onObjectBegin = toBeginVisit(visitor.onObjectBegin), onObjectProperty = toOneArgVisitWithPath(visitor.onObjectProperty), onObjectEnd = toEndVisit(visitor.onObjectEnd), onArrayBegin = toBeginVisit(visitor.onArrayBegin), onArrayEnd = toEndVisit(visitor.onArrayEnd), onLiteralValue = toOneArgVisitWithPath(visitor.onLiteralValue), onSeparator = toOneArgVisit(visitor.onSeparator), onComment = toNoArgVisit(visitor.onComment), onError = toOneArgVisit(visitor.onError);
  const disallowComments = options && options.disallowComments;
  const allowTrailingComma = options && options.allowTrailingComma;
  function scanNext() {
    while (true) {
      const token = _scanner.scan();
      switch (_scanner.getTokenError()) {
        case 4:
          handleError(14);
          break;
        case 5:
          handleError(15);
          break;
        case 3:
          handleError(13);
          break;
        case 1:
          if (!disallowComments) {
            handleError(11);
          }
          break;
        case 2:
          handleError(12);
          break;
        case 6:
          handleError(16);
          break;
      }
      switch (token) {
        case 12:
        case 13:
          if (disallowComments) {
            handleError(10);
          } else {
            onComment();
          }
          break;
        case 16:
          handleError(1);
          break;
        case 15:
        case 14:
          break;
        default:
          return token;
      }
    }
  }
  function handleError(error, skipUntilAfter = [], skipUntil = []) {
    onError(error);
    if (skipUntilAfter.length + skipUntil.length > 0) {
      let token = _scanner.getToken();
      while (token !== 17) {
        if (skipUntilAfter.indexOf(token) !== -1) {
          scanNext();
          break;
        } else if (skipUntil.indexOf(token) !== -1) {
          break;
        }
        token = scanNext();
      }
    }
  }
  function parseString(isValue) {
    const value = _scanner.getTokenValue();
    if (isValue) {
      onLiteralValue(value);
    } else {
      onObjectProperty(value);
      _jsonPath.push(value);
    }
    scanNext();
    return true;
  }
  function parseLiteral() {
    switch (_scanner.getToken()) {
      case 11:
        const tokenValue = _scanner.getTokenValue();
        let value = Number(tokenValue);
        if (isNaN(value)) {
          handleError(2);
          value = 0;
        }
        onLiteralValue(value);
        break;
      case 7:
        onLiteralValue(null);
        break;
      case 8:
        onLiteralValue(true);
        break;
      case 9:
        onLiteralValue(false);
        break;
      default:
        return false;
    }
    scanNext();
    return true;
  }
  function parseProperty() {
    if (_scanner.getToken() !== 10) {
      handleError(3, [], [2, 5]);
      return false;
    }
    parseString(false);
    if (_scanner.getToken() === 6) {
      onSeparator(":");
      scanNext();
      if (!parseValue()) {
        handleError(4, [], [2, 5]);
      }
    } else {
      handleError(5, [], [2, 5]);
    }
    _jsonPath.pop();
    return true;
  }
  function parseObject() {
    onObjectBegin();
    scanNext();
    let needsComma = false;
    while (_scanner.getToken() !== 2 && _scanner.getToken() !== 17) {
      if (_scanner.getToken() === 5) {
        if (!needsComma) {
          handleError(4, [], []);
        }
        onSeparator(",");
        scanNext();
        if (_scanner.getToken() === 2 && allowTrailingComma) {
          break;
        }
      } else if (needsComma) {
        handleError(6, [], []);
      }
      if (!parseProperty()) {
        handleError(4, [], [2, 5]);
      }
      needsComma = true;
    }
    onObjectEnd();
    if (_scanner.getToken() !== 2) {
      handleError(7, [2], []);
    } else {
      scanNext();
    }
    return true;
  }
  function parseArray() {
    onArrayBegin();
    scanNext();
    let isFirstElement = true;
    let needsComma = false;
    while (_scanner.getToken() !== 4 && _scanner.getToken() !== 17) {
      if (_scanner.getToken() === 5) {
        if (!needsComma) {
          handleError(4, [], []);
        }
        onSeparator(",");
        scanNext();
        if (_scanner.getToken() === 4 && allowTrailingComma) {
          break;
        }
      } else if (needsComma) {
        handleError(6, [], []);
      }
      if (isFirstElement) {
        _jsonPath.push(0);
        isFirstElement = false;
      } else {
        _jsonPath[_jsonPath.length - 1]++;
      }
      if (!parseValue()) {
        handleError(4, [], [4, 5]);
      }
      needsComma = true;
    }
    onArrayEnd();
    if (!isFirstElement) {
      _jsonPath.pop();
    }
    if (_scanner.getToken() !== 4) {
      handleError(8, [4], []);
    } else {
      scanNext();
    }
    return true;
  }
  function parseValue() {
    switch (_scanner.getToken()) {
      case 3:
        return parseArray();
      case 1:
        return parseObject();
      case 10:
        return parseString(true);
      default:
        return parseLiteral();
    }
  }
  scanNext();
  if (_scanner.getToken() === 17) {
    if (options.allowEmptyContent) {
      return true;
    }
    handleError(4, [], []);
    return false;
  }
  if (!parseValue()) {
    handleError(4, [], []);
    return false;
  }
  if (_scanner.getToken() !== 17) {
    handleError(9, [], []);
  }
  return true;
}
function getNodeType(value) {
  switch (typeof value) {
    case "boolean":
      return "boolean";
    case "number":
      return "number";
    case "string":
      return "string";
    case "object": {
      if (!value) {
        return "null";
      } else if (Array.isArray(value)) {
        return "array";
      }
      return "object";
    }
    default:
      return "null";
  }
}

// node_modules/.bun/jsonc-parser@3.3.1/node_modules/jsonc-parser/lib/esm/main.js
var ScanError;
(function(ScanError2) {
  ScanError2[ScanError2["None"] = 0] = "None";
  ScanError2[ScanError2["UnexpectedEndOfComment"] = 1] = "UnexpectedEndOfComment";
  ScanError2[ScanError2["UnexpectedEndOfString"] = 2] = "UnexpectedEndOfString";
  ScanError2[ScanError2["UnexpectedEndOfNumber"] = 3] = "UnexpectedEndOfNumber";
  ScanError2[ScanError2["InvalidUnicode"] = 4] = "InvalidUnicode";
  ScanError2[ScanError2["InvalidEscapeCharacter"] = 5] = "InvalidEscapeCharacter";
  ScanError2[ScanError2["InvalidCharacter"] = 6] = "InvalidCharacter";
})(ScanError || (ScanError = {}));
var SyntaxKind;
(function(SyntaxKind2) {
  SyntaxKind2[SyntaxKind2["OpenBraceToken"] = 1] = "OpenBraceToken";
  SyntaxKind2[SyntaxKind2["CloseBraceToken"] = 2] = "CloseBraceToken";
  SyntaxKind2[SyntaxKind2["OpenBracketToken"] = 3] = "OpenBracketToken";
  SyntaxKind2[SyntaxKind2["CloseBracketToken"] = 4] = "CloseBracketToken";
  SyntaxKind2[SyntaxKind2["CommaToken"] = 5] = "CommaToken";
  SyntaxKind2[SyntaxKind2["ColonToken"] = 6] = "ColonToken";
  SyntaxKind2[SyntaxKind2["NullKeyword"] = 7] = "NullKeyword";
  SyntaxKind2[SyntaxKind2["TrueKeyword"] = 8] = "TrueKeyword";
  SyntaxKind2[SyntaxKind2["FalseKeyword"] = 9] = "FalseKeyword";
  SyntaxKind2[SyntaxKind2["StringLiteral"] = 10] = "StringLiteral";
  SyntaxKind2[SyntaxKind2["NumericLiteral"] = 11] = "NumericLiteral";
  SyntaxKind2[SyntaxKind2["LineCommentTrivia"] = 12] = "LineCommentTrivia";
  SyntaxKind2[SyntaxKind2["BlockCommentTrivia"] = 13] = "BlockCommentTrivia";
  SyntaxKind2[SyntaxKind2["LineBreakTrivia"] = 14] = "LineBreakTrivia";
  SyntaxKind2[SyntaxKind2["Trivia"] = 15] = "Trivia";
  SyntaxKind2[SyntaxKind2["Unknown"] = 16] = "Unknown";
  SyntaxKind2[SyntaxKind2["EOF"] = 17] = "EOF";
})(SyntaxKind || (SyntaxKind = {}));
var parse2 = parse;
var parseTree2 = parseTree;
var ParseErrorCode;
(function(ParseErrorCode2) {
  ParseErrorCode2[ParseErrorCode2["InvalidSymbol"] = 1] = "InvalidSymbol";
  ParseErrorCode2[ParseErrorCode2["InvalidNumberFormat"] = 2] = "InvalidNumberFormat";
  ParseErrorCode2[ParseErrorCode2["PropertyNameExpected"] = 3] = "PropertyNameExpected";
  ParseErrorCode2[ParseErrorCode2["ValueExpected"] = 4] = "ValueExpected";
  ParseErrorCode2[ParseErrorCode2["ColonExpected"] = 5] = "ColonExpected";
  ParseErrorCode2[ParseErrorCode2["CommaExpected"] = 6] = "CommaExpected";
  ParseErrorCode2[ParseErrorCode2["CloseBraceExpected"] = 7] = "CloseBraceExpected";
  ParseErrorCode2[ParseErrorCode2["CloseBracketExpected"] = 8] = "CloseBracketExpected";
  ParseErrorCode2[ParseErrorCode2["EndOfFileExpected"] = 9] = "EndOfFileExpected";
  ParseErrorCode2[ParseErrorCode2["InvalidCommentToken"] = 10] = "InvalidCommentToken";
  ParseErrorCode2[ParseErrorCode2["UnexpectedEndOfComment"] = 11] = "UnexpectedEndOfComment";
  ParseErrorCode2[ParseErrorCode2["UnexpectedEndOfString"] = 12] = "UnexpectedEndOfString";
  ParseErrorCode2[ParseErrorCode2["UnexpectedEndOfNumber"] = 13] = "UnexpectedEndOfNumber";
  ParseErrorCode2[ParseErrorCode2["InvalidUnicode"] = 14] = "InvalidUnicode";
  ParseErrorCode2[ParseErrorCode2["InvalidEscapeCharacter"] = 15] = "InvalidEscapeCharacter";
  ParseErrorCode2[ParseErrorCode2["InvalidCharacter"] = 16] = "InvalidCharacter";
})(ParseErrorCode || (ParseErrorCode = {}));

// compiler/typescript/src/compile.ts
var BUILD_PLAN_FORMAT_VERSION = 0;
var DECLARATION_LOCATOR_FORMAT_VERSION = 0;
var PROGRAM_ENTRYPOINT = `import { runProgram } from "file:///opt/helmr/runtime/helmr/entry.mjs";
await runProgram(new URL("./declarations.json", import.meta.url));
`;
function analyze(options) {
  const located = locateDefinitions(options);
  const queues = compileQueues(located, options.exports);
  const sandboxExports = new Map;
  for (const item of located) {
    if (item.definition.kind === "sandbox") {
      sandboxExports.set(item.definition.id, item.value);
    }
  }
  const definitions = located.map(({ definition }) => compileDefinition(definition, options, queues, sandboxExports));
  const programExports = compileProgramExports(located);
  const buildPlan = Object.freeze({
    formatVersion: BUILD_PLAN_FORMAT_VERSION,
    definitions: Object.freeze(definitions),
    queues: Object.freeze([...queues.values()].map((entry) => Object.freeze({ ...entry })).sort((left, right) => compareUTF82(left.name, right.name)))
  });
  const declarationLocator = Object.freeze({
    declarations: programExports.declarationLocator.declarations,
    formatVersion: DECLARATION_LOCATOR_FORMAT_VERSION
  });
  return {
    buildPlan,
    buildPlanBytes: canonicalizeJsonValue(buildPlan),
    declarationLocator,
    declarationLocatorBytes: canonicalizeJsonValue(declarationLocator),
    programDeclarations: programExports.programDeclarations,
    entrypointBytes: new TextEncoder().encode(PROGRAM_ENTRYPOINT)
  };
}
function analyzeProgramExports(options) {
  return compileProgramExports(locateDefinitions(options));
}
function locateDefinitions(options) {
  if (options.architecture !== "x86_64") {
    throw new Error(`unsupported architecture ${JSON.stringify(options.architecture)}`);
  }
  const located = discoverDefinitions(options.exports);
  if (located.length === 0) {
    throw new Error("BuildPlan definitions must be non-empty");
  }
  if (located.length > 1e4) {
    throw new Error("BuildPlan definitions exceed 10000");
  }
  located.sort(compareLocatedDefinitions);
  return located;
}
function compileProgramExports(located) {
  const declarations = located.flatMap((item) => item.definition.kind === "sandbox" ? [] : [locatorEntry(item)]);
  const programDeclarations = located.flatMap(({ definition }) => definition.kind === "sandbox" ? [] : [programDeclaration(definition)]);
  return Object.freeze({
    declarationLocator: Object.freeze({
      declarations: Object.freeze(declarations),
      formatVersion: DECLARATION_LOCATOR_FORMAT_VERSION
    }),
    programDeclarations: Object.freeze(programDeclarations)
  });
}
function normalizeWorkspaceResources(resources) {
  return Object.freeze({
    milliCpu: normalizeCpu(resources.cpu),
    memoryMiB: normalizeIecMiB(resources.memory, "memory")
  });
}
function discoverDefinitions(exports) {
  const identities = new Map;
  for (const item of exports) {
    const definition = inspectDefinition(item.value) ?? inspectSandboxDefinition(item.value);
    if (definition === undefined)
      continue;
    validateModulePath(item.modulePath);
    validateExportName(item.exportName);
    const key = `${definition.kind}\x00${definition.id}`;
    const existing = identities.get(key);
    if (existing !== undefined) {
      if (existing.value === item.value) {
        const candidate = {
          definition,
          modulePath: item.modulePath,
          exportName: item.exportName,
          value: item.value
        };
        if (compareLocatorOccurrence(candidate, existing.located) < 0) {
          existing.located = candidate;
        }
        continue;
      }
      throw new Error(`duplicate ${definition.kind} declaration ${JSON.stringify(definition.id)} at ${existing.located.modulePath}#${existing.located.exportName} and ${item.modulePath}#${item.exportName}`);
    }
    const located = {
      definition,
      modulePath: item.modulePath,
      exportName: item.exportName,
      value: item.value
    };
    identities.set(key, {
      value: item.value,
      located
    });
  }
  return [...identities.values()].map((item) => item.located);
}
function compileDefinition(definition, options, queues, sandboxExports) {
  switch (definition.kind) {
    case "task":
      return {
        kind: "task",
        declaredId: definition.id,
        manifest: {
          payload: {
            kind: definition.hasPayload ? "standard_schema" : "none"
          },
          run: normalizeRun(definition, "task", queues),
          ...definition.schedule === undefined ? {} : {
            schedule: compileSchedule(definition, sandboxExports)
          }
        }
      };
    case "actor":
      return {
        kind: "actor",
        declaredId: definition.id,
        manifest: {
          run: normalizeRun(definition, "actor", queues),
          idleTimeoutMs: definition.idleTimeout === undefined ? 30000 : normalizeDuration(definition.idleTimeout, `actor ${JSON.stringify(definition.id)} idleTimeout`, 1, 3600000)
        }
      };
    case "sandbox":
      return {
        kind: "sandbox",
        declaredId: definition.id,
        manifest: {
          imageBuild: compileImageBuild(definition.image, options),
          resources: normalizeWorkspaceResources(definition.resources)
        }
      };
  }
}
function compileSchedule(definition, sandboxExports) {
  const schedule = definition.schedule;
  if (schedule === undefined)
    throw new Error("Task schedule is undefined");
  const sandbox = inspectSandboxDefinition(schedule.workspace.sandbox);
  if (sandbox === undefined) {
    throw new Error(`task ${JSON.stringify(definition.id)} schedule has an invalid Sandbox definition`);
  }
  const exported = sandboxExports.get(sandbox.id);
  if (exported === undefined) {
    throw new Error(`task ${JSON.stringify(definition.id)} schedule references unexported Sandbox ${JSON.stringify(sandbox.id)}`);
  }
  if (exported !== schedule.workspace.sandbox) {
    throw new Error(`task ${JSON.stringify(definition.id)} schedule references a different Sandbox object than the exported definition ${JSON.stringify(sandbox.id)}`);
  }
  return {
    cron: schedule.cron,
    timezone: schedule.timezone,
    workspace: {
      sandboxId: sandbox.id,
      secrets: schedule.workspace.secrets
    }
  };
}
function compileQueues(located, exports) {
  const queues = new Map;
  for (const item of exports) {
    if (isQueue(item.value)) {
      addQueue(queues, item.value, item.value);
    }
  }
  for (const { definition } of located) {
    if (definition.kind !== "task" && definition.kind !== "actor")
      continue;
    if (typeof definition.queue === "object") {
      addQueue(queues, definition.queue, definition.queue);
    } else if (definition.queue === undefined) {
      addQueue(queues, {
        name: `${definition.kind}/${definition.id}`
      }, definition);
    }
  }
  for (const { definition } of located) {
    if ((definition.kind === "task" || definition.kind === "actor") && typeof definition.queue === "string" && !queues.has(definition.queue)) {
      throw new Error(`${definition.kind} ${JSON.stringify(definition.id)} references undefined queue ${JSON.stringify(definition.queue)}`);
    }
  }
  if (queues.size > 1000)
    throw new Error("BuildPlan queues exceed 1000");
  return new Map([...queues].map(([name, entry]) => [name, entry.queue]));
}
function addQueue(queues, queue, owner) {
  validateQueueName(queue.name);
  const next = {
    name: queue.name,
    ...queue.concurrencyLimit === undefined || queue.concurrencyLimit === null ? {} : { concurrencyLimit: queue.concurrencyLimit }
  };
  const existing = queues.get(queue.name);
  if (existing !== undefined) {
    if (existing.owner === owner)
      return;
    throw new Error(`duplicate queue declaration ${JSON.stringify(queue.name)}`);
  }
  queues.set(queue.name, { owner, queue: next });
}
function normalizeRun(definition, kind, queues) {
  const queue = definition.queue === undefined ? `${kind}/${definition.id}` : typeof definition.queue === "string" ? definition.queue : definition.queue.name;
  if (!queues.has(queue)) {
    throw new Error(`${kind} ${JSON.stringify(definition.id)} queue is undefined`);
  }
  const maxDurationMs = definition.maxDuration === undefined ? 900000 : normalizeDuration(definition.maxDuration, `${kind} ${JSON.stringify(definition.id)} maxDuration`, 5000, 86400000);
  return {
    queue,
    maxDurationMs,
    retry: normalizeRetry(definition.retry),
    ...definition.ttl === undefined ? {} : {
      ttlMs: normalizeDuration(definition.ttl, `${kind} ${JSON.stringify(definition.id)} ttl`, 1, 31536000000)
    }
  };
}
function normalizeRetry(retry) {
  if (retry === undefined || retry.enabled === false) {
    return { enabled: false };
  }
  if (!Number.isInteger(retry.maxAttempts) || retry.maxAttempts < 1 || retry.maxAttempts > 10) {
    throw new Error("retry maxAttempts must be an integer in [1,10]");
  }
  const minMs = retry.backoff?.minDelay === undefined ? 1000 : normalizeDuration(retry.backoff.minDelay, "retry backoff minDelay", 1, 86400000);
  const maxMs = retry.backoff?.maxDelay === undefined ? 30000 : normalizeDuration(retry.backoff.maxDelay, "retry backoff maxDelay", 1, 86400000);
  const factor = retry.backoff?.factor ?? 2;
  const jitter = retry.backoff?.jitter ?? "full";
  if (minMs > maxMs) {
    throw new Error("retry backoff minDelay must not exceed maxDelay");
  }
  if (!Number.isSafeInteger(factor) || factor < 1 || factor > 100) {
    throw new Error("retry backoff factor must be an integer in [1,100]");
  }
  if (jitter !== "none" && jitter !== "full") {
    throw new Error("retry backoff jitter must be none or full");
  }
  return {
    enabled: true,
    maxAttempts: retry.maxAttempts,
    backoff: { minMs, maxMs, factor, jitter }
  };
}
function compileImageBuild(root, options) {
  const images = new Map;
  const visiting = new Set;
  const visit2 = (image) => {
    if (visiting.has(image.key)) {
      throw new Error(`image graph contains a cycle at ${JSON.stringify(image.key)}`);
    }
    const existing = images.get(image.key);
    if (existing !== undefined) {
      if (existing !== image) {
        throw new Error(`image key ${JSON.stringify(image.key)} is not unique`);
      }
      return;
    }
    visiting.add(image.key);
    images.set(image.key, image);
    for (const step of image.steps) {
      if (step.kind === "copy_from_image") {
        const source2 = inspectImage(step.source);
        if (source2 === undefined)
          throw new Error("invalid copyFrom image");
        visit2(source2);
      }
    }
    visiting.delete(image.key);
  };
  visit2(root);
  const specs = [...images.values()].sort((left, right) => compareUTF82(left.key, right.key)).map((image) => ({
    key: image.key,
    platform: {
      os: "linux",
      architecture: options.architecture
    },
    steps: image.steps.map((step) => compileImageStep(step, options))
  }));
  const stepCount = specs.reduce((total, image) => total + image.steps.length, 0);
  if (stepCount > 1e4)
    throw new Error("image build exceeds 10000 steps");
  return {
    root: root.key,
    images: specs
  };
}
function compileImageStep(step, options) {
  switch (step.kind) {
    case "from":
      assertExactKeys(step, ["kind", "ref"], "image from step");
      return { from: { ref: step.ref } };
    case "run":
      assertExactKeys(step, ["argv", "kind"], "image run step");
      return {
        run: {
          argv: [...step.argv]
        }
      };
    case "copy_source_file":
      assertExactKeys(step, ["destination", "kind", "source"], "image source-file copy step");
      return {
        copySourceFile: {
          dst: step.destination,
          path: step.source.path
        }
      };
    case "copy_source_directory":
      assertExactKeys(step, ["destination", "kind", "source"], "image source-directory copy step");
      return {
        copySourceDir: {
          dst: step.destination,
          path: step.source.path
        }
      };
    case "copy_from_image": {
      assertExactKeys(step, ["destination", "kind", "source", "sourcePath"], "image cross-image copy step");
      const source2 = inspectImage(step.source);
      if (source2 === undefined)
        throw new Error("invalid copyFrom image");
      return {
        copyFromImage: {
          dst: step.destination,
          imageKey: source2.key,
          srcPath: step.sourcePath
        }
      };
    }
    case "workdir":
      assertExactKeys(step, ["kind", "path"], "image workdir step");
      return { workdir: { path: step.path } };
    case "env":
      assertExactKeys(step, ["key", "kind", "value"], "image env step");
      return { env: { key: step.key, value: step.value } };
    case "user":
      assertExactKeys(step, ["kind", "name"], "image user step");
      return { user: { name: step.name } };
  }
}
function assertExactKeys(value, expected, label) {
  const actual = Object.keys(value).sort(compareUTF82);
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    throw new Error(`${label} has unknown members`);
  }
}
function locatorEntry(item) {
  if (item.definition.kind === "sandbox") {
    throw new Error("Sandbox has no executable locator");
  }
  return {
    declaredId: item.definition.id,
    exportName: item.exportName,
    kind: item.definition.kind,
    modulePath: item.modulePath,
    slot: "handler"
  };
}
function programDeclaration(definition) {
  switch (definition.kind) {
    case "task":
      return {
        kind: "task",
        declaredId: definition.id,
        slots: definition.hasPayload ? ["handler", "payloadSchema"] : ["handler"]
      };
    case "actor":
      return {
        kind: "actor",
        declaredId: definition.id,
        slots: ["handler"]
      };
  }
}
function normalizeCpu(cpu) {
  if (!Number.isFinite(cpu) || cpu <= 0) {
    throw new Error("workspace cpu must be a finite positive number");
  }
  const text = cpu.toString();
  const match = /^(\d+)(?:\.(\d+))?(?:e([+-]?\d+))?$/i.exec(text);
  if (match === null)
    throw new Error("workspace cpu cannot be normalized");
  const integer = match[1];
  const fraction = match[2] ?? "";
  const exponent = Number(match[3] ?? "0");
  const significand = BigInt(`${integer}${fraction}`);
  const scale = exponent - fraction.length + 3;
  let milliCpu;
  if (scale >= 0) {
    milliCpu = significand * 10n ** BigInt(scale);
  } else {
    const divisor = 10n ** BigInt(-scale);
    if (significand % divisor !== 0n) {
      throw new Error("workspace cpu must resolve to whole milliCPU");
    }
    milliCpu = significand / divisor;
  }
  return safePositiveNumber(milliCpu, "workspace milliCPU");
}
function normalizeIecMiB(value, label) {
  const match = /^([1-9]\d*)(MiB|GiB)$/.exec(value);
  if (match === null) {
    throw new Error(`workspace ${label} must be a positive canonical integer suffixed by MiB or GiB`);
  }
  const result = BigInt(match[1]) * (match[2] === "GiB" ? 1024n : 1n);
  return safePositiveNumber(result, `workspace ${label} MiB`);
}
function normalizeDuration(value, label, minimumMs, maximumMs) {
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(value);
  if (match === null) {
    throw new Error(`${label} must match ^[1-9][0-9]*(ms|s|m|h|d)$`);
  }
  const multipliers = {
    ms: 1n,
    s: 1000n,
    m: 60000n,
    h: 3600000n,
    d: 86400000n
  };
  const milliseconds = BigInt(match[1]) * multipliers[match[2]];
  if (milliseconds < BigInt(minimumMs) || milliseconds > BigInt(maximumMs)) {
    throw new Error(`${label} must resolve to milliseconds in [${minimumMs},${maximumMs}]`);
  }
  return Number(milliseconds);
}
function safePositiveNumber(value, label) {
  if (value <= 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`${label} must be a positive safe integer`);
  }
  return Number(value);
}
function validateModulePath(path) {
  const suffixes = [
    ".cjs",
    ".cts",
    ".js",
    ".jsx",
    ".mjs",
    ".mts",
    ".ts",
    ".tsx"
  ];
  const components = path.split("/");
  if (path.length === 0 || !hasOnlyUnicodeScalarValues2(path) || path.startsWith("/") || path.includes("\\") || /[\p{Cc}]/u.test(path) || components.some((component) => component === "" || component === "." || component === "..") || components.includes("node_modules") || components[0] === "helmr" || path.endsWith(".d.ts") || path.endsWith(".d.mts") || path.endsWith(".d.cts") || !suffixes.some((suffix) => path.endsWith(suffix))) {
    throw new Error(`modulePath ${JSON.stringify(path)} is not an admitted first-party module path`);
  }
}
function validateExportName(name) {
  const length = new TextEncoder().encode(name).length;
  if (length < 1 || length > 256 || !hasOnlyUnicodeScalarValues2(name) || /[\p{Cc}]/u.test(name)) {
    throw new Error(`exportName ${JSON.stringify(name)} is invalid`);
  }
}
function compareLocatedDefinitions(left, right) {
  const order = {
    task: 0,
    actor: 1,
    sandbox: 2
  };
  return order[left.definition.kind] - order[right.definition.kind] || compareUTF82(left.definition.id, right.definition.id);
}
function compareLocatorOccurrence(left, right) {
  return compareUTF82(left.modulePath, right.modulePath) || compareUTF82(left.exportName, right.exportName);
}

// compiler/typescript/src/compile-selection.ts
import { lstat as lstat2, readFile, realpath as realpath2, stat } from "node:fs/promises";
import { relative as relative2, resolve as resolve2, sep as sep2 } from "node:path";
function installedPackageRoot(path) {
  const parts = path.split("/");
  const index = parts.lastIndexOf("node_modules");
  if (index < 0 || !parts[index + 1])
    return;
  const count = parts[index + 1].startsWith("@") ? 3 : 2;
  if (parts.length < index + count)
    return;
  return parts.slice(0, index + count).join("/");
}
function selectedPackage(path, roots) {
  const owner = installedPackageRoot(path);
  return owner !== undefined && roots.has(owner);
}
async function resolveCompilePackages(root, selectors) {
  root = await realpath2(root);
  if (new Set(selectors).size !== selectors.length) {
    throw new Error("helmr.config.ts compilePackages contains duplicate selectors");
  }
  const packages = [];
  for (const logicalRoot of [...selectors].sort(compareUTF82)) {
    try {
      if (!validPath(logicalRoot) || installedPackageRoot(logicalRoot) !== logicalRoot) {
        throw new Error("expected a clean project-relative installed package root, e.g. node_modules/@scope/package");
      }
      const target = await realpath2(resolve2(root, logicalRoot));
      const resolvedRoot = relative2(root, target).split(sep2).join("/");
      if (!validPath(resolvedRoot))
        throw new Error("resolved root escapes project or uses a reserved path");
      if (!(await stat(target)).isDirectory())
        throw new Error("selected root is not a directory");
      if (resolvedRoot.split("/").includes("node_modules") && installedPackageRoot(resolvedRoot) !== resolvedRoot) {
        throw new Error("resolved root is not an installed package root");
      }
      parseManifest(await regularManifest(resolve2(target, "package.json")));
      packages.push({ logicalRoot, resolvedRoot });
    } catch (error) {
      throw new Error(`helmr.config.ts compilePackages selector ${JSON.stringify(logicalRoot)}: ${error instanceof Error ? error.message : String(error)}`);
    }
  }
  return packages;
}
function validPath(path) {
  const parts = path.split("/");
  return path.isWellFormed() && !/[\\\p{Cc}]/u.test(path) && path !== "helmr" && !path.startsWith("helmr/") && parts.length <= 128 && Buffer.byteLength(`/opt/helmr/program/${path}\x00`) <= 4096 && parts.every((part) => part !== "" && part !== "." && part !== ".." && part !== ".helmr" && Buffer.byteLength(part) <= 255);
}
async function regularManifest(path) {
  const metadata = await lstat2(path);
  if (!metadata.isFile())
    throw new Error(`package manifest must be a regular file: ${path}`);
  if (metadata.size > 16 << 20)
    throw new Error(`package manifest exceeds 16 MiB: ${path}`);
  return readFile(path);
}
function parseManifest(raw) {
  const text = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true }).decode(raw);
  const errors = [];
  const tree = parseTree2(text, errors, { allowTrailingComma: false, disallowComments: true });
  if (errors.length || tree?.type !== "object")
    throw new Error("package.json must be a strict JSON object");
  function visit2(node) {
    if (node.type === "number" && !Number.isFinite(node.value) || node.type === "string" && !node.value.isWellFormed()) {
      throw new Error("package.json contains a non-finite number or invalid Unicode string");
    }
    if (node.type === "object") {
      const names = new Set;
      for (const property of node.children ?? []) {
        const name = property.children[0].value;
        if (names.has(name))
          throw new Error(`package.json contains duplicate object key ${JSON.stringify(name)}`);
        names.add(name);
      }
    }
    for (const child of node.children ?? [])
      visit2(child);
  }
  visit2(tree);
  return JSON.parse(text);
}

// compiler/typescript/src/config-origin.ts
import { build } from "esbuild";

// compiler/typescript/src/bundle.ts
var COMPILER_API_VERSION = "helmr.compiler.v0";
var ESBUILD_VERSION = "0.28.2";
var RUNTIME_PROGRAM_ROOT = "/opt/helmr/program";
if (esbuildVersion !== ESBUILD_VERSION) {
  throw new Error(`esbuild version ${JSON.stringify(esbuildVersion)} does not match ${ESBUILD_VERSION}`);
}
var declarationExtensions = [
  ".cjs",
  ".cts",
  ".js",
  ".jsx",
  ".mjs",
  ".mts",
  ".ts",
  ".tsx"
];
function compilerContract() {
  return {
    apiVersion: COMPILER_API_VERSION,
    esbuildVersion: ESBUILD_VERSION,
    optionsContractDigest: compilerOptionsContractDigest(),
    output: {
      aggregate: "analysis-only",
      finalModules: "independent",
      sharedChunks: false,
      sourceMaps: "external"
    },
    source: {
      declarationExtensions,
      packageDependencies: "external",
      semantics: "pinned-esbuild",
      projectSources: "bundled",
      compilePackages: "explicit-installed-roots"
    }
  };
}
async function compileProgram(options) {
  const root = await realpath3(options.root);
  const outputRoot = resolve3(options.outputRoot);
  const compilePackages = await resolveCompilePackages(root, options.config.compilePackages);
  const selectedRoots = new Set(compilePackages.map((item) => item.resolvedRoot));
  const modules = await discoverModules(root, options.config);
  if (modules.length === 0) {
    throw new Error("configured dirs contain no declaration source modules");
  }
  const analysisRoot = resolve3(outputRoot, "analysis");
  await mkdir(analysisRoot, { recursive: false });
  try {
    const aggregatePath = resolve3(analysisRoot, "aggregate.mjs");
    const aggregate = await bundleEntry({
      root,
      entrySource: aggregateEntry(modules),
      sourcefile: "<helmr-analysis>",
      outfile: resolve3(root, "helmr/analysis.mjs"),
      nodeVersion: options.nodeVersion,
      runtimeRoot: root,
      selectedRoots
    });
    await writeFile(aggregatePath, aggregate.code);
    const namespaces = await importAggregate(aggregatePath);
    const analyzed = analyze({
      architecture: options.architecture,
      exports: analysisExports(namespaces)
    });
    const declarationsBySource = new Map;
    for (const declaration of analyzed.declarationLocator.declarations) {
      const exports = declarationsBySource.get(declaration.modulePath) ?? [];
      exports.push(declaration.exportName);
      declarationsBySource.set(declaration.modulePath, exports);
    }
    const canonicalSourceGroups = [...declarationsBySource].sort(([left], [right]) => compareUTF82(left, right));
    const generated = new Map;
    const files = new Map;
    const finalOutputs = [];
    const metafiles = [aggregate.metafile];
    const runtimeRoot = resolve3(options.runtimeRoot ?? RUNTIME_PROGRAM_ROOT);
    const finalBundle = await bundleEntries({
      root,
      outputRoot: analysisRoot,
      entries: canonicalSourceGroups.map(([source2, exportNames]) => ({
        source: source2,
        entrySource: finalEntry(source2, exportNames),
        sourcefile: `<helmr-final-${source2}>`,
        outfile: resolve3(analysisRoot, generatedModulePath(source2))
      })),
      nodeVersion: options.nodeVersion,
      runtimeRoot,
      selectedRoots
    });
    metafiles.push(finalBundle.metafile);
    for (const [source2, compiled] of finalBundle.outputs) {
      const path = generatedModulePath(source2);
      const sourceMap = normalizeSourceMap(root, resolve3(analysisRoot, path), source2, compiled.map, selectedRoots);
      const analysisModulePath = resolve3(analysisRoot, path);
      await writeFile(`${analysisModulePath}.map`, sourceMap);
      await verifyFinalModule(analysisModulePath, source2, analyzed, options.architecture);
      generated.set(source2, path);
      files.set(path, compiled.code);
      files.set(`${path}.map`, sourceMap);
      finalOutputs.push({
        moduleDigest: `sha256:${sha256(compiled.code)}`,
        modulePath: path,
        sourceMapDigest: `sha256:${sha256(sourceMap)}`,
        sourceMapPath: `${path}.map`,
        sourcePath: source2
      });
    }
    finalOutputs.sort((left, right) => compareUTF82(left.modulePath, right.modulePath));
    const locator = {
      declarations: analyzed.declarationLocator.declarations.map((item) => ({
        ...item,
        modulePath: requiredGeneratedPath(generated, item.modulePath)
      })),
      formatVersion: analyzed.declarationLocator.formatVersion
    };
    const locatorBytes = canonicalizeJsonValue(locator);
    const analysis = Object.freeze({
      ...analyzed,
      declarationLocator: Object.freeze({
        ...locator,
        declarations: Object.freeze(locator.declarations)
      }),
      declarationLocatorBytes: locatorBytes
    });
    const configBytes = canonicalizeJsonValue(options.config);
    files.set("helmr/config.json", configBytes);
    const externalEdges = finalBundle.externalEdges;
    const inputs = await compilerInputs(root, metafiles, selectedRoots, new Set(["<helmr-analysis>", ...finalBundle.virtualInputs]));
    const tsconfigs = await compilerTSConfigs(root, inputs.map((item) => item.path));
    files.set("helmr/compiler-result.json", canonicalizeJsonValue({
      compiler: compilerContract(),
      config: {
        digest: `sha256:${sha256(configBytes)}`,
        path: "helmr/config.json"
      },
      execution: {
        nodeVersion: options.nodeVersion,
        optionsDigest: compilerOptionsDigest(options.nodeVersion)
      },
      discoveryCandidates: modules,
      externalEdges,
      inputs,
      compilePackages,
      outputs: finalOutputs,
      selections: analyzed.declarationLocator.declarations.map((item) => ({
        declaredId: item.declaredId,
        exportName: item.exportName,
        kind: item.kind,
        slot: item.slot,
        sourcePath: item.modulePath
      })),
      tsconfigs
    }));
    return Object.freeze({
      analysis,
      files,
      modules: Object.freeze(modules),
      optionsDigest: compilerOptionsDigest(options.nodeVersion)
    });
  } finally {
    await rm(analysisRoot, { force: true, recursive: true });
  }
}
function compilerOptionsContractDigest() {
  return compilerOptionsDigestForTarget("exact-managed-node");
}
function compilerOptionsDigest(nodeVersion) {
  return compilerOptionsDigestForTarget(esbuildNodeTarget(nodeVersion));
}
function compilerOptionsDigestForTarget(target) {
  const canonical = canonicalizeJsonValue({
    apiVersion: COMPILER_API_VERSION,
    banner: 'import { createRequire as __helmrCreateRequire } from "node:module"; const require = __helmrCreateRequire(import.meta.url);',
    bundle: true,
    esbuildVersion: ESBUILD_VERSION,
    format: "esm",
    finalEntryGraph: "multi-entry",
    legalComments: "none",
    metafile: true,
    packages: "bundle",
    platform: "node",
    preserveSymlinks: false,
    sourceMap: "external",
    sourceMapSources: "absolute-program-urls",
    sourcesContent: false,
    splitting: false,
    treeShaking: true,
    declarationExtensions,
    sourceSemantics: "pinned-esbuild",
    dependencyBoundary: "explicit-installed-roots",
    rootConfig: "build-only",
    target
  });
  return `sha256:${sha256(canonical)}`;
}
async function bundleEntry(options) {
  const externalEdges = [];
  const result = await build2({
    ...baseOptions(options.root, options.runtimeRoot, options.nodeVersion, options.selectedRoots, externalEdges),
    outfile: options.outfile,
    stdin: {
      contents: options.entrySource,
      loader: "js",
      resolveDir: options.root,
      sourcefile: options.sourcefile
    }
  });
  const output = {
    ...singleOutputFiles(requireOutputFiles(result.outputFiles), options.outfile),
    externalEdges: sortedExternalEdges(externalEdges),
    metafile: requiredMetafile(result.metafile)
  };
  return output;
}
async function bundleEntries(options) {
  const externalEdges = [];
  const entries = new Map(options.entries.map((entry) => [entry.sourcefile, entry]));
  const result = await build2({
    ...baseOptions(options.root, options.runtimeRoot, options.nodeVersion, options.selectedRoots, externalEdges, [finalEntryPlugin(options.root, entries)]),
    entryPoints: options.entries.map((entry) => ({
      in: entry.sourcefile,
      out: projectPath2(options.outputRoot, entry.outfile).slice(0, -4)
    })),
    outdir: options.outputRoot,
    outExtension: { ".js": ".mjs" },
    write: true
  });
  const metafile = requiredMetafile(result.metafile);
  const outputPaths = new Set(Object.keys(metafile.outputs).map((path) => resolve3(options.root, path)));
  if (outputPaths.size !== options.entries.length * 2) {
    throw new Error("esbuild output topology does not match the v0 contract");
  }
  const externalEdgesByImporter = new Map;
  for (const edge of externalEdges) {
    const importerEdges = externalEdgesByImporter.get(edge.importer) ?? [];
    importerEdges.push(edge);
    externalEdgesByImporter.set(edge.importer, importerEdges);
  }
  const outputs = new Map;
  const externalEdgeGroups = [];
  for (const entry of options.entries) {
    if (!outputPaths.has(entry.outfile) || !outputPaths.has(`${entry.outfile}.map`)) {
      throw new Error("esbuild output topology does not match the v0 contract");
    }
    const output = requiredMetafileOutput(options.root, metafile, entry.outfile);
    const inputPaths = outputInputPaths(options.root, output.inputs);
    externalEdgeGroups.push(externalEdgesForOutput(output, inputPaths, externalEdgesByImporter));
    outputs.set(entry.source, {
      code: await readFile2(entry.outfile),
      map: await readFile2(`${entry.outfile}.map`)
    });
  }
  return {
    externalEdges: mergeExternalEdges(externalEdgeGroups),
    metafile,
    outputs,
    virtualInputs: new Set(options.entries.map((entry) => `helmr-final:${entry.sourcefile}`))
  };
}
function baseOptions(root, runtimeRoot, nodeVersion, selectedRoots, externalEdges, plugins = [], phase = "program") {
  return {
    absWorkingDir: root,
    bundle: true,
    format: "esm",
    legalComments: "none",
    logLevel: "silent",
    metafile: true,
    packages: "bundle",
    platform: "node",
    plugins: [...plugins, dependencyBoundary(root, runtimeRoot, selectedRoots, externalEdges, phase)],
    banner: {
      js: 'import { createRequire as __helmrCreateRequire } from "node:module"; const require = __helmrCreateRequire(import.meta.url);'
    },
    sourcesContent: false,
    sourcemap: "external",
    splitting: false,
    target: esbuildNodeTarget(nodeVersion),
    treeShaking: true,
    write: false
  };
}
async function verifyFinalModule(path, source2, aggregate, architecture) {
  const namespace = await importModuleNamespace(path);
  const expected = aggregate.declarationLocator.declarations.filter((item) => item.modulePath === source2);
  const exports = expected.map((item) => {
    if (!Object.prototype.hasOwnProperty.call(namespace, item.exportName)) {
      throw new Error(`final module ${JSON.stringify(source2)} is missing export ${JSON.stringify(item.exportName)}`);
    }
    return {
      exportName: item.exportName,
      modulePath: source2,
      value: namespace[item.exportName]
    };
  });
  const verified = analyzeProgramExports({ architecture, exports });
  const actual = canonicalizeJsonValue({
    declarations: verified.declarationLocator.declarations,
    programDeclarations: verified.programDeclarations
  });
  const wanted = canonicalizeJsonValue({
    declarations: expected,
    programDeclarations: aggregate.programDeclarations.filter((item) => expected.some((located) => located.kind === item.kind && located.declaredId === item.declaredId))
  });
  if (!Buffer.from(actual).equals(Buffer.from(wanted))) {
    throw new Error(`final module ${JSON.stringify(source2)} does not match aggregate analysis`);
  }
}
async function importModuleNamespace(path) {
  const value = await import(`${pathToFileURL(path).href}?digest=${sha256(await readFile2(path))}`);
  if (typeof value !== "object" || value === null) {
    throw new Error("generated final module has no ESM namespace");
  }
  return value;
}
function esbuildNodeTarget(nodeVersion) {
  if (!/^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$/.test(nodeVersion)) {
    throw new Error("Compiler Node version must be an exact canonical SemVer");
  }
  return `node${nodeVersion}`;
}
function finalEntryPlugin(root, entries) {
  return {
    name: "helmr-final-entry",
    setup(build3) {
      build3.onResolve({ filter: /^<helmr-final-/ }, (args) => {
        return { namespace: "helmr-final", path: args.path };
      });
      build3.onLoad({ filter: /.*/, namespace: "helmr-final" }, (args) => {
        const entry = entries.get(args.path);
        if (entry === undefined) {
          return { errors: [{ text: `unknown final entry ${args.path}` }] };
        }
        return {
          contents: entry.entrySource,
          loader: "js",
          resolveDir: root
        };
      });
    }
  };
}
function requiredMetafileOutput(root, metafile, outfile) {
  const matches = Object.entries(metafile.outputs).filter(([path]) => resolve3(root, path) === outfile);
  if (matches.length !== 1) {
    throw new Error("esbuild metafile output does not match the v0 topology");
  }
  return matches[0][1];
}
function outputInputPaths(root, inputs) {
  return new Set(Object.entries(inputs).filter(([, input]) => input.bytesInOutput > 0).map(([path]) => projectPath2(root, resolve3(root, path))));
}
function externalEdgesForOutput(output, inputs, externalEdgesByImporter) {
  const emitted = new Set(output.imports.filter((item) => item.external).map((item) => `${item.kind}\x00${item.path}`));
  const externalEdges = [];
  for (const input of inputs) {
    for (const edge of externalEdgesByImporter.get(input) ?? []) {
      if (emitted.has(`${edge.kind}\x00${edge.runtimePath}`)) {
        externalEdges.push(edge);
      }
    }
  }
  return externalEdges;
}
function dependencyBoundary(root, runtimeRoot, selectedRoots, externalEdges, phase) {
  const canonicalRoot = resolve3(root);
  return {
    name: "helmr-dependency-boundary",
    async setup(build3) {
      const rootConfig = phase === "program" ? await realpath3(resolve3(canonicalRoot, "helmr.config.ts")).catch((error) => {
        if (error.code === "ENOENT")
          return;
        throw error;
      }) : undefined;
      build3.onResolve({ filter: /.*/ }, async (args) => {
        if (args.pluginData === resolvedByBoundary || args.path.startsWith("node:")) {
          return;
        }
        const result = await build3.resolve(args.path, {
          importer: args.importer,
          kind: args.kind,
          namespace: args.namespace,
          pluginData: resolvedByBoundary,
          resolveDir: args.resolveDir,
          with: args.with
        });
        if (result.errors.length !== 0 || result.external)
          return result;
        if (result.path === "")
          return result;
        const logicalPath = projectPath2(canonicalRoot, resolve3(result.path));
        const path = await realpath3(result.path);
        const resolvedPath = projectPath2(canonicalRoot, path);
        if (!inside2(relative3(canonicalRoot, path))) {
          return {
            errors: [{
              text: `resolved path escapes submitted source: ${args.path}`
            }]
          };
        }
        if (phase === "program" && path === rootConfig) {
          return { errors: [{ text: "helmr.config.ts is build-only and cannot be imported by Program source; move shared data or helpers to an ordinary module" }] };
        }
        if (phase === "config" && /\.(?:[cm]?ts|tsx|jsx)$/.test(path)) {
          return;
        }
        if (phase === "program" && selectedPackage(resolvedPath, selectedRoots)) {
          return;
        }
        if (hasNodeModules(resolvedPath)) {
          const importer = args.importer === "" ? args.importer : projectPath2(canonicalRoot, resolve3(args.importer));
          if (/\.(?:ts|tsx|mts|cts)$/.test(resolvedPath)) {
            return { errors: [{
              text: `Cannot externalize TypeScript dependency ${JSON.stringify(args.path)} imported by ${JSON.stringify(importer)}: ${resolvedPath}. Node cannot execute TypeScript under node_modules. Add ${JSON.stringify(installedPackageRoot(resolvedPath))} to helmr.config.ts compilePackages to compile this installed package, or install Node-ready JavaScript.`
            }] };
          }
          const runtimePath = resolve3(runtimeRoot, logicalPath);
          externalEdges.push({
            importer,
            kind: args.kind,
            logicalPath,
            resolvedPath,
            runtimePath,
            specifier: args.path
          });
          const target = resolve3(runtimeRoot, logicalPath);
          return {
            external: true,
            path: target
          };
        }
        return;
      });
    }
  };
}
var resolvedByBoundary = Object.freeze({});
function sortedExternalEdges(edges) {
  const unique = new Map(edges.map((edge) => [externalEdgeKey(edge), edge]));
  return [...unique.values()].sort((left, right) => compareUTF82(externalEdgeKey(left), externalEdgeKey(right)));
}
function mergeExternalEdges(groups) {
  return sortedExternalEdges(groups.flat());
}
function externalEdgeKey(edge) {
  return [
    edge.importer,
    edge.specifier,
    edge.kind,
    edge.logicalPath,
    edge.resolvedPath,
    edge.runtimePath
  ].join("\x00");
}
function aggregateEntry(modules) {
  const imports = modules.map((path, index) => `import * as module${index} from ${JSON.stringify(`./${path}`)};`);
  const entries = modules.map((path, index) => `{ modulePath: ${JSON.stringify(path)}, namespace: module${index} }`);
  return `${imports.join(`
`)}
export default [${entries.join(",")}];
`;
}
function finalEntry(source2, exportNames) {
  const bindings = exportNames.map((name, index) => `const helmrExport${index} = source[${JSON.stringify(name)}];`);
  const exports = exportNames.map((name, index) => `helmrExport${index} as ${JSON.stringify(name)}`);
  return [
    `import * as source from ${JSON.stringify(`./${source2}`)};`,
    ...bindings,
    `export { ${exports.join(", ")} };`,
    ""
  ].join(`
`);
}
function generatedModulePath(source2) {
  const directory = dirname(source2);
  const prefix = directory === "." ? "" : `${directory}/`;
  return `${prefix}.helmr/modules/${sha256(source2)}.mjs`;
}
async function importAggregate(path) {
  const value = await import(`${pathToFileURL(path).href}?digest=${sha256(await readFile2(path))}`);
  if (typeof value !== "object" || value === null || !Array.isArray(value["default"])) {
    throw new Error("analysis bundle did not export module namespaces");
  }
  return value.default;
}
function analysisExports(modules) {
  return modules.flatMap(({ modulePath, namespace }) => Object.getOwnPropertyNames(namespace).sort(compareUTF82).map((exportName) => ({
    exportName,
    modulePath,
    value: namespace[exportName]
  })));
}
function normalizeSourceMap(root, outfile, source2, raw, selectedRoots) {
  const value = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(raw));
  if (typeof value !== "object" || value === null) {
    throw new Error("esbuild source map root is not an object");
  }
  const map = value;
  if (map["version"] !== 3 || !Array.isArray(map["sources"]) || !map["sources"].every((item) => typeof item === "string") || typeof map["mappings"] !== "string" || !Array.isArray(map["names"])) {
    throw new Error("esbuild source map does not match the v0 topology");
  }
  const sources = map["sources"].map((item) => {
    const decoded = decodeURIComponent(item);
    if (decoded === `helmr-final:<helmr-final-${source2}>`) {
      return programSourceURL(source2);
    }
    const absolute = resolve3(dirname(outfile), decoded);
    const path = projectPath2(root, absolute);
    if (!inside2(relative3(root, absolute)) || hasNodeModules(path) && !selectedPackage(path, selectedRoots)) {
      throw new Error(`source map source escapes first-party Program files: ${item}`);
    }
    return programSourceURL(path);
  });
  return canonicalizeJsonValue({
    mappings: map["mappings"],
    names: map["names"],
    sources,
    version: 3
  });
}
function programSourceURL(path) {
  return pathToFileURL(resolve3(RUNTIME_PROGRAM_ROOT, path)).href;
}
function singleOutputFiles(files, outfile) {
  const code = files.find((file) => file.path === outfile);
  const map = files.find((file) => file.path === `${outfile}.map`);
  if (code === undefined || map === undefined || files.length !== 2) {
    throw new Error("esbuild output topology does not match the v0 contract");
  }
  return { code: code.contents, map: map.contents };
}
function requireOutputFiles(files) {
  if (files === undefined)
    throw new Error("esbuild returned no output files");
  return files;
}
function requiredMetafile(metafile) {
  if (metafile === undefined)
    throw new Error("esbuild returned no metafile");
  return metafile;
}
async function compilerInputs(root, metafiles, selectedRoots, virtualInputs) {
  const paths = new Set;
  for (const metafile of metafiles) {
    for (const input of Object.keys(metafile.inputs)) {
      if (virtualInputs.has(input))
        continue;
      const absolute = await realpath3(resolve3(root, input));
      const path = projectPath2(root, absolute);
      if (!inside2(relative3(root, absolute)))
        throw new Error(`compiler input escapes project: ${input}`);
      if (hasNodeModules(path) && !selectedPackage(path, selectedRoots)) {
        throw new Error(`compiler input is not in a selected package: ${path}`);
      }
      paths.add(path);
    }
  }
  const sorted = [...paths].sort(compareUTF82);
  return Promise.all(sorted.map(async (path) => ({
    digest: `sha256:${sha256(await readFile2(resolve3(root, path)))}`,
    path
  })));
}
async function compilerTSConfigs(root, inputs) {
  const roots = new Set;
  for (const input of inputs) {
    let directory = dirname(resolve3(root, input));
    for (;; ) {
      const candidate = resolve3(directory, "tsconfig.json");
      try {
        await readFile2(candidate);
        roots.add(candidate);
        break;
      } catch (error) {
        if (error.code !== "ENOENT")
          throw error;
      }
      if (directory === root)
        break;
      const parent = dirname(directory);
      if (!inside2(relative3(root, parent)))
        break;
      directory = parent;
    }
  }
  const configs = new Map;
  const pending = [...roots];
  while (pending.length !== 0) {
    const candidate = await realpath3(pending.shift());
    const path = projectPath2(root, candidate);
    if (!inside2(path)) {
      throw new Error(`tsconfig path escapes project: ${candidate}`);
    }
    if (configs.has(path))
      continue;
    const contents = await readFile2(candidate);
    configs.set(path, `sha256:${sha256(contents)}`);
    const errors = [];
    const document = parse2(contents.toString("utf8"), errors, {
      allowTrailingComma: true,
      disallowComments: false
    });
    if (errors.length !== 0 || typeof document !== "object" || document === null || Array.isArray(document)) {
      throw new Error(`tsconfig ${JSON.stringify(path)} is not valid JSONC`);
    }
    const extended = document["extends"];
    const values = typeof extended === "string" ? [extended] : Array.isArray(extended) && extended.every((value) => typeof value === "string") ? extended : extended === undefined ? [] : (() => {
      throw new Error(`tsconfig ${JSON.stringify(path)} has invalid extends`);
    })();
    for (const specifier of values) {
      pending.push(await resolveTSConfigExtends(candidate, specifier));
    }
  }
  return [...configs].map(([path, digest]) => ({ digest, path })).sort((left, right) => compareUTF82(left.path, right.path));
}
async function resolveTSConfigExtends(configPath, specifier) {
  const directory = dirname(configPath);
  if (specifier.startsWith(".") || specifier.startsWith("/") || /^[A-Za-z]:[\\/]/.test(specifier)) {
    return requiredConfigPath(resolve3(directory, specifier));
  }
  const require2 = createRequire(pathToFileURL(configPath));
  for (const candidate of [specifier, `${specifier}/tsconfig.json`]) {
    try {
      return require2.resolve(candidate);
    } catch (error) {
      if (error.code !== "MODULE_NOT_FOUND") {
        throw error;
      }
    }
  }
  throw new Error(`tsconfig ${JSON.stringify(configPath)} cannot resolve extends ${JSON.stringify(specifier)}`);
}
async function requiredConfigPath(candidate) {
  for (const path of [
    candidate,
    `${candidate}.json`,
    resolve3(candidate, "tsconfig.json")
  ]) {
    try {
      if ((await stat2(path)).isFile())
        return path;
    } catch (error) {
      if (error.code !== "ENOENT")
        throw error;
    }
  }
  throw new Error(`extended tsconfig ${JSON.stringify(candidate)} does not exist`);
}
function projectPath2(root, value) {
  return relative3(root, value).split(sep3).join("/");
}
function requiredGeneratedPath(generated, source2) {
  const path = generated.get(source2);
  if (path === undefined)
    throw new Error(`missing final module for ${source2}`);
  return path;
}
function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}
function hasNodeModules(path) {
  return path.split(sep3).includes("node_modules");
}
function inside2(path) {
  return path === "" || path !== ".." && !path.startsWith(`..${sep3}`) && !path.startsWith("/");
}

// compiler/typescript/src/config.ts
function inspectCanonicalConfig(value) {
  if (typeof value !== "object" || value === null || Array.isArray(value) || Object.getPrototypeOf(value) !== Object.prototype) {
    throw new Error("canonical config must be an ordinary object");
  }
  const record = value;
  const keys = Object.keys(record).sort();
  if (keys.length !== 3 || keys[0] !== "compilePackages" || keys[1] !== "dirs" || keys[2] !== "ignorePatterns") {
    throw new Error("canonical config does not match the build contract");
  }
  return inspectConfig({
    compilePackages: record["compilePackages"],
    dirs: record["dirs"],
    ignorePatterns: record["ignorePatterns"]
  });
}

// compiler/typescript/src/program-compiler.ts
async function main() {
  if (process.argv.length === 3 && process.argv[2] === "--describe") {
    process.stdout.write(canonicalizeJsonValue(compilerContract()));
    return;
  }
  if (process.argv.length !== 6 || process.argv[2] === undefined || process.argv[3] === undefined || process.argv[4] === undefined || process.argv[5] === undefined) {
    throw new Error("Program Compiler requires a Program root, canonical config path, exact Node version, and output root");
  }
  const root = resolve4(process.argv[2]);
  const config = inspectCanonicalConfig(JSON.parse(await readFile3(process.argv[3], "utf8")));
  const compiled = await compileProgram({
    architecture: "x86_64",
    config,
    nodeVersion: process.argv[4],
    outputRoot: process.argv[5],
    root
  });
  for (const [path, contents] of compiled.files) {
    const target = resolve4(process.argv[5], path);
    await mkdir2(dirname2(target), { recursive: true });
    await writeFile2(target, contents);
  }
  await writeResult(successfulVerificationResult(compiled.analysis));
}
async function writeResult(result) {
  const configured = process.env["HELMR_SUPERVISOR_FD"];
  const fd = configured === undefined ? 3 : Number(configured);
  if (!Number.isSafeInteger(fd) || fd < 3) {
    throw new Error("Program Compiler result descriptor is invalid");
  }
  const output = createWriteStream("", { fd, autoClose: false });
  const frame = encodeVerificationResultFrame(result);
  await new Promise((resolve5, reject) => {
    output.once("error", reject);
    output.end(frame, resolve5);
  });
}
try {
  await main();
} catch (error) {
  if (process.argv[2] === "--describe")
    throw error;
  const message = error instanceof Error ? error.message : String(error);
  await writeResult(failedVerificationResult(message));
}
