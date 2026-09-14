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
function hasControl(value) {
  for (let index = 0;index < value.length; index++) {
    const code = charCodeAt2(value, index);
    if (code <= 31 || code >= 127 && code <= 159)
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
// sdk/typescript/src/workspace.ts
var sandboxDefinitionBrand = Symbol.for("helmr.sdk.v0.sandbox");
var workspaceAddressBrand = Symbol.for("helmr.sdk.v0.workspace-address");
var workspaces = Object.freeze({
  ref: createWorkspaceRef
});
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
// compiler/typescript/src/config-evaluator.ts
import { createWriteStream } from "node:fs";

// compiler/typescript/src/bundle.ts
import {
  build as build2,
  version as esbuildVersion
} from "esbuild";
import { createHash } from "node:crypto";
import { createRequire } from "node:module";
import {
  lstat,
  mkdir,
  readFile as readFile2,
  realpath,
  rm,
  stat,
  writeFile
} from "node:fs/promises";
import { dirname as dirname2, relative as relative2, resolve, sep } from "node:path";
import { pathToFileURL as pathToFileURL2 } from "node:url";

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

// compiler/typescript/src/utf8.ts
function compareUTF82(left, right) {
  return Buffer.compare(Buffer.from(left), Buffer.from(right));
}

// compiler/typescript/src/analysis.ts
var textDecoder = new TextDecoder("utf-8", { fatal: true });
var maxVerificationFailureMessageBytes = 16 << 10;

// compiler/typescript/src/compile-selection.ts
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

// compiler/typescript/src/config-origin.ts
import { build } from "esbuild";
import { readFile } from "node:fs/promises";
import { dirname, extname, relative } from "node:path";
import { pathToFileURL } from "node:url";
function configOrigin(root, target) {
  return {
    name: "helmr-config-origin",
    setup(outer) {
      outer.onLoad({ filter: /\.[cm]?[jt]sx?$/ }, async ({ path }) => {
        const contents = await readFile(path, "utf8");
        const extension = extname(path);
        const loader = extension === ".tsx" ? "tsx" : extension === ".jsx" ? "jsx" : extension.endsWith("ts") ? "ts" : "js";
        const location = JSON.stringify(relative(root, path));
        const shim = `
          import { createRequire } from "node:module";
          const url = ${JSON.stringify(pathToFileURL(path).href)};
          const filename = ${JSON.stringify(path)};
          const dirname = ${JSON.stringify(dirname(path))};
          const meta = { url, filename, dirname, main: false, resolve() {
            throw new Error(${JSON.stringify(`Config source ${location}: import.meta.resolve() is unsupported; use a Node-ready installed JavaScript helper for native import resolution`)});
          }};
          const resolve = createRequire(url).resolve;
          export { meta as "import.meta", filename as "__filename",
            dirname as "__dirname", resolve as "require.resolve" };
        `;
        const transformed = await build({
          absWorkingDir: root,
          entryPoints: [path],
          outfile: `${path}.helmr-origin.js`,
          bundle: false,
          write: false,
          platform: "node",
          target,
          sourcemap: "inline",
          sourcesContent: true,
          logLevel: "silent",
          inject: ["<helmr-config-origin>"],
          plugins: [{
            name: "helmr-captured-config-source",
            setup(inner) {
              inner.onResolve({ filter: /^<helmr-config-origin>$/ }, () => ({
                path,
                namespace: "helmr-origin"
              }));
              inner.onLoad({ filter: /.*/, namespace: "helmr-origin" }, () => ({
                contents: shim,
                loader: "js"
              }));
              inner.onLoad({ filter: /.*/ }, (args) => args.path === path ? { contents, loader, resolveDir: dirname(path) } : undefined);
            }
          }]
        });
        if (transformed.outputFiles?.length !== 1) {
          throw new Error("config origin transform must produce one virtual output");
        }
        return {
          contents: transformed.outputFiles[0].text,
          loader: "js",
          resolveDir: dirname(path)
        };
      });
    }
  };
}

// compiler/typescript/src/bundle.ts
var ESBUILD_VERSION = "0.28.2";
if (esbuildVersion !== ESBUILD_VERSION) {
  throw new Error(`esbuild version ${JSON.stringify(esbuildVersion)} does not match ${ESBUILD_VERSION}`);
}
async function compileConfig(options) {
  const root = await realpath(options.root);
  const entry = resolve(root, "helmr.config.ts");
  if (!(await lstat(entry)).isFile()) {
    throw new Error("helmr.config.ts must be a regular file");
  }
  const outputRoot = resolve(options.outputRoot, "config");
  await mkdir(outputRoot, { recursive: false });
  try {
    const output = resolve(outputRoot, "config-evaluation.mjs");
    const compiled = await bundleFile({
      root,
      entry,
      nodeVersion: options.nodeVersion,
      outfile: output,
      runtimeRoot: root
    });
    await compilerTSConfigs(root, Object.keys(compiled.metafile.inputs));
    await writeFile(`${output}.map`, compiled.map);
    await writeFile(output, compiled.code);
    return {
      path: output,
      cleanup: async () => {
        await rm(outputRoot, { force: true, recursive: true });
      }
    };
  } catch (error) {
    await rm(outputRoot, { force: true, recursive: true });
    throw error;
  }
}
async function bundleFile(options) {
  const externalEdges = [];
  const result = await build2({
    ...baseOptions(options.root, options.runtimeRoot, options.nodeVersion, new Set, externalEdges, [configOrigin(options.root, esbuildNodeTarget(options.nodeVersion))], "config"),
    sourcemap: "linked",
    logOverride: {
      "unsupported-require-call": "error",
      "indirect-require": "error",
      "unsupported-dynamic-import": "error"
    },
    entryPoints: [options.entry],
    outfile: options.outfile
  });
  const output = {
    ...singleOutputFiles(requireOutputFiles(result.outputFiles), options.outfile),
    externalEdges: sortedExternalEdges(externalEdges),
    metafile: requiredMetafile(result.metafile)
  };
  return output;
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
function esbuildNodeTarget(nodeVersion) {
  if (!/^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)$/.test(nodeVersion)) {
    throw new Error("Compiler Node version must be an exact canonical SemVer");
  }
  return `node${nodeVersion}`;
}
function dependencyBoundary(root, runtimeRoot, selectedRoots, externalEdges, phase) {
  const canonicalRoot = resolve(root);
  return {
    name: "helmr-dependency-boundary",
    async setup(build3) {
      const rootConfig = phase === "program" ? await realpath(resolve(canonicalRoot, "helmr.config.ts")).catch((error) => {
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
        const logicalPath = projectPath(canonicalRoot, resolve(result.path));
        const path = await realpath(result.path);
        const resolvedPath = projectPath(canonicalRoot, path);
        if (!inside(relative2(canonicalRoot, path))) {
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
          const importer = args.importer === "" ? args.importer : projectPath(canonicalRoot, resolve(args.importer));
          if (/\.(?:ts|tsx|mts|cts)$/.test(resolvedPath)) {
            return { errors: [{
              text: `Cannot externalize TypeScript dependency ${JSON.stringify(args.path)} imported by ${JSON.stringify(importer)}: ${resolvedPath}. Node cannot execute TypeScript under node_modules. Add ${JSON.stringify(installedPackageRoot(resolvedPath))} to helmr.config.ts compilePackages to compile this installed package, or install Node-ready JavaScript.`
            }] };
          }
          const runtimePath = resolve(runtimeRoot, logicalPath);
          externalEdges.push({
            importer,
            kind: args.kind,
            logicalPath,
            resolvedPath,
            runtimePath,
            specifier: args.path
          });
          const target = resolve(runtimeRoot, logicalPath);
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
async function compilerTSConfigs(root, inputs) {
  const roots = new Set;
  for (const input of inputs) {
    let directory = dirname2(resolve(root, input));
    for (;; ) {
      const candidate = resolve(directory, "tsconfig.json");
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
      const parent = dirname2(directory);
      if (!inside(relative2(root, parent)))
        break;
      directory = parent;
    }
  }
  const configs = new Map;
  const pending = [...roots];
  while (pending.length !== 0) {
    const candidate = await realpath(pending.shift());
    const path = projectPath(root, candidate);
    if (!inside(path)) {
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
  const directory = dirname2(configPath);
  if (specifier.startsWith(".") || specifier.startsWith("/") || /^[A-Za-z]:[\\/]/.test(specifier)) {
    return requiredConfigPath(resolve(directory, specifier));
  }
  const require2 = createRequire(pathToFileURL2(configPath));
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
    resolve(candidate, "tsconfig.json")
  ]) {
    try {
      if ((await stat(path)).isFile())
        return path;
    } catch (error) {
      if (error.code !== "ENOENT")
        throw error;
    }
  }
  throw new Error(`extended tsconfig ${JSON.stringify(candidate)} does not exist`);
}
function projectPath(root, value) {
  return relative2(root, value).split(sep).join("/");
}
function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}
function hasNodeModules(path) {
  return path.split(sep).includes("node_modules");
}
function inside(path) {
  return path === "" || path !== ".." && !path.startsWith(`..${sep}`) && !path.startsWith("/");
}

// compiler/typescript/src/config.ts
import { lstat as lstat2 } from "node:fs/promises";
import { pathToFileURL as pathToFileURL3 } from "node:url";

class MissingConfigError extends Error {
  constructor(path) {
    super(`missing helmr.config.ts at ${path}`);
    this.name = "MissingConfigError";
  }
}
async function loadConfig(path) {
  let metadata;
  try {
    metadata = await lstat2(path);
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
    const value = await import(pathToFileURL3(path).href);
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

// compiler/typescript/src/config-evaluator.ts
var maxConfigBytes = 1 << 20;
async function main() {
  if (process.argv.length !== 5 || process.argv[2] === undefined || process.argv[3] === undefined || process.argv[4] === undefined) {
    throw new Error("Config Evaluator requires a Program root, exact Node version, and output root");
  }
  const compiled = await compileConfig({
    nodeVersion: process.argv[3],
    outputRoot: process.argv[4],
    root: process.argv[2]
  });
  let config;
  try {
    config = await loadConfig(compiled.path);
  } finally {
    await compiled.cleanup();
  }
  const body = canonicalizeJsonValue(config);
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
