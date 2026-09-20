// sdk/typescript/src/builder.ts
var builderBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.builder");
var BuilderValue = class _BuilderValue {
  steps;
  constructor(steps = []) {
    this.steps = Object.freeze([...steps]);
    Object.defineProperty(this, builderBrand, { value: true });
    Object.freeze(this);
  }
  run(argv, ...unexpected) {
    if (unexpected.length !== 0) {
      throw new Error("builder.run() accepts only argv");
    }
    if (!Array.isArray(argv) || argv.some((argument) => typeof argument !== "string")) {
      throw new Error("builder.run() requires an argv array of strings");
    }
    return new _BuilderValue([
      ...this.steps,
      Object.freeze({ kind: "run", argv: Object.freeze([...argv]) })
    ]);
  }
  copy(source2, destination, ...unexpected) {
    if (unexpected.length !== 0) {
      throw new Error("builder.copy() accepts only a source and a destination");
    }
    if (typeof source2 !== "string") {
      throw new Error(
        "builder.copy() requires a project source path as its first argument; source.file() and source.directory() belong to image.copy()"
      );
    }
    if (typeof destination !== "string") {
      throw new Error("builder.copy() requires a destination path as its second argument");
    }
    if (/[*?\\]/.test(source2)) {
      throw new Error(
        "builder.copy() sources are literal project paths; '*', '?' and '\\' are not supported, copy the containing directory instead"
      );
    }
    return new _BuilderValue([
      ...this.steps,
      Object.freeze({ kind: "copy", source: source2, destination })
    ]);
  }
};
function builder(...unexpected) {
  if (unexpected.length !== 0) {
    throw new Error("builder() takes no arguments; it always starts from the Helmr builder image");
  }
  return new BuilderValue();
}
function isBuilder(value) {
  return typeof value === "object" && value !== null && value[builderBrand] === true;
}

// sdk/typescript/src/internal/utf8.ts
var encoder = new TextEncoder();
var encode = TextEncoder.prototype.encode.call.bind(
  TextEncoder.prototype.encode
);
var charCodeAt = String.prototype.charCodeAt.call.bind(
  String.prototype.charCodeAt
);
function compareUTF8(left, right) {
  const leftBytes = encode(encoder, left);
  const rightBytes = encode(encoder, right);
  const length = leftBytes.length < rightBytes.length ? leftBytes.length : rightBytes.length;
  for (let index = 0; index < length; index++) {
    const difference = leftBytes[index] - rightBytes[index];
    if (difference !== 0) return difference;
  }
  return leftBytes.length - rightBytes.length;
}
function hasOnlyUnicodeScalarValues(value) {
  for (let index = 0; index < value.length; index++) {
    const unit = charCodeAt(value, index);
    if (unit >= 55296 && unit <= 56319) {
      if (index + 1 === value.length) return false;
      const next = charCodeAt(value, index + 1);
      if (next < 56320 || next > 57343) return false;
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
var startsWith = String.prototype.startsWith.call.bind(
  String.prototype.startsWith
);
var endsWith = String.prototype.endsWith.call.bind(
  String.prototype.endsWith
);
var includes = String.prototype.includes.call.bind(
  String.prototype.includes
);
var split = String.prototype.split.call.bind(
  String.prototype.split
);
var slice = String.prototype.slice.call.bind(
  String.prototype.slice
);
var charCodeAt2 = String.prototype.charCodeAt.call.bind(
  String.prototype.charCodeAt
);
var regexpTest = RegExp.prototype.test.call.bind(
  RegExp.prototype.test
);
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
      for (let candidate = pathIndex; candidate <= pathSegments.length; candidate++) {
        if (matches(patternIndex + 1, candidate)) return true;
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
  for (let index = 0; index < segments.length; index++) {
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
  if (typeof value !== "string" || value === "" || !hasOnlyUnicodeScalarValues(value) || startsWith(value, "./") || startsWith(value, "/") || endsWith(value, "/") || includes(value, "//") || includes(value, "\\") || hasControl(value) || startsWith(value, "!") || regexpTest(/[[\]{}]/, value) || regexpTest(/[?*+@!]\(/, value)) {
    throw new Error(`unsupported ignorePattern ${JSON.stringify(value)}`);
  }
  const segments = split(value, "/");
  for (let index = 0; index < segments.length; index++) {
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
  while (patternCharacters[patternIndex] === "*") patternIndex++;
  return patternIndex === patternCharacters.length;
}
function hasControl(value) {
  for (let index = 0; index < value.length; index++) {
    const code = charCodeAt2(value, index);
    if (code <= 31 || code >= 127 && code <= 159) return true;
  }
  return false;
}
function codePoints(value) {
  const result = [];
  for (let index = 0; index < value.length; ) {
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
  let invalidKey = false;
  for (let index = 0; index < keys.length; index++) {
    const key = keys[index];
    if (typeof key !== "string" || key !== "dirs" && key !== "ignorePatterns" && key !== "build") {
      invalidKey = true;
      break;
    }
  }
  if (invalidKey) {
    throw new Error("config accepts only dirs, ignorePatterns and build");
  }
  for (let index = 0; index < keys.length; index++) {
    const key = keys[index];
    if (typeof key !== "string") {
      throw new Error("config accepts only dirs, ignorePatterns and build");
    }
    const descriptor = descriptors[key];
    if (descriptor === void 0 || !descriptor.enumerable || !hasOwn(descriptor, "value")) {
      throw new Error("config properties must be enumerable data properties");
    }
  }
  const dirs = normalizeStringSet(
    hasOwn(descriptors, "dirs") ? descriptors["dirs"]?.value : ["tasks"],
    "config dirs",
    validateDirectory,
    true
  );
  const ignorePatterns = normalizeStringSet(
    hasOwn(descriptors, "ignorePatterns") ? descriptors["ignorePatterns"]?.value : [],
    "config ignorePatterns",
    validateIgnorePattern,
    false
  );
  return freeze({
    dirs: freeze(dirs),
    ignorePatterns: freeze(ignorePatterns),
    build: normalizeBuild(
      hasOwn(descriptors, "build") ? descriptors["build"]?.value : void 0
    )
  });
}
function normalizeBuild(value) {
  const secretNamePattern = /^[A-Z_][A-Z0-9_]{0,127}$/;
  const maxInstallCommandBytes = 16 << 10;
  if (value === void 0) {
    return freeze({ builder: builder(), installCommand: void 0, secrets: freeze([]) });
  }
  if (typeof value !== "object" || value === null || arrayIsArray(value) || getPrototypeOf(value) !== objectPrototype) {
    throw new Error("config build must be an ordinary object");
  }
  const descriptors = getOwnPropertyDescriptors(value);
  const keys = ownKeys(value);
  for (let index = 0; index < keys.length; index++) {
    const key = keys[index];
    if (typeof key !== "string" || key !== "builder" && key !== "installCommand" && key !== "secrets") {
      throw new Error("config build accepts only builder, installCommand and secrets");
    }
    const descriptor = descriptors[key];
    if (descriptor === void 0 || !descriptor.enumerable || !hasOwn(descriptor, "value")) {
      throw new Error("config build properties must be enumerable data properties");
    }
  }
  const builderValue = descriptors["builder"]?.value;
  if (builderValue !== void 0 && !isBuilder(builderValue)) {
    throw new Error(
      "config build.builder must be created by builder(); image() describes a Workspace image, not the build environment"
    );
  }
  const installCommand = descriptors["installCommand"]?.value;
  if (installCommand !== void 0) {
    if (typeof installCommand !== "string" || !regexpTest(/\S/, installCommand) || installCommand.length > maxInstallCommandBytes || includes(installCommand, "\0")) {
      throw new Error("config build.installCommand must be a non-empty command string");
    }
  }
  const secrets = hasOwn(descriptors, "secrets") && descriptors["secrets"]?.value !== void 0 ? normalizeStringSet(
    descriptors["secrets"]?.value,
    "config build.secrets",
    (name) => {
      if (typeof name !== "string" || !regexpTest(secretNamePattern, name)) {
        throw new Error("config build.secrets entries must be environment variable names such as NPM_TOKEN");
      }
      return name;
    },
    false
  ) : [];
  return freeze({
    builder: builderValue === void 0 ? builder() : builderValue,
    installCommand,
    secrets: freeze(secrets)
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
  for (let index = 0; index < length; index++) {
    const key = `${index}`;
    if (keys[index] !== key) {
      throw new Error(`${name} must be a dense ordinary array`);
    }
    const descriptor = getOwnPropertyDescriptor(value, key);
    if (descriptor === void 0 || !descriptor.enumerable || !hasOwn(descriptor, "value")) {
      throw new Error(`${name} entries must be enumerable data properties`);
    }
    const current = normalize(descriptor.value);
    let insertion = normalized.length;
    while (insertion > 0 && compareUTF8(current, normalized[insertion - 1]) < 0) {
      setArrayIndex(
        normalized,
        insertion,
        normalized[insertion - 1]
      );
      insertion--;
    }
    setArrayIndex(normalized, insertion, current);
  }
  if (nonempty && length === 0) {
    throw new Error(`${name} must be non-empty`);
  }
  for (let index = 1; index < normalized.length; index++) {
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
function assertPayloadSchema(value, label = "payload") {
  if (value === void 0) {
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
var TaskIdError = class extends Error {
  name = "TaskIdError";
  value;
  constructor(value) {
    super(`task id must match ${TASK_ID_PATTERN}: ${JSON.stringify(value)}`);
    this.value = value;
  }
};
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
  for (let index = 1; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (!(isAsciiAlnum(code) || code === 46 || code === 95 || code === 45)) {
      return false;
    }
  }
  return true;
}
var TaskQueueNameError = class extends Error {
  name = "TaskQueueNameError";
  value;
  constructor(value) {
    super(`queue name must match ${QUEUE_NAME_PATTERN}: ${JSON.stringify(value)}`);
    this.value = value;
  }
};
var TaskQueueConcurrencyLimitError = class extends Error {
  name = "TaskQueueConcurrencyLimitError";
  value;
  constructor(value) {
    super("queue concurrencyLimit must be a positive integer");
    this.value = value;
  }
};
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
  for (let index = 1; index < value.length; index += 1) {
    const code = value.charCodeAt(index);
    if (!(isAsciiAlnum(code) || code === 46 || code === 95 || code === 45 || code === 47)) {
      return false;
    }
  }
  return true;
}
function validateOptionalQueueConcurrencyLimit(value) {
  if (value === void 0 || value === null) {
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
var runtimeOperationsSymbol = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.runtime_operations");
function currentRuntimeOperations() {
  const operations = globalThis[runtimeOperationsSymbol];
  if (operations === void 0) {
    throw new Error(
      "runtime operation is unavailable without the Helmr managed runtime"
    );
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
function sessionOperationOptions(request = {}) {
  return { idempotencyKey: request.idempotencyKey ?? crypto.randomUUID() };
}
function createRuntimeSessionRef(id) {
  const sessionId = resourceID(id, "Session ID");
  const turn = (id2) => {
    const turnId = resourceID(id2, "Turn ID");
    return Object.freeze({
      id: turnId,
      sessionId,
      async send(data, request, options) {
        const receipt = await currentRuntimeOperations().sessionTurnSend(
          sessionId,
          turnId,
          data,
          sessionOperationOptions(request),
          options?.signal
        );
        return Object.freeze({ id: receipt.messageId, status: receipt.status });
      },
      retrieve(options) {
        return currentRuntimeOperations().sessionTurnRetrieve(
          sessionId,
          turnId,
          options?.signal
        );
      },
      interrupt(request, options) {
        return currentRuntimeOperations().sessionTurnInterrupt(
          sessionId,
          turnId,
          sessionOperationOptions(request),
          options?.signal
        );
      }
    });
  };
  return Object.freeze({
    id: sessionId,
    turn,
    async send(data, request, options) {
      const receipt = await currentRuntimeOperations().sessionSend(
        sessionId,
        data,
        sessionOperationOptions(request),
        options?.signal
      );
      return receipt.kind === "enqueued" ? Object.freeze({ kind: receipt.kind, turn: turn(receipt.turnId) }) : Object.freeze({
        kind: receipt.kind,
        turn: turn(receipt.turnId),
        message: Object.freeze({
          id: receipt.messageId,
          status: "accepted"
        })
      });
    },
    async enqueue(data, request, options) {
      const receipt = await currentRuntimeOperations().sessionEnqueue(
        sessionId,
        data,
        sessionOperationOptions(request),
        options?.signal
      );
      return turn(receipt.turnId);
    },
    events: Object.freeze({
      list(query, options) {
        return currentRuntimeOperations().sessionEvents(
          sessionId,
          query,
          options?.signal
        );
      }
    }),
    retrieve(options) {
      return currentRuntimeOperations().sessionRetrieve(
        sessionId,
        options?.signal
      );
    },
    close(request, options) {
      return currentRuntimeOperations().sessionClose(
        sessionId,
        sessionOperationOptions(request),
        options?.signal
      );
    },
    cancel(request, options) {
      return currentRuntimeOperations().sessionCancel(
        sessionId,
        sessionOperationOptions(request),
        options?.signal
      );
    },
    resume(request, options) {
      return currentRuntimeOperations().sessionResume(
        sessionId,
        { ...request, ...sessionOperationOptions(request) },
        options?.signal
      );
    }
  });
}
var sessions = Object.freeze({ ref: createRuntimeSessionRef });

// sdk/typescript/src/definitions.ts
var privateDefinitionBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.definition");
var privateQueueBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.queue");
function inspectDefinition(value) {
  if (typeof value !== "object" && typeof value !== "function" || value === null) {
    return void 0;
  }
  if (!Object.hasOwn(value, privateDefinitionBrand)) return void 0;
  const definition = value[privateDefinitionBrand];
  if (!isInternalDefinition(definition)) {
    throw new Error("invalid private definition record");
  }
  return definition;
}
function isQueue(value) {
  if (typeof value !== "object" || value === null) return false;
  if (!Object.hasOwn(value, privateQueueBrand)) return false;
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
  if (typeof value !== "object" || value === null) return false;
  const definition = value;
  if (typeof definition.id !== "string") return false;
  validateTaskId(definition.id);
  switch (definition.kind) {
    case "task":
      if (typeof definition.handler !== "function" || typeof definition.hasPayload !== "boolean") {
        return false;
      }
      if (definition.hasPayload) {
        assertPayloadSchema(
          definition.payloadSchema,
          `task ${JSON.stringify(definition.id)} payload`
        );
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
var imageBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.image");
var sourceFileBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.source-file");
var sourceDirectoryBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.source-directory");
var SourceFileValue = class {
  path;
  constructor(path) {
    this.path = path;
    Object.defineProperty(this, sourceFileBrand, { value: true });
    Object.freeze(this);
  }
};
var SourceDirectoryValue = class {
  path;
  constructor(path) {
    this.path = path;
    Object.defineProperty(this, sourceDirectoryBrand, { value: true });
    Object.freeze(this);
  }
};
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
    return void 0;
  }
  const imageValue = value;
  return { key: imageValue.key, steps: imageValue.steps };
}

// sdk/typescript/src/workspace.ts
var sandboxDefinitionBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.sandbox");
var workspaceAddressBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.workspace-address");
var workspaces = Object.freeze({
  ref: createWorkspaceRef
});
function inspectSandboxDefinition(value) {
  if (typeof value !== "object" || value === null) return void 0;
  if (!Object.hasOwn(value, sandboxDefinitionBrand)) return void 0;
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
      return currentRuntimeOperations().workspaceRetrieve(
        workspaceID,
        options?.signal
      );
    },
    exec(request, options) {
      return currentRuntimeOperations().workspaceExec(
        workspaceID,
        request,
        options?.signal
      );
    },
    delete(request, options) {
      return currentRuntimeOperations().workspaceDelete(
        workspaceID,
        request,
        options?.signal
      );
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
var textEncoder = new TextEncoder();
function canonicalizeJsonValue(value) {
  return textEncoder.encode(serialize(value, /* @__PURE__ */ new Set()));
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
import { mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, resolve as resolve3 } from "node:path";

// compiler/typescript/src/analysis.ts
import { lstat, readdir, realpath } from "node:fs/promises";
import { relative, resolve, sep } from "node:path";

// compiler/typescript/src/utf8.ts
function compareUTF82(left, right) {
  return Buffer.compare(Buffer.from(left), Buffer.from(right));
}
function hasOnlyUnicodeScalarValues2(value) {
  for (let index = 0; index < value.length; index++) {
    const unit = value.charCodeAt(index);
    if (unit >= 55296 && unit <= 56319) {
      if (index + 1 === value.length) return false;
      const next = value.charCodeAt(index + 1);
      if (next < 56320 || next > 57343) return false;
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
    files.push(
      {
        path: "helmr/analysis-locators.json",
        content: decodeGeneratedFile(analysis.declarationLocatorBytes)
      }
    );
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
    throw new Error(
      `verification failure message must be nonblank UTF-8 of at most ${maxVerificationFailureMessageBytes} bytes`
    );
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
  const candidates = /* @__PURE__ */ new Set();
  for (const configured of config.dirs) {
    const directory = resolve(canonicalRoot, configured);
    if (!inside(canonicalRoot, directory)) {
      throw new Error(`configured dir escapes the project root: ${configured}`);
    }
    const relativeDirectory = projectPath(canonicalRoot, directory);
    if (hasComponent(relativeDirectory, "node_modules")) {
      throw new Error(`configured dir enters the dependency namespace: ${configured}`);
    }
    await requireUnlinkedDirectory(canonicalRoot, directory, configured);
    await appendCandidates(canonicalRoot, directory, candidates);
  }
  const rootConfig = await realpath(resolve(canonicalRoot, "helmr.config.ts")).catch((error) => {
    if (error.code === "ENOENT") return void 0;
    throw error;
  });
  const modules = [...candidates].filter(
    (path) => resolve(canonicalRoot, path) !== rootConfig && !config.ignorePatterns.some((pattern) => matchesIgnorePattern(pattern, path))
  );
  modules.sort(compareUTF82);
  return modules;
}
async function appendCandidates(root, directory, candidates) {
  const entries = await readdir(directory, { withFileTypes: true });
  entries.sort((left, right) => compareUTF82(left.name, right.name));
  for (const entry of entries) {
    const absolute = resolve(directory, entry.name);
    const path = projectPath(root, absolute);
    if (hasComponent(path, "node_modules")) continue;
    const metadata = await lstat(absolute);
    if (metadata.isSymbolicLink()) continue;
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
    if (error.code === "ENOENT") return;
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

// compiler/typescript/src/source.ts
import { installModuleExecution, moduleExecutionIdentity } from "../moduleexecution/loader.mjs";
import { createHash } from "node:crypto";
import { realpath as realpath2 } from "node:fs/promises";
import { resolve as resolve2 } from "node:path";
import { pathToFileURL } from "node:url";

// compiler/typescript/src/compile.ts
var BUILD_PLAN_FORMAT_VERSION = 0;
var DECLARATION_LOCATOR_FORMAT_VERSION = 0;
function analyze(options) {
  const located = locateDefinitions(options);
  const queues = compileQueues(located, options.exports);
  const sandboxExports = /* @__PURE__ */ new Map();
  for (const item of located) {
    if (item.definition.kind === "sandbox") {
      sandboxExports.set(item.definition.id, item.value);
    }
  }
  const definitions = located.map(
    ({ definition }) => compileDefinition(definition, options, queues, sandboxExports)
  );
  const programExports = compileProgramExports(located);
  const buildPlan = Object.freeze({
    formatVersion: BUILD_PLAN_FORMAT_VERSION,
    definitions: Object.freeze(definitions),
    queues: Object.freeze(
      [...queues.values()].map((entry) => Object.freeze({ ...entry })).sort((left, right) => compareUTF82(left.name, right.name))
    )
  });
  const declarationLocator = Object.freeze({
    declarations: programExports.declarationLocator.declarations,
    formatVersion: DECLARATION_LOCATOR_FORMAT_VERSION
  });
  return {
    buildPlan,
    buildPlanBytes: canonicalizeJsonValue(buildPlan),
    declarationLocator,
    declarationLocatorBytes: canonicalizeJsonValue(
      declarationLocator
    ),
    programDeclarations: programExports.programDeclarations
  };
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
  const declarations = located.flatMap(
    (item) => item.definition.kind === "sandbox" ? [] : [locatorEntry(item)]
  );
  const programDeclarations = located.flatMap(
    ({ definition }) => definition.kind === "sandbox" ? [] : [programDeclaration(definition)]
  );
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
  const identities = /* @__PURE__ */ new Map();
  for (const item of exports) {
    const definition = inspectDefinition(item.value) ?? inspectSandboxDefinition(item.value);
    if (definition === void 0) continue;
    validateSourcePath(item.sourcePath);
    validateExportName(item.exportName);
    const key = `${definition.kind}\0${definition.id}`;
    const existing = identities.get(key);
    if (existing !== void 0) {
      if (existing.value === item.value) {
        const candidate = {
          definition,
          sourcePath: item.sourcePath,
          exportName: item.exportName,
          value: item.value
        };
        if (compareLocatorOccurrence(candidate, existing.located) < 0) {
          existing.located = candidate;
        }
        continue;
      }
      throw new Error(
        `duplicate ${definition.kind} declaration ${JSON.stringify(definition.id)} at ${existing.located.sourcePath}#${existing.located.exportName} and ${item.sourcePath}#${item.exportName}`
      );
    }
    const located = {
      definition,
      sourcePath: item.sourcePath,
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
          ...definition.schedule === void 0 ? {} : {
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
          idleTimeoutMs: definition.idleTimeout === void 0 ? 3e4 : normalizeDuration(
            definition.idleTimeout,
            `actor ${JSON.stringify(definition.id)} idleTimeout`,
            1,
            36e5
          )
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
  if (schedule === void 0) throw new Error("Task schedule is undefined");
  const sandbox = inspectSandboxDefinition(schedule.workspace.sandbox);
  if (sandbox === void 0) {
    throw new Error(
      `task ${JSON.stringify(definition.id)} schedule has an invalid Sandbox definition`
    );
  }
  const exported = sandboxExports.get(sandbox.id);
  if (exported === void 0) {
    throw new Error(
      `task ${JSON.stringify(definition.id)} schedule references unexported Sandbox ${JSON.stringify(sandbox.id)}`
    );
  }
  if (exported !== schedule.workspace.sandbox) {
    throw new Error(
      `task ${JSON.stringify(definition.id)} schedule references a different Sandbox object than the exported definition ${JSON.stringify(sandbox.id)}`
    );
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
  const queues = /* @__PURE__ */ new Map();
  for (const item of exports) {
    if (isQueue(item.value)) {
      addQueue(queues, item.value, item.value);
    }
  }
  for (const { definition } of located) {
    if (definition.kind !== "task" && definition.kind !== "actor") continue;
    if (typeof definition.queue === "object") {
      addQueue(queues, definition.queue, definition.queue);
    } else if (definition.queue === void 0) {
      addQueue(queues, {
        name: `${definition.kind}/${definition.id}`
      }, definition);
    }
  }
  for (const { definition } of located) {
    if ((definition.kind === "task" || definition.kind === "actor") && typeof definition.queue === "string" && !queues.has(definition.queue)) {
      throw new Error(
        `${definition.kind} ${JSON.stringify(definition.id)} references undefined queue ${JSON.stringify(definition.queue)}`
      );
    }
  }
  if (queues.size > 1e3) throw new Error("BuildPlan queues exceed 1000");
  return new Map(
    [...queues].map(([name, entry]) => [name, entry.queue])
  );
}
function addQueue(queues, queue, owner) {
  validateQueueName(queue.name);
  const next = {
    name: queue.name,
    ...queue.concurrencyLimit === void 0 || queue.concurrencyLimit === null ? {} : { concurrencyLimit: queue.concurrencyLimit }
  };
  const existing = queues.get(queue.name);
  if (existing !== void 0) {
    if (existing.owner === owner) return;
    throw new Error(`duplicate queue declaration ${JSON.stringify(queue.name)}`);
  }
  queues.set(queue.name, { owner, queue: next });
}
function normalizeRun(definition, kind, queues) {
  const queue = definition.queue === void 0 ? `${kind}/${definition.id}` : typeof definition.queue === "string" ? definition.queue : definition.queue.name;
  if (!queues.has(queue)) {
    throw new Error(`${kind} ${JSON.stringify(definition.id)} queue is undefined`);
  }
  const maxDurationMs = definition.maxDuration === void 0 ? 9e5 : normalizeDuration(
    definition.maxDuration,
    `${kind} ${JSON.stringify(definition.id)} maxDuration`,
    5e3,
    864e5
  );
  return {
    queue,
    maxDurationMs,
    retry: normalizeRetry(definition.retry),
    ...definition.ttl === void 0 ? {} : {
      ttlMs: normalizeDuration(
        definition.ttl,
        `${kind} ${JSON.stringify(definition.id)} ttl`,
        1,
        31536e6
      )
    }
  };
}
function normalizeRetry(retry) {
  if (retry === void 0 || retry.enabled === false) {
    return { enabled: false };
  }
  if (!Number.isInteger(retry.maxAttempts) || retry.maxAttempts < 1 || retry.maxAttempts > 10) {
    throw new Error("retry maxAttempts must be an integer in [1,10]");
  }
  const minMs = retry.backoff?.minDelay === void 0 ? 1e3 : normalizeDuration(
    retry.backoff.minDelay,
    "retry backoff minDelay",
    1,
    864e5
  );
  const maxMs = retry.backoff?.maxDelay === void 0 ? 3e4 : normalizeDuration(
    retry.backoff.maxDelay,
    "retry backoff maxDelay",
    1,
    864e5
  );
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
  const images = /* @__PURE__ */ new Map();
  const visiting = /* @__PURE__ */ new Set();
  const visit = (image) => {
    if (visiting.has(image.key)) {
      throw new Error(`image graph contains a cycle at ${JSON.stringify(image.key)}`);
    }
    const existing = images.get(image.key);
    if (existing !== void 0) {
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
        if (source2 === void 0) throw new Error("invalid copyFrom image");
        visit(source2);
      }
    }
    visiting.delete(image.key);
  };
  visit(root);
  const specs = [...images.values()].sort((left, right) => compareUTF82(left.key, right.key)).map((image) => ({
    key: image.key,
    platform: {
      os: "linux",
      architecture: options.architecture
    },
    steps: image.steps.map((step) => compileImageStep(step, options))
  }));
  const stepCount = specs.reduce((total, image) => total + image.steps.length, 0);
  if (stepCount > 1e4) throw new Error("image build exceeds 10000 steps");
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
      assertExactKeys(
        step,
        ["destination", "kind", "source"],
        "image source-file copy step"
      );
      return {
        copySourceFile: {
          dst: step.destination,
          path: step.source.path
        }
      };
    case "copy_source_directory":
      assertExactKeys(
        step,
        ["destination", "kind", "source"],
        "image source-directory copy step"
      );
      return {
        copySourceDir: {
          dst: step.destination,
          path: step.source.path
        }
      };
    case "copy_from_image": {
      assertExactKeys(
        step,
        ["destination", "kind", "source", "sourcePath"],
        "image cross-image copy step"
      );
      const source2 = inspectImage(step.source);
      if (source2 === void 0) throw new Error("invalid copyFrom image");
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
    sourcePath: item.sourcePath,
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
  if (match === null) throw new Error("workspace cpu cannot be normalized");
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
    throw new Error(
      `workspace ${label} must be a positive canonical integer suffixed by MiB or GiB`
    );
  }
  const result = BigInt(match[1]) * (match[2] === "GiB" ? 1024n : 1n);
  return safePositiveNumber(result, `workspace ${label} MiB`);
}
function normalizeDuration(value, label, minimumMs, maximumMs) {
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(value);
  if (match === null) {
    throw new Error(
      `${label} must match ^[1-9][0-9]*(ms|s|m|h|d)$`
    );
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
    throw new Error(
      `${label} must resolve to milliseconds in [${minimumMs},${maximumMs}]`
    );
  }
  return Number(milliseconds);
}
function safePositiveNumber(value, label) {
  if (value <= 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error(`${label} must be a positive safe integer`);
  }
  return Number(value);
}
function validateSourcePath(path) {
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
  if (path.length === 0 || !hasOnlyUnicodeScalarValues2(path) || path.startsWith("/") || path.includes("\\") || /[\p{Cc}]/u.test(path) || components.some(
    (component) => component === "" || component === "." || component === ".."
  ) || components.includes("node_modules") || components[0] === "helmr" || path === "helmr.config.ts" || path.endsWith(".d.ts") || path.endsWith(".d.mts") || path.endsWith(".d.cts") || !suffixes.some((suffix) => path.endsWith(suffix))) {
    throw new Error(
      `sourcePath ${JSON.stringify(path)} is not an admitted first-party module path`
    );
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
  return compareUTF82(left.sourcePath, right.sourcePath) || compareUTF82(left.exportName, right.exportName);
}

// compiler/typescript/src/source.ts
var COMPILER_API_VERSION = "helmr.compiler.v0";
function compilerContract() {
  return { apiVersion: COMPILER_API_VERSION, language: moduleExecutionIdentity() };
}
async function compileProgram(options) {
  if (process.versions.node !== options.nodeVersion) throw new Error(`Program Compiler Node version ${process.versions.node} does not match ${options.nodeVersion}`);
  if (!/^sha256:[0-9a-f]{64}$/.test(options.inputTreeDigest)) throw new Error("Program Compiler input tree digest is invalid");
  const root = await realpath2(options.root);
  const language = moduleExecutionIdentity();
  const execution = installModuleExecution({ root });
  try {
    const modules = await discoverModules(root, options.config);
    if (modules.length === 0) throw new Error("configured dirs contain no declaration source modules");
    const exports = [];
    for (const sourcePath of modules) {
      const namespace = await execution.importSourceExports(pathToFileURL(resolve2(root, sourcePath)));
      for (const exportName of Object.keys(namespace).sort(compareUTF82)) {
        exports.push({ sourcePath, exportName, value: namespace[exportName] });
      }
    }
    const analysis = analyze({ architecture: options.architecture, exports });
    const configBytes = canonicalizeJsonValue(options.config);
    const result = {
      apiVersion: COMPILER_API_VERSION,
      language,
      nodeVersion: options.nodeVersion,
      config: { path: "helmr/config.json", digest: `sha256:${createHash("sha256").update(configBytes).digest("hex")}` },
      inputTreeDigest: options.inputTreeDigest,
      discoveryCandidates: modules,
      selections: analysis.declarationLocator.declarations
    };
    return { analysis, modules, files: /* @__PURE__ */ new Map([
      ["helmr/config.json", configBytes],
      ["helmr/compiler-result.json", canonicalizeJsonValue(result)]
    ]) };
  } finally {
    execution.dispose();
  }
}

// compiler/typescript/src/config.ts
function inspectCanonicalConfig(value) {
  if (typeof value !== "object" || value === null || Array.isArray(value) || Object.getPrototypeOf(value) !== Object.prototype) {
    throw new Error("canonical config must be an ordinary object");
  }
  const record = value;
  const keys = Object.keys(record).sort();
  if (keys.length !== 2 || keys[0] !== "dirs" || keys[1] !== "ignorePatterns") {
    throw new Error("canonical config does not match the build contract");
  }
  const config = inspectConfig({
    dirs: record["dirs"],
    ignorePatterns: record["ignorePatterns"]
  });
  return { dirs: config.dirs, ignorePatterns: config.ignorePatterns };
}

// compiler/typescript/src/program-compiler.ts
async function main() {
  if (process.argv.length === 3 && process.argv[2] === "--describe") {
    process.stdout.write(canonicalizeJsonValue(compilerContract()));
    return;
  }
  if (process.argv.length !== 7 || process.argv[2] === void 0 || process.argv[3] === void 0 || process.argv[4] === void 0 || process.argv[5] === void 0 || process.argv[6] === void 0) {
    throw new Error(
      "Program Compiler requires a Program root, canonical config path, exact Node version, input tree digest, and output root"
    );
  }
  const root = resolve3(process.argv[2]);
  const config = inspectCanonicalConfig(
    JSON.parse(await readFile(process.argv[3], "utf8"))
  );
  const compiled = await compileProgram({
    architecture: "x86_64",
    config,
    nodeVersion: process.argv[4],
    inputTreeDigest: process.argv[5],
    root
  });
  for (const [path, contents] of compiled.files) {
    const target = resolve3(process.argv[6], path);
    await mkdir(dirname(target), { recursive: true });
    await writeFile(target, contents);
  }
  await writeResult(successfulVerificationResult(compiled.analysis));
}
async function writeResult(result) {
  const configured = process.env["HELMR_SUPERVISOR_FD"];
  const fd = configured === void 0 ? 3 : Number(configured);
  if (!Number.isSafeInteger(fd) || fd < 3) {
    throw new Error("Program Compiler result descriptor is invalid");
  }
  const output = createWriteStream("", { fd, autoClose: false });
  const frame = encodeVerificationResultFrame(result);
  await new Promise((resolve4, reject) => {
    output.once("error", reject);
    output.end(frame, resolve4);
  });
}
try {
  await main();
} catch (error) {
  if (process.argv[2] === "--describe") throw error;
  const message = error instanceof Error ? error.message : String(error);
  await writeResult(failedVerificationResult(message));
}
