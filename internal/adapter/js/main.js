import { createRequire } from "node:module";
var __defProp = Object.defineProperty;
var __returnValue = (v) => v;
function __exportSetter(name, newValue) {
  this[name] = __returnValue.bind(null, newValue);
}
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, {
      get: all[name],
      enumerable: true,
      configurable: true,
      set: __exportSetter.bind(all, name)
    });
};
var __require = /* @__PURE__ */ createRequire(import.meta.url);
// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/is-message.js
function isMessage(arg, schema) {
  const isMessage2 = arg !== null && typeof arg == "object" && "$typeName" in arg && typeof arg.$typeName == "string";
  if (!isMessage2) {
    return false;
  }
  if (schema === undefined) {
    return true;
  }
  return schema.typeName === arg.$typeName;
}
// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/descriptors.js
var ScalarType;
(function(ScalarType2) {
  ScalarType2[ScalarType2["DOUBLE"] = 1] = "DOUBLE";
  ScalarType2[ScalarType2["FLOAT"] = 2] = "FLOAT";
  ScalarType2[ScalarType2["INT64"] = 3] = "INT64";
  ScalarType2[ScalarType2["UINT64"] = 4] = "UINT64";
  ScalarType2[ScalarType2["INT32"] = 5] = "INT32";
  ScalarType2[ScalarType2["FIXED64"] = 6] = "FIXED64";
  ScalarType2[ScalarType2["FIXED32"] = 7] = "FIXED32";
  ScalarType2[ScalarType2["BOOL"] = 8] = "BOOL";
  ScalarType2[ScalarType2["STRING"] = 9] = "STRING";
  ScalarType2[ScalarType2["BYTES"] = 12] = "BYTES";
  ScalarType2[ScalarType2["UINT32"] = 13] = "UINT32";
  ScalarType2[ScalarType2["SFIXED32"] = 15] = "SFIXED32";
  ScalarType2[ScalarType2["SFIXED64"] = 16] = "SFIXED64";
  ScalarType2[ScalarType2["SINT32"] = 17] = "SINT32";
  ScalarType2[ScalarType2["SINT64"] = 18] = "SINT64";
})(ScalarType || (ScalarType = {}));

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/wire/varint.js
function varint64read() {
  let lowBits = 0;
  let highBits = 0;
  for (let shift = 0;shift < 28; shift += 7) {
    let b = this.buf[this.pos++];
    lowBits |= (b & 127) << shift;
    if ((b & 128) == 0) {
      this.assertBounds();
      return [lowBits, highBits];
    }
  }
  let middleByte = this.buf[this.pos++];
  lowBits |= (middleByte & 15) << 28;
  highBits = (middleByte & 112) >> 4;
  if ((middleByte & 128) == 0) {
    this.assertBounds();
    return [lowBits, highBits];
  }
  for (let shift = 3;shift <= 31; shift += 7) {
    let b = this.buf[this.pos++];
    highBits |= (b & 127) << shift;
    if ((b & 128) == 0) {
      this.assertBounds();
      return [lowBits, highBits];
    }
  }
  throw new Error("invalid varint");
}
function varint64write(lo, hi, bytes) {
  for (let i = 0;i < 28; i = i + 7) {
    const shift = lo >>> i;
    const hasNext = !(shift >>> 7 == 0 && hi == 0);
    const byte = (hasNext ? shift | 128 : shift) & 255;
    bytes.push(byte);
    if (!hasNext) {
      return;
    }
  }
  const splitBits = lo >>> 28 & 15 | (hi & 7) << 4;
  const hasMoreBits = !(hi >> 3 == 0);
  bytes.push((hasMoreBits ? splitBits | 128 : splitBits) & 255);
  if (!hasMoreBits) {
    return;
  }
  for (let i = 3;i < 31; i = i + 7) {
    const shift = hi >>> i;
    const hasNext = !(shift >>> 7 == 0);
    const byte = (hasNext ? shift | 128 : shift) & 255;
    bytes.push(byte);
    if (!hasNext) {
      return;
    }
  }
  bytes.push(hi >>> 31 & 1);
}
var TWO_PWR_32_DBL = 4294967296;
function int64FromString(dec) {
  const minus = dec[0] === "-";
  if (minus) {
    dec = dec.slice(1);
  }
  const base = 1e6;
  let lowBits = 0;
  let highBits = 0;
  function add1e6digit(begin, end) {
    const digit1e6 = Number(dec.slice(begin, end));
    highBits *= base;
    lowBits = lowBits * base + digit1e6;
    if (lowBits >= TWO_PWR_32_DBL) {
      highBits = highBits + (lowBits / TWO_PWR_32_DBL | 0);
      lowBits = lowBits % TWO_PWR_32_DBL;
    }
  }
  add1e6digit(-24, -18);
  add1e6digit(-18, -12);
  add1e6digit(-12, -6);
  add1e6digit(-6);
  return minus ? negate(lowBits, highBits) : newBits(lowBits, highBits);
}
function int64ToString(lo, hi) {
  let bits = newBits(lo, hi);
  const negative = bits.hi & 2147483648;
  if (negative) {
    bits = negate(bits.lo, bits.hi);
  }
  const result = uInt64ToString(bits.lo, bits.hi);
  return negative ? "-" + result : result;
}
function uInt64ToString(lo, hi) {
  ({ lo, hi } = toUnsigned(lo, hi));
  if (hi <= 2097151) {
    return String(TWO_PWR_32_DBL * hi + lo);
  }
  const low = lo & 16777215;
  const mid = (lo >>> 24 | hi << 8) & 16777215;
  const high = hi >> 16 & 65535;
  let digitA = low + mid * 6777216 + high * 6710656;
  let digitB = mid + high * 8147497;
  let digitC = high * 2;
  const base = 1e7;
  if (digitA >= base) {
    digitB += Math.floor(digitA / base);
    digitA %= base;
  }
  if (digitB >= base) {
    digitC += Math.floor(digitB / base);
    digitB %= base;
  }
  return digitC.toString() + decimalFrom1e7WithLeadingZeros(digitB) + decimalFrom1e7WithLeadingZeros(digitA);
}
function toUnsigned(lo, hi) {
  return { lo: lo >>> 0, hi: hi >>> 0 };
}
function newBits(lo, hi) {
  return { lo: lo | 0, hi: hi | 0 };
}
function negate(lowBits, highBits) {
  highBits = ~highBits;
  if (lowBits) {
    lowBits = ~lowBits + 1;
  } else {
    highBits += 1;
  }
  return newBits(lowBits, highBits);
}
var decimalFrom1e7WithLeadingZeros = (digit1e7) => {
  const partial = String(digit1e7);
  return "0000000".slice(partial.length) + partial;
};
function varint32write(value, bytes) {
  if (value >= 0) {
    while (value > 127) {
      bytes.push(value & 127 | 128);
      value = value >>> 7;
    }
    bytes.push(value);
  } else {
    for (let i = 0;i < 9; i++) {
      bytes.push(value & 127 | 128);
      value = value >> 7;
    }
    bytes.push(1);
  }
}
function varint32read() {
  let b = this.buf[this.pos++];
  let result = b & 127;
  if ((b & 128) == 0) {
    this.assertBounds();
    return result;
  }
  b = this.buf[this.pos++];
  result |= (b & 127) << 7;
  if ((b & 128) == 0) {
    this.assertBounds();
    return result;
  }
  b = this.buf[this.pos++];
  result |= (b & 127) << 14;
  if ((b & 128) == 0) {
    this.assertBounds();
    return result;
  }
  b = this.buf[this.pos++];
  result |= (b & 127) << 21;
  if ((b & 128) == 0) {
    this.assertBounds();
    return result;
  }
  b = this.buf[this.pos++];
  result |= (b & 15) << 28;
  for (let readBytes = 5;(b & 128) !== 0 && readBytes < 10; readBytes++)
    b = this.buf[this.pos++];
  if ((b & 128) != 0)
    throw new Error("invalid varint");
  this.assertBounds();
  return result >>> 0;
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/proto-int64.js
var protoInt64 = /* @__PURE__ */ makeInt64Support();
function makeInt64Support() {
  const dv = new DataView(new ArrayBuffer(8));
  const ok = typeof BigInt === "function" && typeof dv.getBigInt64 === "function" && typeof dv.getBigUint64 === "function" && typeof dv.setBigInt64 === "function" && typeof dv.setBigUint64 === "function" && (!!globalThis.Deno || typeof process != "object" || typeof process.env != "object" || process.env.BUF_BIGINT_DISABLE !== "1");
  if (ok) {
    const MIN = BigInt("-9223372036854775808");
    const MAX = BigInt("9223372036854775807");
    const UMIN = BigInt("0");
    const UMAX = BigInt("18446744073709551615");
    return {
      zero: BigInt(0),
      supported: true,
      parse(value) {
        const bi = typeof value == "bigint" ? value : BigInt(value);
        if (bi > MAX || bi < MIN) {
          throw new Error(`invalid int64: ${value}`);
        }
        return bi;
      },
      uParse(value) {
        const bi = typeof value == "bigint" ? value : BigInt(value);
        if (bi > UMAX || bi < UMIN) {
          throw new Error(`invalid uint64: ${value}`);
        }
        return bi;
      },
      enc(value) {
        dv.setBigInt64(0, this.parse(value), true);
        return {
          lo: dv.getInt32(0, true),
          hi: dv.getInt32(4, true)
        };
      },
      uEnc(value) {
        dv.setBigInt64(0, this.uParse(value), true);
        return {
          lo: dv.getInt32(0, true),
          hi: dv.getInt32(4, true)
        };
      },
      dec(lo, hi) {
        dv.setInt32(0, lo, true);
        dv.setInt32(4, hi, true);
        return dv.getBigInt64(0, true);
      },
      uDec(lo, hi) {
        dv.setInt32(0, lo, true);
        dv.setInt32(4, hi, true);
        return dv.getBigUint64(0, true);
      }
    };
  }
  return {
    zero: "0",
    supported: false,
    parse(value) {
      if (typeof value != "string") {
        value = value.toString();
      }
      assertInt64String(value);
      return value;
    },
    uParse(value) {
      if (typeof value != "string") {
        value = value.toString();
      }
      assertUInt64String(value);
      return value;
    },
    enc(value) {
      if (typeof value != "string") {
        value = value.toString();
      }
      assertInt64String(value);
      return int64FromString(value);
    },
    uEnc(value) {
      if (typeof value != "string") {
        value = value.toString();
      }
      assertUInt64String(value);
      return int64FromString(value);
    },
    dec(lo, hi) {
      return int64ToString(lo, hi);
    },
    uDec(lo, hi) {
      return uInt64ToString(lo, hi);
    }
  };
}
function assertInt64String(value) {
  if (!/^-?[0-9]+$/.test(value)) {
    throw new Error("invalid int64: " + value);
  }
}
function assertUInt64String(value) {
  if (!/^[0-9]+$/.test(value)) {
    throw new Error("invalid uint64: " + value);
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/scalar.js
function scalarZeroValue(type, longAsString) {
  switch (type) {
    case ScalarType.STRING:
      return "";
    case ScalarType.BOOL:
      return false;
    case ScalarType.DOUBLE:
    case ScalarType.FLOAT:
      return 0;
    case ScalarType.INT64:
    case ScalarType.UINT64:
    case ScalarType.SFIXED64:
    case ScalarType.FIXED64:
    case ScalarType.SINT64:
      return longAsString ? "0" : protoInt64.zero;
    case ScalarType.BYTES:
      return new Uint8Array(0);
    default:
      return 0;
  }
}
function isScalarZeroValue(type, value) {
  switch (type) {
    case ScalarType.BOOL:
      return value === false;
    case ScalarType.STRING:
      return value === "";
    case ScalarType.BYTES:
      return value instanceof Uint8Array && !value.byteLength;
    default:
      return value == 0;
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/unsafe.js
var IMPLICIT = 2;
var unsafeLocal = Symbol.for("reflect unsafe local");
function unsafeOneofCase(target, oneof) {
  const c = target[oneof.localName].case;
  if (c === undefined) {
    return c;
  }
  return oneof.fields.find((f) => f.localName === c);
}
function unsafeIsSet(target, field) {
  const name = field.localName;
  if (field.oneof) {
    return target[field.oneof.localName].case === name;
  }
  if (field.presence != IMPLICIT) {
    return target[name] !== undefined && Object.prototype.hasOwnProperty.call(target, name);
  }
  switch (field.fieldKind) {
    case "list":
      return target[name].length > 0;
    case "map":
      return Object.keys(target[name]).length > 0;
    case "scalar":
      return !isScalarZeroValue(field.scalar, target[name]);
    case "enum":
      return target[name] !== field.enum.values[0].number;
  }
  throw new Error("message field with implicit presence");
}
function unsafeIsSetExplicit(target, localName) {
  return Object.prototype.hasOwnProperty.call(target, localName) && target[localName] !== undefined;
}
function unsafeGet(target, field) {
  if (field.oneof) {
    const oneof = target[field.oneof.localName];
    if (oneof.case === field.localName) {
      return oneof.value;
    }
    return;
  }
  return target[field.localName];
}
function unsafeSet(target, field, value) {
  if (field.oneof) {
    target[field.oneof.localName] = {
      case: field.localName,
      value
    };
  } else {
    target[field.localName] = value;
  }
}
function unsafeClear(target, field) {
  const name = field.localName;
  if (field.oneof) {
    const oneofLocalName = field.oneof.localName;
    if (target[oneofLocalName].case === name) {
      target[oneofLocalName] = { case: undefined };
    }
  } else if (field.presence != IMPLICIT) {
    delete target[name];
  } else {
    switch (field.fieldKind) {
      case "map":
        target[name] = {};
        break;
      case "list":
        target[name] = [];
        break;
      case "enum":
        target[name] = field.enum.values[0].number;
        break;
      case "scalar":
        target[name] = scalarZeroValue(field.scalar, field.longAsString);
        break;
    }
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/guard.js
function isObject(arg) {
  return arg !== null && typeof arg == "object" && !Array.isArray(arg);
}
function isReflectList(arg, field) {
  var _a, _b, _c, _d;
  if (isObject(arg) && unsafeLocal in arg && "add" in arg && "field" in arg && typeof arg.field == "function") {
    if (field !== undefined) {
      const a = field;
      const b = arg.field();
      return a.listKind == b.listKind && a.scalar === b.scalar && ((_a = a.message) === null || _a === undefined ? undefined : _a.typeName) === ((_b = b.message) === null || _b === undefined ? undefined : _b.typeName) && ((_c = a.enum) === null || _c === undefined ? undefined : _c.typeName) === ((_d = b.enum) === null || _d === undefined ? undefined : _d.typeName);
    }
    return true;
  }
  return false;
}
function isReflectMap(arg, field) {
  var _a, _b, _c, _d;
  if (isObject(arg) && unsafeLocal in arg && "has" in arg && "field" in arg && typeof arg.field == "function") {
    if (field !== undefined) {
      const a = field, b = arg.field();
      return a.mapKey === b.mapKey && a.mapKind == b.mapKind && a.scalar === b.scalar && ((_a = a.message) === null || _a === undefined ? undefined : _a.typeName) === ((_b = b.message) === null || _b === undefined ? undefined : _b.typeName) && ((_c = a.enum) === null || _c === undefined ? undefined : _c.typeName) === ((_d = b.enum) === null || _d === undefined ? undefined : _d.typeName);
    }
    return true;
  }
  return false;
}
function isReflectMessage(arg, messageDesc) {
  return isObject(arg) && unsafeLocal in arg && "desc" in arg && isObject(arg.desc) && arg.desc.kind === "message" && (messageDesc === undefined || arg.desc.typeName == messageDesc.typeName);
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/wkt/wrappers.js
function isWrapper(arg) {
  return isWrapperTypeName(arg.$typeName);
}
function isWrapperDesc(messageDesc) {
  const f = messageDesc.fields[0];
  return isWrapperTypeName(messageDesc.typeName) && f !== undefined && f.fieldKind == "scalar" && f.name == "value" && f.number == 1;
}
function isWrapperTypeName(name) {
  return name.startsWith("google.protobuf.") && [
    "DoubleValue",
    "FloatValue",
    "Int64Value",
    "UInt64Value",
    "Int32Value",
    "UInt32Value",
    "BoolValue",
    "StringValue",
    "BytesValue"
  ].includes(name.substring(16));
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/create.js
var EDITION_PROTO3 = 999;
var EDITION_PROTO2 = 998;
var IMPLICIT2 = 2;
function create(schema, init) {
  if (isMessage(init, schema)) {
    return init;
  }
  const message = createZeroMessage(schema);
  if (init !== undefined) {
    initMessage(schema, message, init);
  }
  return message;
}
function initMessage(messageDesc, message, init) {
  for (const member of messageDesc.members) {
    let value = init[member.localName];
    if (value == null) {
      continue;
    }
    let field;
    if (member.kind == "oneof") {
      const oneofField = unsafeOneofCase(init, member);
      if (!oneofField) {
        continue;
      }
      field = oneofField;
      value = unsafeGet(init, oneofField);
    } else {
      field = member;
    }
    switch (field.fieldKind) {
      case "message":
        value = toMessage(field, value);
        break;
      case "scalar":
        value = initScalar(field, value);
        break;
      case "list":
        value = initList(field, value);
        break;
      case "map":
        value = initMap(field, value);
        break;
    }
    unsafeSet(message, field, value);
  }
  return message;
}
function initScalar(field, value) {
  if (field.scalar == ScalarType.BYTES) {
    return toU8Arr(value);
  }
  return value;
}
function initMap(field, value) {
  if (isObject(value)) {
    if (field.scalar == ScalarType.BYTES) {
      return convertObjectValues(value, toU8Arr);
    }
    if (field.mapKind == "message") {
      return convertObjectValues(value, (val) => toMessage(field, val));
    }
  }
  return value;
}
function initList(field, value) {
  if (Array.isArray(value)) {
    if (field.scalar == ScalarType.BYTES) {
      return value.map(toU8Arr);
    }
    if (field.listKind == "message") {
      return value.map((item) => toMessage(field, item));
    }
  }
  return value;
}
function toMessage(field, value) {
  if (field.fieldKind == "message" && !field.oneof && isWrapperDesc(field.message)) {
    return initScalar(field.message.fields[0], value);
  }
  if (isObject(value)) {
    if (field.message.typeName == "google.protobuf.Struct" && field.parent.typeName !== "google.protobuf.Value") {
      return value;
    }
    if (!isMessage(value, field.message)) {
      return create(field.message, value);
    }
  }
  return value;
}
function toU8Arr(value) {
  return Array.isArray(value) ? new Uint8Array(value) : value;
}
function convertObjectValues(obj, fn) {
  const ret = {};
  for (const entry of Object.entries(obj)) {
    ret[entry[0]] = fn(entry[1]);
  }
  return ret;
}
var tokenZeroMessageField = Symbol();
var messagePrototypes = new WeakMap;
function createZeroMessage(desc) {
  let msg;
  if (!needsPrototypeChain(desc)) {
    msg = {
      $typeName: desc.typeName
    };
    for (const member of desc.members) {
      if (member.kind == "oneof" || member.presence == IMPLICIT2) {
        msg[member.localName] = createZeroField(member);
      }
    }
  } else {
    const cached = messagePrototypes.get(desc);
    let prototype;
    let members;
    if (cached) {
      ({ prototype, members } = cached);
    } else {
      prototype = {};
      members = new Set;
      for (const member of desc.members) {
        if (member.kind == "oneof") {
          continue;
        }
        if (member.fieldKind != "scalar" && member.fieldKind != "enum") {
          continue;
        }
        if (member.presence == IMPLICIT2) {
          continue;
        }
        members.add(member);
        prototype[member.localName] = createZeroField(member);
      }
      messagePrototypes.set(desc, { prototype, members });
    }
    msg = Object.create(prototype);
    msg.$typeName = desc.typeName;
    for (const member of desc.members) {
      if (members.has(member)) {
        continue;
      }
      if (member.kind == "field") {
        if (member.fieldKind == "message") {
          continue;
        }
        if (member.fieldKind == "scalar" || member.fieldKind == "enum") {
          if (member.presence != IMPLICIT2) {
            continue;
          }
        }
      }
      msg[member.localName] = createZeroField(member);
    }
  }
  return msg;
}
function needsPrototypeChain(desc) {
  switch (desc.file.edition) {
    case EDITION_PROTO3:
      return false;
    case EDITION_PROTO2:
      return true;
    default:
      return desc.fields.some((f) => f.presence != IMPLICIT2 && f.fieldKind != "message" && !f.oneof);
  }
}
function createZeroField(field) {
  if (field.kind == "oneof") {
    return { case: undefined };
  }
  if (field.fieldKind == "list") {
    return [];
  }
  if (field.fieldKind == "map") {
    return {};
  }
  if (field.fieldKind == "message") {
    return tokenZeroMessageField;
  }
  const defaultValue = field.getDefaultValue();
  if (defaultValue !== undefined) {
    return field.fieldKind == "scalar" && field.longAsString ? defaultValue.toString() : defaultValue;
  }
  return field.fieldKind == "scalar" ? scalarZeroValue(field.scalar, field.longAsString) : field.enum.values[0].number;
}
// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/error.js
class FieldError extends Error {
  constructor(fieldOrOneof, message, name = "FieldValueInvalidError") {
    super(message);
    this.name = name;
    this.field = () => fieldOrOneof;
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/wire/text-encoding.js
var symbol = Symbol.for("@bufbuild/protobuf/text-encoding");
function getTextEncoding() {
  if (globalThis[symbol] == undefined) {
    const te = new globalThis.TextEncoder;
    const td = new globalThis.TextDecoder;
    globalThis[symbol] = {
      encodeUtf8(text) {
        return te.encode(text);
      },
      decodeUtf8(bytes) {
        return td.decode(bytes);
      },
      checkUtf8(text) {
        try {
          encodeURIComponent(text);
          return true;
        } catch (_) {
          return false;
        }
      }
    };
  }
  return globalThis[symbol];
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/wire/binary-encoding.js
var WireType;
(function(WireType2) {
  WireType2[WireType2["Varint"] = 0] = "Varint";
  WireType2[WireType2["Bit64"] = 1] = "Bit64";
  WireType2[WireType2["LengthDelimited"] = 2] = "LengthDelimited";
  WireType2[WireType2["StartGroup"] = 3] = "StartGroup";
  WireType2[WireType2["EndGroup"] = 4] = "EndGroup";
  WireType2[WireType2["Bit32"] = 5] = "Bit32";
})(WireType || (WireType = {}));
var FLOAT32_MAX = 340282346638528860000000000000000000000;
var FLOAT32_MIN = -340282346638528860000000000000000000000;
var UINT32_MAX = 4294967295;
var INT32_MAX = 2147483647;
var INT32_MIN = -2147483648;

class BinaryWriter {
  constructor(encodeUtf8 = getTextEncoding().encodeUtf8) {
    this.encodeUtf8 = encodeUtf8;
    this.stack = [];
    this.chunks = [];
    this.buf = [];
  }
  finish() {
    if (this.buf.length) {
      this.chunks.push(new Uint8Array(this.buf));
      this.buf = [];
    }
    let len = 0;
    for (let i = 0;i < this.chunks.length; i++)
      len += this.chunks[i].length;
    let bytes = new Uint8Array(len);
    let offset = 0;
    for (let i = 0;i < this.chunks.length; i++) {
      bytes.set(this.chunks[i], offset);
      offset += this.chunks[i].length;
    }
    this.chunks = [];
    return bytes;
  }
  fork() {
    this.stack.push({ chunks: this.chunks, buf: this.buf });
    this.chunks = [];
    this.buf = [];
    return this;
  }
  join() {
    let chunk = this.finish();
    let prev = this.stack.pop();
    if (!prev)
      throw new Error("invalid state, fork stack empty");
    this.chunks = prev.chunks;
    this.buf = prev.buf;
    this.uint32(chunk.byteLength);
    return this.raw(chunk);
  }
  tag(fieldNo, type) {
    return this.uint32((fieldNo << 3 | type) >>> 0);
  }
  raw(chunk) {
    if (this.buf.length) {
      this.chunks.push(new Uint8Array(this.buf));
      this.buf = [];
    }
    this.chunks.push(chunk);
    return this;
  }
  uint32(value) {
    assertUInt32(value);
    while (value > 127) {
      this.buf.push(value & 127 | 128);
      value = value >>> 7;
    }
    this.buf.push(value);
    return this;
  }
  int32(value) {
    assertInt32(value);
    varint32write(value, this.buf);
    return this;
  }
  bool(value) {
    this.buf.push(value ? 1 : 0);
    return this;
  }
  bytes(value) {
    this.uint32(value.byteLength);
    return this.raw(value);
  }
  string(value) {
    let chunk = this.encodeUtf8(value);
    this.uint32(chunk.byteLength);
    return this.raw(chunk);
  }
  float(value) {
    assertFloat32(value);
    let chunk = new Uint8Array(4);
    new DataView(chunk.buffer).setFloat32(0, value, true);
    return this.raw(chunk);
  }
  double(value) {
    let chunk = new Uint8Array(8);
    new DataView(chunk.buffer).setFloat64(0, value, true);
    return this.raw(chunk);
  }
  fixed32(value) {
    assertUInt32(value);
    let chunk = new Uint8Array(4);
    new DataView(chunk.buffer).setUint32(0, value, true);
    return this.raw(chunk);
  }
  sfixed32(value) {
    assertInt32(value);
    let chunk = new Uint8Array(4);
    new DataView(chunk.buffer).setInt32(0, value, true);
    return this.raw(chunk);
  }
  sint32(value) {
    assertInt32(value);
    value = (value << 1 ^ value >> 31) >>> 0;
    varint32write(value, this.buf);
    return this;
  }
  sfixed64(value) {
    let chunk = new Uint8Array(8), view = new DataView(chunk.buffer), tc = protoInt64.enc(value);
    view.setInt32(0, tc.lo, true);
    view.setInt32(4, tc.hi, true);
    return this.raw(chunk);
  }
  fixed64(value) {
    let chunk = new Uint8Array(8), view = new DataView(chunk.buffer), tc = protoInt64.uEnc(value);
    view.setInt32(0, tc.lo, true);
    view.setInt32(4, tc.hi, true);
    return this.raw(chunk);
  }
  int64(value) {
    let tc = protoInt64.enc(value);
    varint64write(tc.lo, tc.hi, this.buf);
    return this;
  }
  sint64(value) {
    const tc = protoInt64.enc(value), sign = tc.hi >> 31, lo = tc.lo << 1 ^ sign, hi = (tc.hi << 1 | tc.lo >>> 31) ^ sign;
    varint64write(lo, hi, this.buf);
    return this;
  }
  uint64(value) {
    const tc = protoInt64.uEnc(value);
    varint64write(tc.lo, tc.hi, this.buf);
    return this;
  }
}

class BinaryReader {
  constructor(buf, decodeUtf8 = getTextEncoding().decodeUtf8) {
    this.decodeUtf8 = decodeUtf8;
    this.varint64 = varint64read;
    this.uint32 = varint32read;
    this.buf = buf;
    this.len = buf.length;
    this.pos = 0;
    this.view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  }
  tag() {
    let tag = this.uint32(), fieldNo = tag >>> 3, wireType = tag & 7;
    if (fieldNo <= 0 || wireType < 0 || wireType > 5)
      throw new Error("illegal tag: field no " + fieldNo + " wire type " + wireType);
    return [fieldNo, wireType];
  }
  skip(wireType, fieldNo) {
    let start = this.pos;
    switch (wireType) {
      case WireType.Varint:
        while (this.buf[this.pos++] & 128) {}
        break;
      case WireType.Bit64:
        this.pos += 4;
      case WireType.Bit32:
        this.pos += 4;
        break;
      case WireType.LengthDelimited:
        let len = this.uint32();
        this.pos += len;
        break;
      case WireType.StartGroup:
        for (;; ) {
          const [fn, wt] = this.tag();
          if (wt === WireType.EndGroup) {
            if (fieldNo !== undefined && fn !== fieldNo) {
              throw new Error("invalid end group tag");
            }
            break;
          }
          this.skip(wt, fn);
        }
        break;
      default:
        throw new Error("cant skip wire type " + wireType);
    }
    this.assertBounds();
    return this.buf.subarray(start, this.pos);
  }
  assertBounds() {
    if (this.pos > this.len)
      throw new RangeError("premature EOF");
  }
  int32() {
    return this.uint32() | 0;
  }
  sint32() {
    let zze = this.uint32();
    return zze >>> 1 ^ -(zze & 1);
  }
  int64() {
    return protoInt64.dec(...this.varint64());
  }
  uint64() {
    return protoInt64.uDec(...this.varint64());
  }
  sint64() {
    let [lo, hi] = this.varint64();
    let s = -(lo & 1);
    lo = (lo >>> 1 | (hi & 1) << 31) ^ s;
    hi = hi >>> 1 ^ s;
    return protoInt64.dec(lo, hi);
  }
  bool() {
    let [lo, hi] = this.varint64();
    return lo !== 0 || hi !== 0;
  }
  fixed32() {
    return this.view.getUint32((this.pos += 4) - 4, true);
  }
  sfixed32() {
    return this.view.getInt32((this.pos += 4) - 4, true);
  }
  fixed64() {
    return protoInt64.uDec(this.sfixed32(), this.sfixed32());
  }
  sfixed64() {
    return protoInt64.dec(this.sfixed32(), this.sfixed32());
  }
  float() {
    return this.view.getFloat32((this.pos += 4) - 4, true);
  }
  double() {
    return this.view.getFloat64((this.pos += 8) - 8, true);
  }
  bytes() {
    let len = this.uint32(), start = this.pos;
    this.pos += len;
    this.assertBounds();
    return this.buf.subarray(start, start + len);
  }
  string() {
    return this.decodeUtf8(this.bytes());
  }
}
function assertInt32(arg) {
  if (typeof arg == "string") {
    arg = Number(arg);
  } else if (typeof arg != "number") {
    throw new Error("invalid int32: " + typeof arg);
  }
  if (!Number.isInteger(arg) || arg > INT32_MAX || arg < INT32_MIN)
    throw new Error("invalid int32: " + arg);
}
function assertUInt32(arg) {
  if (typeof arg == "string") {
    arg = Number(arg);
  } else if (typeof arg != "number") {
    throw new Error("invalid uint32: " + typeof arg);
  }
  if (!Number.isInteger(arg) || arg > UINT32_MAX || arg < 0)
    throw new Error("invalid uint32: " + arg);
}
function assertFloat32(arg) {
  if (typeof arg == "string") {
    const o = arg;
    arg = Number(arg);
    if (Number.isNaN(arg) && o !== "NaN") {
      throw new Error("invalid float32: " + o);
    }
  } else if (typeof arg != "number") {
    throw new Error("invalid float32: " + typeof arg);
  }
  if (Number.isFinite(arg) && (arg > FLOAT32_MAX || arg < FLOAT32_MIN))
    throw new Error("invalid float32: " + arg);
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/reflect-check.js
function checkField(field, value) {
  const check = field.fieldKind == "list" ? isReflectList(value, field) : field.fieldKind == "map" ? isReflectMap(value, field) : checkSingular(field, value);
  if (check === true) {
    return;
  }
  let reason;
  switch (field.fieldKind) {
    case "list":
      reason = `expected ${formatReflectList(field)}, got ${formatVal(value)}`;
      break;
    case "map":
      reason = `expected ${formatReflectMap(field)}, got ${formatVal(value)}`;
      break;
    default: {
      reason = reasonSingular(field, value, check);
    }
  }
  return new FieldError(field, reason);
}
function checkListItem(field, index, value) {
  const check = checkSingular(field, value);
  if (check !== true) {
    return new FieldError(field, `list item #${index + 1}: ${reasonSingular(field, value, check)}`);
  }
  return;
}
function checkMapEntry(field, key, value) {
  const checkKey = checkScalarValue(key, field.mapKey);
  if (checkKey !== true) {
    return new FieldError(field, `invalid map key: ${reasonSingular({ scalar: field.mapKey }, key, checkKey)}`);
  }
  const checkVal = checkSingular(field, value);
  if (checkVal !== true) {
    return new FieldError(field, `map entry ${formatVal(key)}: ${reasonSingular(field, value, checkVal)}`);
  }
  return;
}
function checkSingular(field, value) {
  if (field.scalar !== undefined) {
    return checkScalarValue(value, field.scalar);
  }
  if (field.enum !== undefined) {
    if (field.enum.open) {
      return Number.isInteger(value);
    }
    return field.enum.values.some((v) => v.number === value);
  }
  return isReflectMessage(value, field.message);
}
function checkScalarValue(value, scalar) {
  switch (scalar) {
    case ScalarType.DOUBLE:
      return typeof value == "number";
    case ScalarType.FLOAT:
      if (typeof value != "number") {
        return false;
      }
      if (Number.isNaN(value) || !Number.isFinite(value)) {
        return true;
      }
      if (value > FLOAT32_MAX || value < FLOAT32_MIN) {
        return `${value.toFixed()} out of range`;
      }
      return true;
    case ScalarType.INT32:
    case ScalarType.SFIXED32:
    case ScalarType.SINT32:
      if (typeof value !== "number" || !Number.isInteger(value)) {
        return false;
      }
      if (value > INT32_MAX || value < INT32_MIN) {
        return `${value.toFixed()} out of range`;
      }
      return true;
    case ScalarType.FIXED32:
    case ScalarType.UINT32:
      if (typeof value !== "number" || !Number.isInteger(value)) {
        return false;
      }
      if (value > UINT32_MAX || value < 0) {
        return `${value.toFixed()} out of range`;
      }
      return true;
    case ScalarType.BOOL:
      return typeof value == "boolean";
    case ScalarType.STRING:
      if (typeof value != "string") {
        return false;
      }
      return getTextEncoding().checkUtf8(value) || "invalid UTF8";
    case ScalarType.BYTES:
      return value instanceof Uint8Array;
    case ScalarType.INT64:
    case ScalarType.SFIXED64:
    case ScalarType.SINT64:
      if (typeof value == "bigint" || typeof value == "number" || typeof value == "string" && value.length > 0) {
        try {
          protoInt64.parse(value);
          return true;
        } catch (_) {
          return `${value} out of range`;
        }
      }
      return false;
    case ScalarType.FIXED64:
    case ScalarType.UINT64:
      if (typeof value == "bigint" || typeof value == "number" || typeof value == "string" && value.length > 0) {
        try {
          protoInt64.uParse(value);
          return true;
        } catch (_) {
          return `${value} out of range`;
        }
      }
      return false;
  }
}
function reasonSingular(field, val, details) {
  details = typeof details == "string" ? `: ${details}` : `, got ${formatVal(val)}`;
  if (field.scalar !== undefined) {
    return `expected ${scalarTypeDescription(field.scalar)}` + details;
  }
  if (field.enum !== undefined) {
    return `expected ${field.enum.toString()}` + details;
  }
  return `expected ${formatReflectMessage(field.message)}` + details;
}
function formatVal(val) {
  switch (typeof val) {
    case "object":
      if (val === null) {
        return "null";
      }
      if (val instanceof Uint8Array) {
        return `Uint8Array(${val.length})`;
      }
      if (Array.isArray(val)) {
        return `Array(${val.length})`;
      }
      if (isReflectList(val)) {
        return formatReflectList(val.field());
      }
      if (isReflectMap(val)) {
        return formatReflectMap(val.field());
      }
      if (isReflectMessage(val)) {
        return formatReflectMessage(val.desc);
      }
      if (isMessage(val)) {
        return `message ${val.$typeName}`;
      }
      return "object";
    case "string":
      return val.length > 30 ? "string" : `"${val.split('"').join("\\\"")}"`;
    case "boolean":
      return String(val);
    case "number":
      return String(val);
    case "bigint":
      return String(val) + "n";
    default:
      return typeof val;
  }
}
function formatReflectMessage(desc) {
  return `ReflectMessage (${desc.typeName})`;
}
function formatReflectList(field) {
  switch (field.listKind) {
    case "message":
      return `ReflectList (${field.message.toString()})`;
    case "enum":
      return `ReflectList (${field.enum.toString()})`;
    case "scalar":
      return `ReflectList (${ScalarType[field.scalar]})`;
  }
}
function formatReflectMap(field) {
  switch (field.mapKind) {
    case "message":
      return `ReflectMap (${ScalarType[field.mapKey]}, ${field.message.toString()})`;
    case "enum":
      return `ReflectMap (${ScalarType[field.mapKey]}, ${field.enum.toString()})`;
    case "scalar":
      return `ReflectMap (${ScalarType[field.mapKey]}, ${ScalarType[field.scalar]})`;
  }
}
function scalarTypeDescription(scalar) {
  switch (scalar) {
    case ScalarType.STRING:
      return "string";
    case ScalarType.BOOL:
      return "boolean";
    case ScalarType.INT64:
    case ScalarType.SINT64:
    case ScalarType.SFIXED64:
      return "bigint (int64)";
    case ScalarType.UINT64:
    case ScalarType.FIXED64:
      return "bigint (uint64)";
    case ScalarType.BYTES:
      return "Uint8Array";
    case ScalarType.DOUBLE:
      return "number (float64)";
    case ScalarType.FLOAT:
      return "number (float32)";
    case ScalarType.FIXED32:
    case ScalarType.UINT32:
      return "number (uint32)";
    case ScalarType.INT32:
    case ScalarType.SFIXED32:
    case ScalarType.SINT32:
      return "number (int32)";
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/reflect.js
function reflect(messageDesc, message, check = true) {
  return new ReflectMessageImpl(messageDesc, message, check);
}
var messageSortedFields = new WeakMap;

class ReflectMessageImpl {
  get sortedFields() {
    const cached = messageSortedFields.get(this.desc);
    if (cached) {
      return cached;
    }
    const sortedFields = this.desc.fields.concat().sort((a, b) => a.number - b.number);
    messageSortedFields.set(this.desc, sortedFields);
    return sortedFields;
  }
  constructor(messageDesc, message, check = true) {
    this.lists = new Map;
    this.maps = new Map;
    this.check = check;
    this.desc = messageDesc;
    this.message = this[unsafeLocal] = message !== null && message !== undefined ? message : create(messageDesc);
    this.fields = messageDesc.fields;
    this.oneofs = messageDesc.oneofs;
    this.members = messageDesc.members;
  }
  findNumber(number) {
    if (!this._fieldsByNumber) {
      this._fieldsByNumber = new Map(this.desc.fields.map((f) => [f.number, f]));
    }
    return this._fieldsByNumber.get(number);
  }
  oneofCase(oneof) {
    assertOwn(this.message, oneof);
    return unsafeOneofCase(this.message, oneof);
  }
  isSet(field) {
    assertOwn(this.message, field);
    return unsafeIsSet(this.message, field);
  }
  clear(field) {
    assertOwn(this.message, field);
    unsafeClear(this.message, field);
  }
  get(field) {
    assertOwn(this.message, field);
    const value = unsafeGet(this.message, field);
    switch (field.fieldKind) {
      case "list":
        let list = this.lists.get(field);
        if (!list || list[unsafeLocal] !== value) {
          this.lists.set(field, list = new ReflectListImpl(field, value, this.check));
        }
        return list;
      case "map":
        let map = this.maps.get(field);
        if (!map || map[unsafeLocal] !== value) {
          this.maps.set(field, map = new ReflectMapImpl(field, value, this.check));
        }
        return map;
      case "message":
        return messageToReflect(field, value, this.check);
      case "scalar":
        return value === undefined ? scalarZeroValue(field.scalar, false) : longToReflect(field, value);
      case "enum":
        return value !== null && value !== undefined ? value : field.enum.values[0].number;
    }
  }
  set(field, value) {
    assertOwn(this.message, field);
    if (this.check) {
      const err = checkField(field, value);
      if (err) {
        throw err;
      }
    }
    let local;
    if (field.fieldKind == "message") {
      local = messageToLocal(field, value);
    } else if (isReflectMap(value) || isReflectList(value)) {
      local = value[unsafeLocal];
    } else {
      local = longToLocal(field, value);
    }
    unsafeSet(this.message, field, local);
  }
  getUnknown() {
    return this.message.$unknown;
  }
  setUnknown(value) {
    this.message.$unknown = value;
  }
}
function assertOwn(owner, member) {
  if (member.parent.typeName !== owner.$typeName) {
    throw new FieldError(member, `cannot use ${member.toString()} with message ${owner.$typeName}`, "ForeignFieldError");
  }
}
class ReflectListImpl {
  field() {
    return this._field;
  }
  get size() {
    return this._arr.length;
  }
  constructor(field, unsafeInput, check) {
    this._field = field;
    this._arr = this[unsafeLocal] = unsafeInput;
    this.check = check;
  }
  get(index) {
    const item = this._arr[index];
    return item === undefined ? undefined : listItemToReflect(this._field, item, this.check);
  }
  set(index, item) {
    if (index < 0 || index >= this._arr.length) {
      throw new FieldError(this._field, `list item #${index + 1}: out of range`);
    }
    if (this.check) {
      const err = checkListItem(this._field, index, item);
      if (err) {
        throw err;
      }
    }
    this._arr[index] = listItemToLocal(this._field, item);
  }
  add(item) {
    if (this.check) {
      const err = checkListItem(this._field, this._arr.length, item);
      if (err) {
        throw err;
      }
    }
    this._arr.push(listItemToLocal(this._field, item));
    return;
  }
  clear() {
    this._arr.splice(0, this._arr.length);
  }
  [Symbol.iterator]() {
    return this.values();
  }
  keys() {
    return this._arr.keys();
  }
  *values() {
    for (const item of this._arr) {
      yield listItemToReflect(this._field, item, this.check);
    }
  }
  *entries() {
    for (let i = 0;i < this._arr.length; i++) {
      yield [i, listItemToReflect(this._field, this._arr[i], this.check)];
    }
  }
}
class ReflectMapImpl {
  constructor(field, unsafeInput, check = true) {
    this.obj = this[unsafeLocal] = unsafeInput !== null && unsafeInput !== undefined ? unsafeInput : {};
    this.check = check;
    this._field = field;
  }
  field() {
    return this._field;
  }
  set(key, value) {
    if (this.check) {
      const err = checkMapEntry(this._field, key, value);
      if (err) {
        throw err;
      }
    }
    this.obj[mapKeyToLocal(key)] = mapValueToLocal(this._field, value);
    return this;
  }
  delete(key) {
    const k = mapKeyToLocal(key);
    const has = Object.prototype.hasOwnProperty.call(this.obj, k);
    if (has) {
      delete this.obj[k];
    }
    return has;
  }
  clear() {
    for (const key of Object.keys(this.obj)) {
      delete this.obj[key];
    }
  }
  get(key) {
    let val = this.obj[mapKeyToLocal(key)];
    if (val !== undefined) {
      val = mapValueToReflect(this._field, val, this.check);
    }
    return val;
  }
  has(key) {
    return Object.prototype.hasOwnProperty.call(this.obj, mapKeyToLocal(key));
  }
  *keys() {
    for (const objKey of Object.keys(this.obj)) {
      yield mapKeyToReflect(objKey, this._field.mapKey);
    }
  }
  *entries() {
    for (const objEntry of Object.entries(this.obj)) {
      yield [
        mapKeyToReflect(objEntry[0], this._field.mapKey),
        mapValueToReflect(this._field, objEntry[1], this.check)
      ];
    }
  }
  [Symbol.iterator]() {
    return this.entries();
  }
  get size() {
    return Object.keys(this.obj).length;
  }
  *values() {
    for (const val of Object.values(this.obj)) {
      yield mapValueToReflect(this._field, val, this.check);
    }
  }
  forEach(callbackfn, thisArg) {
    for (const mapEntry of this.entries()) {
      callbackfn.call(thisArg, mapEntry[1], mapEntry[0], this);
    }
  }
}
function messageToLocal(field, value) {
  if (!isReflectMessage(value)) {
    return value;
  }
  if (isWrapper(value.message) && !field.oneof && field.fieldKind == "message") {
    return value.message.value;
  }
  if (value.desc.typeName == "google.protobuf.Struct" && field.parent.typeName != "google.protobuf.Value") {
    return wktStructToLocal(value.message);
  }
  return value.message;
}
function messageToReflect(field, value, check) {
  if (value !== undefined) {
    if (isWrapperDesc(field.message) && !field.oneof && field.fieldKind == "message") {
      value = {
        $typeName: field.message.typeName,
        value: longToReflect(field.message.fields[0], value)
      };
    } else if (field.message.typeName == "google.protobuf.Struct" && field.parent.typeName != "google.protobuf.Value" && isObject(value)) {
      value = wktStructToReflect(value);
    }
  }
  return new ReflectMessageImpl(field.message, value, check);
}
function listItemToLocal(field, value) {
  if (field.listKind == "message") {
    return messageToLocal(field, value);
  }
  return longToLocal(field, value);
}
function listItemToReflect(field, value, check) {
  if (field.listKind == "message") {
    return messageToReflect(field, value, check);
  }
  return longToReflect(field, value);
}
function mapValueToLocal(field, value) {
  if (field.mapKind == "message") {
    return messageToLocal(field, value);
  }
  return longToLocal(field, value);
}
function mapValueToReflect(field, value, check) {
  if (field.mapKind == "message") {
    return messageToReflect(field, value, check);
  }
  return value;
}
function mapKeyToLocal(key) {
  return typeof key == "string" || typeof key == "number" ? key : String(key);
}
function mapKeyToReflect(key, type) {
  switch (type) {
    case ScalarType.STRING:
      return key;
    case ScalarType.INT32:
    case ScalarType.FIXED32:
    case ScalarType.UINT32:
    case ScalarType.SFIXED32:
    case ScalarType.SINT32: {
      const n = Number.parseInt(key);
      if (Number.isFinite(n)) {
        return n;
      }
      break;
    }
    case ScalarType.BOOL:
      switch (key) {
        case "true":
          return true;
        case "false":
          return false;
      }
      break;
    case ScalarType.UINT64:
    case ScalarType.FIXED64:
      try {
        return protoInt64.uParse(key);
      } catch (_a) {}
      break;
    default:
      try {
        return protoInt64.parse(key);
      } catch (_b) {}
      break;
  }
  return key;
}
function longToReflect(field, value) {
  switch (field.scalar) {
    case ScalarType.INT64:
    case ScalarType.SFIXED64:
    case ScalarType.SINT64:
      if ("longAsString" in field && field.longAsString && typeof value == "string") {
        value = protoInt64.parse(value);
      }
      break;
    case ScalarType.FIXED64:
    case ScalarType.UINT64:
      if ("longAsString" in field && field.longAsString && typeof value == "string") {
        value = protoInt64.uParse(value);
      }
      break;
  }
  return value;
}
function longToLocal(field, value) {
  switch (field.scalar) {
    case ScalarType.INT64:
    case ScalarType.SFIXED64:
    case ScalarType.SINT64:
      if ("longAsString" in field && field.longAsString) {
        value = String(value);
      } else if (typeof value == "string" || typeof value == "number") {
        value = protoInt64.parse(value);
      }
      break;
    case ScalarType.FIXED64:
    case ScalarType.UINT64:
      if ("longAsString" in field && field.longAsString) {
        value = String(value);
      } else if (typeof value == "string" || typeof value == "number") {
        value = protoInt64.uParse(value);
      }
      break;
  }
  return value;
}
function wktStructToReflect(json) {
  const struct = {
    $typeName: "google.protobuf.Struct",
    fields: {}
  };
  if (isObject(json)) {
    for (const [k, v] of Object.entries(json)) {
      struct.fields[k] = wktValueToReflect(v);
    }
  }
  return struct;
}
function wktStructToLocal(val) {
  const json = {};
  for (const [k, v] of Object.entries(val.fields)) {
    json[k] = wktValueToLocal(v);
  }
  return json;
}
function wktValueToLocal(val) {
  switch (val.kind.case) {
    case "structValue":
      return wktStructToLocal(val.kind.value);
    case "listValue":
      return val.kind.value.values.map(wktValueToLocal);
    case "nullValue":
    case undefined:
      return null;
    default:
      return val.kind.value;
  }
}
function wktValueToReflect(json) {
  const value = {
    $typeName: "google.protobuf.Value",
    kind: { case: undefined }
  };
  switch (typeof json) {
    case "number":
      value.kind = { case: "numberValue", value: json };
      break;
    case "string":
      value.kind = { case: "stringValue", value: json };
      break;
    case "boolean":
      value.kind = { case: "boolValue", value: json };
      break;
    case "object":
      if (json === null) {
        const nullValue = 0;
        value.kind = { case: "nullValue", value: nullValue };
      } else if (Array.isArray(json)) {
        const listValue = {
          $typeName: "google.protobuf.ListValue",
          values: []
        };
        if (Array.isArray(json)) {
          for (const e of json) {
            listValue.values.push(wktValueToReflect(e));
          }
        }
        value.kind = {
          case: "listValue",
          value: listValue
        };
      } else {
        value.kind = {
          case: "structValue",
          value: wktStructToReflect(json)
        };
      }
      break;
  }
  return value;
}
// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/wire/base64-encoding.js
function base64Decode(base64Str) {
  const table = getDecodeTable();
  let es = base64Str.length * 3 / 4;
  if (base64Str[base64Str.length - 2] == "=")
    es -= 2;
  else if (base64Str[base64Str.length - 1] == "=")
    es -= 1;
  let bytes = new Uint8Array(es), bytePos = 0, groupPos = 0, b, p = 0;
  for (let i = 0;i < base64Str.length; i++) {
    b = table[base64Str.charCodeAt(i)];
    if (b === undefined) {
      switch (base64Str[i]) {
        case "=":
          groupPos = 0;
        case `
`:
        case "\r":
        case "\t":
        case " ":
          continue;
        default:
          throw Error("invalid base64 string");
      }
    }
    switch (groupPos) {
      case 0:
        p = b;
        groupPos = 1;
        break;
      case 1:
        bytes[bytePos++] = p << 2 | (b & 48) >> 4;
        p = b;
        groupPos = 2;
        break;
      case 2:
        bytes[bytePos++] = (p & 15) << 4 | (b & 60) >> 2;
        p = b;
        groupPos = 3;
        break;
      case 3:
        bytes[bytePos++] = (p & 3) << 6 | b;
        groupPos = 0;
        break;
    }
  }
  if (groupPos == 1)
    throw Error("invalid base64 string");
  return bytes.subarray(0, bytePos);
}
function base64Encode(bytes, encoding = "std") {
  const table = getEncodeTable(encoding);
  const pad = encoding == "std";
  let base64 = "", groupPos = 0, b, p = 0;
  for (let i = 0;i < bytes.length; i++) {
    b = bytes[i];
    switch (groupPos) {
      case 0:
        base64 += table[b >> 2];
        p = (b & 3) << 4;
        groupPos = 1;
        break;
      case 1:
        base64 += table[p | b >> 4];
        p = (b & 15) << 2;
        groupPos = 2;
        break;
      case 2:
        base64 += table[p | b >> 6];
        base64 += table[b & 63];
        groupPos = 0;
        break;
    }
  }
  if (groupPos) {
    base64 += table[p];
    if (pad) {
      base64 += "=";
      if (groupPos == 1)
        base64 += "=";
    }
  }
  return base64;
}
var encodeTableStd;
var encodeTableUrl;
var decodeTable;
function getEncodeTable(encoding) {
  if (!encodeTableStd) {
    encodeTableStd = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/".split("");
    encodeTableUrl = encodeTableStd.slice(0, -2).concat("-", "_");
  }
  return encoding == "url" ? encodeTableUrl : encodeTableStd;
}
function getDecodeTable() {
  if (!decodeTable) {
    decodeTable = [];
    const encodeTable = getEncodeTable("std");
    for (let i = 0;i < encodeTable.length; i++)
      decodeTable[encodeTable[i].charCodeAt(0)] = i;
    decodeTable[45] = encodeTable.indexOf("+");
    decodeTable[95] = encodeTable.indexOf("/");
  }
  return decodeTable;
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/names.js
function protoCamelCase(snakeCase) {
  let capNext = false;
  const b = [];
  for (let i = 0;i < snakeCase.length; i++) {
    let c = snakeCase.charAt(i);
    switch (c) {
      case "_":
        capNext = true;
        break;
      case "0":
      case "1":
      case "2":
      case "3":
      case "4":
      case "5":
      case "6":
      case "7":
      case "8":
      case "9":
        b.push(c);
        capNext = false;
        break;
      default:
        if (capNext) {
          capNext = false;
          c = c.toUpperCase();
        }
        b.push(c);
        break;
    }
  }
  return b.join("");
}
function protoSnakeCase(lowerCamelCase) {
  return lowerCamelCase.replace(/[A-Z]/g, (letter) => "_" + letter.toLowerCase());
}
var reservedObjectProperties = new Set([
  "constructor",
  "toString",
  "toJSON",
  "valueOf"
]);
function safeObjectProperty(name) {
  return reservedObjectProperties.has(name) ? name + "$" : name;
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/codegenv2/restore-json-names.js
function restoreJsonNames(message) {
  for (const f of message.field) {
    if (!unsafeIsSetExplicit(f, "jsonName")) {
      f.jsonName = protoCamelCase(f.name);
    }
  }
  message.nestedType.forEach(restoreJsonNames);
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/wire/text-format.js
function parseTextFormatEnumValue(descEnum, value) {
  const enumValue = descEnum.values.find((v) => v.name === value);
  if (!enumValue) {
    throw new Error(`cannot parse ${descEnum} default value: ${value}`);
  }
  return enumValue.number;
}
function parseTextFormatScalarValue(type, value) {
  switch (type) {
    case ScalarType.STRING:
      return value;
    case ScalarType.BYTES: {
      const u = unescapeBytesDefaultValue(value);
      if (u === false) {
        throw new Error(`cannot parse ${ScalarType[type]} default value: ${value}`);
      }
      return u;
    }
    case ScalarType.INT64:
    case ScalarType.SFIXED64:
    case ScalarType.SINT64:
      return protoInt64.parse(value);
    case ScalarType.UINT64:
    case ScalarType.FIXED64:
      return protoInt64.uParse(value);
    case ScalarType.DOUBLE:
    case ScalarType.FLOAT:
      switch (value) {
        case "inf":
          return Number.POSITIVE_INFINITY;
        case "-inf":
          return Number.NEGATIVE_INFINITY;
        case "nan":
          return Number.NaN;
        default:
          return parseFloat(value);
      }
    case ScalarType.BOOL:
      return value === "true";
    case ScalarType.INT32:
    case ScalarType.UINT32:
    case ScalarType.SINT32:
    case ScalarType.FIXED32:
    case ScalarType.SFIXED32:
      return parseInt(value, 10);
  }
}
function unescapeBytesDefaultValue(str) {
  const b = [];
  const input = {
    tail: str,
    c: "",
    next() {
      if (this.tail.length == 0) {
        return false;
      }
      this.c = this.tail[0];
      this.tail = this.tail.substring(1);
      return true;
    },
    take(n) {
      if (this.tail.length >= n) {
        const r = this.tail.substring(0, n);
        this.tail = this.tail.substring(n);
        return r;
      }
      return false;
    }
  };
  while (input.next()) {
    switch (input.c) {
      case "\\":
        if (input.next()) {
          switch (input.c) {
            case "\\":
              b.push(input.c.charCodeAt(0));
              break;
            case "b":
              b.push(8);
              break;
            case "f":
              b.push(12);
              break;
            case "n":
              b.push(10);
              break;
            case "r":
              b.push(13);
              break;
            case "t":
              b.push(9);
              break;
            case "v":
              b.push(11);
              break;
            case "0":
            case "1":
            case "2":
            case "3":
            case "4":
            case "5":
            case "6":
            case "7": {
              const s = input.c;
              const t = input.take(2);
              if (t === false) {
                return false;
              }
              const n = parseInt(s + t, 8);
              if (Number.isNaN(n)) {
                return false;
              }
              b.push(n);
              break;
            }
            case "x": {
              const s = input.c;
              const t = input.take(2);
              if (t === false) {
                return false;
              }
              const n = parseInt(s + t, 16);
              if (Number.isNaN(n)) {
                return false;
              }
              b.push(n);
              break;
            }
            case "u": {
              const s = input.c;
              const t = input.take(4);
              if (t === false) {
                return false;
              }
              const n = parseInt(s + t, 16);
              if (Number.isNaN(n)) {
                return false;
              }
              const chunk = new Uint8Array(4);
              const view = new DataView(chunk.buffer);
              view.setInt32(0, n, true);
              b.push(chunk[0], chunk[1], chunk[2], chunk[3]);
              break;
            }
            case "U": {
              const s = input.c;
              const t = input.take(8);
              if (t === false) {
                return false;
              }
              const tc = protoInt64.uEnc(s + t);
              const chunk = new Uint8Array(8);
              const view = new DataView(chunk.buffer);
              view.setInt32(0, tc.lo, true);
              view.setInt32(4, tc.hi, true);
              b.push(chunk[0], chunk[1], chunk[2], chunk[3], chunk[4], chunk[5], chunk[6], chunk[7]);
              break;
            }
          }
        }
        break;
      default:
        b.push(input.c.charCodeAt(0));
    }
  }
  return new Uint8Array(b);
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/nested-types.js
function* nestedTypes(desc) {
  switch (desc.kind) {
    case "file":
      for (const message of desc.messages) {
        yield message;
        yield* nestedTypes(message);
      }
      yield* desc.enums;
      yield* desc.services;
      yield* desc.extensions;
      break;
    case "message":
      for (const message of desc.nestedMessages) {
        yield message;
        yield* nestedTypes(message);
      }
      yield* desc.nestedEnums;
      yield* desc.nestedExtensions;
      break;
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/registry.js
function createFileRegistry(...args) {
  const registry = createBaseRegistry();
  if (!args.length) {
    return registry;
  }
  if ("$typeName" in args[0] && args[0].$typeName == "google.protobuf.FileDescriptorSet") {
    for (const file of args[0].file) {
      addFile(file, registry);
    }
    return registry;
  }
  if ("$typeName" in args[0]) {
    let recurseDeps = function(file) {
      const deps = [];
      for (const protoFileName of file.dependency) {
        if (registry.getFile(protoFileName) != null) {
          continue;
        }
        if (seen.has(protoFileName)) {
          continue;
        }
        const dep = resolve(protoFileName);
        if (!dep) {
          throw new Error(`Unable to resolve ${protoFileName}, imported by ${file.name}`);
        }
        if ("kind" in dep) {
          registry.addFile(dep, false, true);
        } else {
          seen.add(dep.name);
          deps.push(dep);
        }
      }
      return deps.concat(...deps.map(recurseDeps));
    };
    const input = args[0];
    const resolve = args[1];
    const seen = new Set;
    for (const file of [input, ...recurseDeps(input)].reverse()) {
      addFile(file, registry);
    }
  } else {
    for (const fileReg of args) {
      for (const file of fileReg.files) {
        registry.addFile(file);
      }
    }
  }
  return registry;
}
function createBaseRegistry() {
  const types = new Map;
  const extendees = new Map;
  const files = new Map;
  return {
    kind: "registry",
    types,
    extendees,
    [Symbol.iterator]() {
      return types.values();
    },
    get files() {
      return files.values();
    },
    addFile(file, skipTypes, withDeps) {
      files.set(file.proto.name, file);
      if (!skipTypes) {
        for (const type of nestedTypes(file)) {
          this.add(type);
        }
      }
      if (withDeps) {
        for (const f of file.dependencies) {
          this.addFile(f, skipTypes, withDeps);
        }
      }
    },
    add(desc) {
      if (desc.kind == "extension") {
        let numberToExt = extendees.get(desc.extendee.typeName);
        if (!numberToExt) {
          extendees.set(desc.extendee.typeName, numberToExt = new Map);
        }
        numberToExt.set(desc.number, desc);
      }
      types.set(desc.typeName, desc);
    },
    get(typeName) {
      return types.get(typeName);
    },
    getFile(fileName) {
      return files.get(fileName);
    },
    getMessage(typeName) {
      const t = types.get(typeName);
      return (t === null || t === undefined ? undefined : t.kind) == "message" ? t : undefined;
    },
    getEnum(typeName) {
      const t = types.get(typeName);
      return (t === null || t === undefined ? undefined : t.kind) == "enum" ? t : undefined;
    },
    getExtension(typeName) {
      const t = types.get(typeName);
      return (t === null || t === undefined ? undefined : t.kind) == "extension" ? t : undefined;
    },
    getExtensionFor(extendee, no) {
      var _a;
      return (_a = extendees.get(extendee.typeName)) === null || _a === undefined ? undefined : _a.get(no);
    },
    getService(typeName) {
      const t = types.get(typeName);
      return (t === null || t === undefined ? undefined : t.kind) == "service" ? t : undefined;
    }
  };
}
var EDITION_PROTO22 = 998;
var EDITION_PROTO32 = 999;
var TYPE_STRING = 9;
var TYPE_GROUP = 10;
var TYPE_MESSAGE = 11;
var TYPE_BYTES = 12;
var TYPE_ENUM = 14;
var LABEL_REPEATED = 3;
var LABEL_REQUIRED = 2;
var JS_STRING = 1;
var IDEMPOTENCY_UNKNOWN = 0;
var EXPLICIT = 1;
var IMPLICIT3 = 2;
var LEGACY_REQUIRED = 3;
var PACKED = 1;
var DELIMITED = 2;
var OPEN = 1;
var featureDefaults = {
  998: {
    fieldPresence: 1,
    enumType: 2,
    repeatedFieldEncoding: 2,
    utf8Validation: 3,
    messageEncoding: 1,
    jsonFormat: 2,
    enforceNamingStyle: 2,
    defaultSymbolVisibility: 1
  },
  999: {
    fieldPresence: 2,
    enumType: 1,
    repeatedFieldEncoding: 1,
    utf8Validation: 2,
    messageEncoding: 1,
    jsonFormat: 1,
    enforceNamingStyle: 2,
    defaultSymbolVisibility: 1
  },
  1000: {
    fieldPresence: 1,
    enumType: 1,
    repeatedFieldEncoding: 1,
    utf8Validation: 2,
    messageEncoding: 1,
    jsonFormat: 1,
    enforceNamingStyle: 2,
    defaultSymbolVisibility: 1
  },
  1001: {
    fieldPresence: 1,
    enumType: 1,
    repeatedFieldEncoding: 1,
    utf8Validation: 2,
    messageEncoding: 1,
    jsonFormat: 1,
    enforceNamingStyle: 1,
    defaultSymbolVisibility: 2
  }
};
function addFile(proto, reg) {
  var _a, _b;
  const file = {
    kind: "file",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === undefined ? undefined : _a.deprecated) !== null && _b !== undefined ? _b : false,
    edition: getFileEdition(proto),
    name: proto.name.replace(/\.proto$/, ""),
    dependencies: findFileDependencies(proto, reg),
    enums: [],
    messages: [],
    extensions: [],
    services: [],
    toString() {
      return `file ${proto.name}`;
    }
  };
  const mapEntriesStore = new Map;
  const mapEntries = {
    get(typeName) {
      return mapEntriesStore.get(typeName);
    },
    add(desc) {
      var _a2;
      assert(((_a2 = desc.proto.options) === null || _a2 === undefined ? undefined : _a2.mapEntry) === true);
      mapEntriesStore.set(desc.typeName, desc);
    }
  };
  for (const enumProto of proto.enumType) {
    addEnum(enumProto, file, undefined, reg);
  }
  for (const messageProto of proto.messageType) {
    addMessage(messageProto, file, undefined, reg, mapEntries);
  }
  for (const serviceProto of proto.service) {
    addService(serviceProto, file, reg);
  }
  addExtensions(file, reg);
  for (const mapEntry of mapEntriesStore.values()) {
    addFields(mapEntry, reg, mapEntries);
  }
  for (const message of file.messages) {
    addFields(message, reg, mapEntries);
    addExtensions(message, reg);
  }
  reg.addFile(file, true);
}
function addExtensions(desc, reg) {
  switch (desc.kind) {
    case "file":
      for (const proto of desc.proto.extension) {
        const ext = newField(proto, desc, reg);
        desc.extensions.push(ext);
        reg.add(ext);
      }
      break;
    case "message":
      for (const proto of desc.proto.extension) {
        const ext = newField(proto, desc, reg);
        desc.nestedExtensions.push(ext);
        reg.add(ext);
      }
      for (const message of desc.nestedMessages) {
        addExtensions(message, reg);
      }
      break;
  }
}
function addFields(message, reg, mapEntries) {
  const allOneofs = message.proto.oneofDecl.map((proto) => newOneof(proto, message));
  const oneofsSeen = new Set;
  for (const proto of message.proto.field) {
    const oneof = findOneof(proto, allOneofs);
    const field = newField(proto, message, reg, oneof, mapEntries);
    message.fields.push(field);
    message.field[field.localName] = field;
    if (oneof === undefined) {
      message.members.push(field);
    } else {
      oneof.fields.push(field);
      if (!oneofsSeen.has(oneof)) {
        oneofsSeen.add(oneof);
        message.members.push(oneof);
      }
    }
  }
  for (const oneof of allOneofs.filter((o) => oneofsSeen.has(o))) {
    message.oneofs.push(oneof);
  }
  for (const child of message.nestedMessages) {
    addFields(child, reg, mapEntries);
  }
}
function addEnum(proto, file, parent, reg) {
  var _a, _b, _c, _d, _e;
  const sharedPrefix = findEnumSharedPrefix(proto.name, proto.value);
  const desc = {
    kind: "enum",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === undefined ? undefined : _a.deprecated) !== null && _b !== undefined ? _b : false,
    file,
    parent,
    open: true,
    name: proto.name,
    typeName: makeTypeName(proto, parent, file),
    value: {},
    values: [],
    sharedPrefix,
    toString() {
      return `enum ${this.typeName}`;
    }
  };
  desc.open = isEnumOpen(desc);
  reg.add(desc);
  for (const p of proto.value) {
    const name = p.name;
    desc.values.push(desc.value[p.number] = {
      kind: "enum_value",
      proto: p,
      deprecated: (_d = (_c = p.options) === null || _c === undefined ? undefined : _c.deprecated) !== null && _d !== undefined ? _d : false,
      parent: desc,
      name,
      localName: safeObjectProperty(sharedPrefix == undefined ? name : name.substring(sharedPrefix.length)),
      number: p.number,
      toString() {
        return `enum value ${desc.typeName}.${name}`;
      }
    });
  }
  ((_e = parent === null || parent === undefined ? undefined : parent.nestedEnums) !== null && _e !== undefined ? _e : file.enums).push(desc);
}
function addMessage(proto, file, parent, reg, mapEntries) {
  var _a, _b, _c, _d;
  const desc = {
    kind: "message",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === undefined ? undefined : _a.deprecated) !== null && _b !== undefined ? _b : false,
    file,
    parent,
    name: proto.name,
    typeName: makeTypeName(proto, parent, file),
    fields: [],
    field: {},
    oneofs: [],
    members: [],
    nestedEnums: [],
    nestedMessages: [],
    nestedExtensions: [],
    toString() {
      return `message ${this.typeName}`;
    }
  };
  if (((_c = proto.options) === null || _c === undefined ? undefined : _c.mapEntry) === true) {
    mapEntries.add(desc);
  } else {
    ((_d = parent === null || parent === undefined ? undefined : parent.nestedMessages) !== null && _d !== undefined ? _d : file.messages).push(desc);
    reg.add(desc);
  }
  for (const enumProto of proto.enumType) {
    addEnum(enumProto, file, desc, reg);
  }
  for (const messageProto of proto.nestedType) {
    addMessage(messageProto, file, desc, reg, mapEntries);
  }
}
function addService(proto, file, reg) {
  var _a, _b;
  const desc = {
    kind: "service",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === undefined ? undefined : _a.deprecated) !== null && _b !== undefined ? _b : false,
    file,
    name: proto.name,
    typeName: makeTypeName(proto, undefined, file),
    methods: [],
    method: {},
    toString() {
      return `service ${this.typeName}`;
    }
  };
  file.services.push(desc);
  reg.add(desc);
  for (const methodProto of proto.method) {
    const method = newMethod(methodProto, desc, reg);
    desc.methods.push(method);
    desc.method[method.localName] = method;
  }
}
function newMethod(proto, parent, reg) {
  var _a, _b, _c, _d;
  let methodKind;
  if (proto.clientStreaming && proto.serverStreaming) {
    methodKind = "bidi_streaming";
  } else if (proto.clientStreaming) {
    methodKind = "client_streaming";
  } else if (proto.serverStreaming) {
    methodKind = "server_streaming";
  } else {
    methodKind = "unary";
  }
  const input = reg.getMessage(trimLeadingDot(proto.inputType));
  const output = reg.getMessage(trimLeadingDot(proto.outputType));
  assert(input, `invalid MethodDescriptorProto: input_type ${proto.inputType} not found`);
  assert(output, `invalid MethodDescriptorProto: output_type ${proto.inputType} not found`);
  const name = proto.name;
  return {
    kind: "rpc",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === undefined ? undefined : _a.deprecated) !== null && _b !== undefined ? _b : false,
    parent,
    name,
    localName: safeObjectProperty(name.length ? safeObjectProperty(name[0].toLowerCase() + name.substring(1)) : name),
    methodKind,
    input,
    output,
    idempotency: (_d = (_c = proto.options) === null || _c === undefined ? undefined : _c.idempotencyLevel) !== null && _d !== undefined ? _d : IDEMPOTENCY_UNKNOWN,
    toString() {
      return `rpc ${parent.typeName}.${name}`;
    }
  };
}
function newOneof(proto, parent) {
  return {
    kind: "oneof",
    proto,
    deprecated: false,
    parent,
    fields: [],
    name: proto.name,
    localName: safeObjectProperty(protoCamelCase(proto.name)),
    toString() {
      return `oneof ${parent.typeName}.${this.name}`;
    }
  };
}
function newField(proto, parentOrFile, reg, oneof, mapEntries) {
  var _a, _b, _c;
  const isExtension = mapEntries === undefined;
  const field = {
    kind: "field",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === undefined ? undefined : _a.deprecated) !== null && _b !== undefined ? _b : false,
    name: proto.name,
    number: proto.number,
    scalar: undefined,
    message: undefined,
    enum: undefined,
    presence: getFieldPresence(proto, oneof, isExtension, parentOrFile),
    listKind: undefined,
    mapKind: undefined,
    mapKey: undefined,
    delimitedEncoding: undefined,
    packed: undefined,
    longAsString: false,
    getDefaultValue: undefined
  };
  if (isExtension) {
    const file = parentOrFile.kind == "file" ? parentOrFile : parentOrFile.file;
    const parent = parentOrFile.kind == "file" ? undefined : parentOrFile;
    const typeName = makeTypeName(proto, parent, file);
    field.kind = "extension";
    field.file = file;
    field.parent = parent;
    field.oneof = undefined;
    field.typeName = typeName;
    field.jsonName = `[${typeName}]`;
    field.toString = () => `extension ${typeName}`;
    const extendee = reg.getMessage(trimLeadingDot(proto.extendee));
    assert(extendee, `invalid FieldDescriptorProto: extendee ${proto.extendee} not found`);
    field.extendee = extendee;
  } else {
    const parent = parentOrFile;
    assert(parent.kind == "message");
    field.parent = parent;
    field.oneof = oneof;
    field.localName = oneof ? protoCamelCase(proto.name) : safeObjectProperty(protoCamelCase(proto.name));
    field.jsonName = proto.jsonName;
    field.toString = () => `field ${parent.typeName}.${proto.name}`;
  }
  const label = proto.label;
  const type = proto.type;
  const jstype = (_c = proto.options) === null || _c === undefined ? undefined : _c.jstype;
  if (label === LABEL_REPEATED) {
    const mapEntry = type == TYPE_MESSAGE ? mapEntries === null || mapEntries === undefined ? undefined : mapEntries.get(trimLeadingDot(proto.typeName)) : undefined;
    if (mapEntry) {
      field.fieldKind = "map";
      const { key, value } = findMapEntryFields(mapEntry);
      field.mapKey = key.scalar;
      field.mapKind = value.fieldKind;
      field.message = value.message;
      field.delimitedEncoding = false;
      field.enum = value.enum;
      field.scalar = value.scalar;
      return field;
    }
    field.fieldKind = "list";
    switch (type) {
      case TYPE_MESSAGE:
      case TYPE_GROUP:
        field.listKind = "message";
        field.message = reg.getMessage(trimLeadingDot(proto.typeName));
        assert(field.message);
        field.delimitedEncoding = isDelimitedEncoding(proto, parentOrFile);
        break;
      case TYPE_ENUM:
        field.listKind = "enum";
        field.enum = reg.getEnum(trimLeadingDot(proto.typeName));
        assert(field.enum);
        break;
      default:
        field.listKind = "scalar";
        field.scalar = type;
        field.longAsString = jstype == JS_STRING;
        break;
    }
    field.packed = isPackedField(proto, parentOrFile);
    return field;
  }
  switch (type) {
    case TYPE_MESSAGE:
    case TYPE_GROUP:
      field.fieldKind = "message";
      field.message = reg.getMessage(trimLeadingDot(proto.typeName));
      assert(field.message, `invalid FieldDescriptorProto: type_name ${proto.typeName} not found`);
      field.delimitedEncoding = isDelimitedEncoding(proto, parentOrFile);
      field.getDefaultValue = () => {
        return;
      };
      break;
    case TYPE_ENUM: {
      const enumeration = reg.getEnum(trimLeadingDot(proto.typeName));
      assert(enumeration !== undefined, `invalid FieldDescriptorProto: type_name ${proto.typeName} not found`);
      field.fieldKind = "enum";
      field.enum = reg.getEnum(trimLeadingDot(proto.typeName));
      field.getDefaultValue = () => {
        return unsafeIsSetExplicit(proto, "defaultValue") ? parseTextFormatEnumValue(enumeration, proto.defaultValue) : undefined;
      };
      break;
    }
    default: {
      field.fieldKind = "scalar";
      field.scalar = type;
      field.longAsString = jstype == JS_STRING;
      field.getDefaultValue = () => {
        return unsafeIsSetExplicit(proto, "defaultValue") ? parseTextFormatScalarValue(type, proto.defaultValue) : undefined;
      };
      break;
    }
  }
  return field;
}
function getFileEdition(proto) {
  switch (proto.syntax) {
    case "":
    case "proto2":
      return EDITION_PROTO22;
    case "proto3":
      return EDITION_PROTO32;
    case "editions":
      if (proto.edition in featureDefaults) {
        return proto.edition;
      }
      throw new Error(`${proto.name}: unsupported edition`);
    default:
      throw new Error(`${proto.name}: unsupported syntax "${proto.syntax}"`);
  }
}
function findFileDependencies(proto, reg) {
  return proto.dependency.map((wantName) => {
    const dep = reg.getFile(wantName);
    if (!dep) {
      throw new Error(`Cannot find ${wantName}, imported by ${proto.name}`);
    }
    return dep;
  });
}
function findEnumSharedPrefix(enumName, values) {
  const prefix = camelToSnakeCase(enumName) + "_";
  for (const value of values) {
    if (!value.name.toLowerCase().startsWith(prefix)) {
      return;
    }
    const shortName = value.name.substring(prefix.length);
    if (shortName.length == 0) {
      return;
    }
    if (/^\d/.test(shortName)) {
      return;
    }
  }
  return prefix;
}
function camelToSnakeCase(camel) {
  return (camel.substring(0, 1) + camel.substring(1).replace(/[A-Z]/g, (c) => "_" + c)).toLowerCase();
}
function makeTypeName(proto, parent, file) {
  let typeName;
  if (parent) {
    typeName = `${parent.typeName}.${proto.name}`;
  } else if (file.proto.package.length > 0) {
    typeName = `${file.proto.package}.${proto.name}`;
  } else {
    typeName = `${proto.name}`;
  }
  return typeName;
}
function trimLeadingDot(typeName) {
  return typeName.startsWith(".") ? typeName.substring(1) : typeName;
}
function findOneof(proto, allOneofs) {
  if (!unsafeIsSetExplicit(proto, "oneofIndex")) {
    return;
  }
  if (proto.proto3Optional) {
    return;
  }
  const oneof = allOneofs[proto.oneofIndex];
  assert(oneof, `invalid FieldDescriptorProto: oneof #${proto.oneofIndex} for field #${proto.number} not found`);
  return oneof;
}
function getFieldPresence(proto, oneof, isExtension, parent) {
  if (proto.label == LABEL_REQUIRED) {
    return LEGACY_REQUIRED;
  }
  if (proto.label == LABEL_REPEATED) {
    return IMPLICIT3;
  }
  if (!!oneof || proto.proto3Optional) {
    return EXPLICIT;
  }
  if (isExtension) {
    return EXPLICIT;
  }
  const resolved = resolveFeature("fieldPresence", { proto, parent });
  if (resolved == IMPLICIT3 && (proto.type == TYPE_MESSAGE || proto.type == TYPE_GROUP)) {
    return EXPLICIT;
  }
  return resolved;
}
function isPackedField(proto, parent) {
  if (proto.label != LABEL_REPEATED) {
    return false;
  }
  switch (proto.type) {
    case TYPE_STRING:
    case TYPE_BYTES:
    case TYPE_GROUP:
    case TYPE_MESSAGE:
      return false;
  }
  const o = proto.options;
  if (o && unsafeIsSetExplicit(o, "packed")) {
    return o.packed;
  }
  return PACKED == resolveFeature("repeatedFieldEncoding", {
    proto,
    parent
  });
}
function findMapEntryFields(mapEntry) {
  const key = mapEntry.fields.find((f) => f.number === 1);
  const value = mapEntry.fields.find((f) => f.number === 2);
  assert(key && key.fieldKind == "scalar" && key.scalar != ScalarType.BYTES && key.scalar != ScalarType.FLOAT && key.scalar != ScalarType.DOUBLE && value && value.fieldKind != "list" && value.fieldKind != "map");
  return { key, value };
}
function isEnumOpen(desc) {
  var _a;
  return OPEN == resolveFeature("enumType", {
    proto: desc.proto,
    parent: (_a = desc.parent) !== null && _a !== undefined ? _a : desc.file
  });
}
function isDelimitedEncoding(proto, parent) {
  if (proto.type == TYPE_GROUP) {
    return true;
  }
  return DELIMITED == resolveFeature("messageEncoding", {
    proto,
    parent
  });
}
function resolveFeature(name, ref) {
  var _a, _b;
  const featureSet = (_a = ref.proto.options) === null || _a === undefined ? undefined : _a.features;
  if (featureSet) {
    const val = featureSet[name];
    if (val != 0) {
      return val;
    }
  }
  if ("kind" in ref) {
    if (ref.kind == "message") {
      return resolveFeature(name, (_b = ref.parent) !== null && _b !== undefined ? _b : ref.file);
    }
    const editionDefaults = featureDefaults[ref.edition];
    if (!editionDefaults) {
      throw new Error(`feature default for edition ${ref.edition} not found`);
    }
    return editionDefaults[name];
  }
  return resolveFeature(name, ref.parent);
}
function assert(condition, msg) {
  if (!condition) {
    throw new Error(msg);
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/codegenv2/boot.js
function boot(boot2) {
  const root = bootFileDescriptorProto(boot2);
  root.messageType.forEach(restoreJsonNames);
  const reg = createFileRegistry(root, () => {
    return;
  });
  return reg.getFile(root.name);
}
function bootFileDescriptorProto(init) {
  const proto = Object.create({
    syntax: "",
    edition: 0
  });
  return Object.assign(proto, Object.assign(Object.assign({ $typeName: "google.protobuf.FileDescriptorProto", dependency: [], publicDependency: [], weakDependency: [], optionDependency: [], service: [], extension: [] }, init), { messageType: init.messageType.map(bootDescriptorProto), enumType: init.enumType.map(bootEnumDescriptorProto) }));
}
function bootDescriptorProto(init) {
  var _a, _b, _c, _d, _e, _f, _g, _h;
  const proto = Object.create({
    visibility: 0
  });
  return Object.assign(proto, {
    $typeName: "google.protobuf.DescriptorProto",
    name: init.name,
    field: (_b = (_a = init.field) === null || _a === undefined ? undefined : _a.map(bootFieldDescriptorProto)) !== null && _b !== undefined ? _b : [],
    extension: [],
    nestedType: (_d = (_c = init.nestedType) === null || _c === undefined ? undefined : _c.map(bootDescriptorProto)) !== null && _d !== undefined ? _d : [],
    enumType: (_f = (_e = init.enumType) === null || _e === undefined ? undefined : _e.map(bootEnumDescriptorProto)) !== null && _f !== undefined ? _f : [],
    extensionRange: (_h = (_g = init.extensionRange) === null || _g === undefined ? undefined : _g.map((e) => Object.assign({ $typeName: "google.protobuf.DescriptorProto.ExtensionRange" }, e))) !== null && _h !== undefined ? _h : [],
    oneofDecl: [],
    reservedRange: [],
    reservedName: []
  });
}
function bootFieldDescriptorProto(init) {
  const proto = Object.create({
    label: 1,
    typeName: "",
    extendee: "",
    defaultValue: "",
    oneofIndex: 0,
    jsonName: "",
    proto3Optional: false
  });
  return Object.assign(proto, Object.assign(Object.assign({ $typeName: "google.protobuf.FieldDescriptorProto" }, init), { options: init.options ? bootFieldOptions(init.options) : undefined }));
}
function bootFieldOptions(init) {
  var _a, _b, _c;
  const proto = Object.create({
    ctype: 0,
    packed: false,
    jstype: 0,
    lazy: false,
    unverifiedLazy: false,
    deprecated: false,
    weak: false,
    debugRedact: false,
    retention: 0
  });
  return Object.assign(proto, Object.assign(Object.assign({ $typeName: "google.protobuf.FieldOptions" }, init), { targets: (_a = init.targets) !== null && _a !== undefined ? _a : [], editionDefaults: (_c = (_b = init.editionDefaults) === null || _b === undefined ? undefined : _b.map((e) => Object.assign({ $typeName: "google.protobuf.FieldOptions.EditionDefault" }, e))) !== null && _c !== undefined ? _c : [], uninterpretedOption: [] }));
}
function bootEnumDescriptorProto(init) {
  const proto = Object.create({
    visibility: 0
  });
  return Object.assign(proto, {
    $typeName: "google.protobuf.EnumDescriptorProto",
    name: init.name,
    reservedName: [],
    reservedRange: [],
    value: init.value.map((e) => Object.assign({ $typeName: "google.protobuf.EnumValueDescriptorProto" }, e))
  });
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/codegenv2/message.js
function messageDesc(file, path, ...paths) {
  return paths.reduce((acc, cur) => acc.nestedMessages[cur], file.messages[path]);
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/wkt/gen/google/protobuf/descriptor_pb.js
var file_google_protobuf_descriptor = /* @__PURE__ */ boot({ name: "google/protobuf/descriptor.proto", package: "google.protobuf", messageType: [{ name: "FileDescriptorSet", field: [{ name: "file", number: 1, type: 11, label: 3, typeName: ".google.protobuf.FileDescriptorProto" }], extensionRange: [{ start: 536000000, end: 536000001 }] }, { name: "FileDescriptorProto", field: [{ name: "name", number: 1, type: 9, label: 1 }, { name: "package", number: 2, type: 9, label: 1 }, { name: "dependency", number: 3, type: 9, label: 3 }, { name: "public_dependency", number: 10, type: 5, label: 3 }, { name: "weak_dependency", number: 11, type: 5, label: 3 }, { name: "option_dependency", number: 15, type: 9, label: 3 }, { name: "message_type", number: 4, type: 11, label: 3, typeName: ".google.protobuf.DescriptorProto" }, { name: "enum_type", number: 5, type: 11, label: 3, typeName: ".google.protobuf.EnumDescriptorProto" }, { name: "service", number: 6, type: 11, label: 3, typeName: ".google.protobuf.ServiceDescriptorProto" }, { name: "extension", number: 7, type: 11, label: 3, typeName: ".google.protobuf.FieldDescriptorProto" }, { name: "options", number: 8, type: 11, label: 1, typeName: ".google.protobuf.FileOptions" }, { name: "source_code_info", number: 9, type: 11, label: 1, typeName: ".google.protobuf.SourceCodeInfo" }, { name: "syntax", number: 12, type: 9, label: 1 }, { name: "edition", number: 14, type: 14, label: 1, typeName: ".google.protobuf.Edition" }] }, { name: "DescriptorProto", field: [{ name: "name", number: 1, type: 9, label: 1 }, { name: "field", number: 2, type: 11, label: 3, typeName: ".google.protobuf.FieldDescriptorProto" }, { name: "extension", number: 6, type: 11, label: 3, typeName: ".google.protobuf.FieldDescriptorProto" }, { name: "nested_type", number: 3, type: 11, label: 3, typeName: ".google.protobuf.DescriptorProto" }, { name: "enum_type", number: 4, type: 11, label: 3, typeName: ".google.protobuf.EnumDescriptorProto" }, { name: "extension_range", number: 5, type: 11, label: 3, typeName: ".google.protobuf.DescriptorProto.ExtensionRange" }, { name: "oneof_decl", number: 8, type: 11, label: 3, typeName: ".google.protobuf.OneofDescriptorProto" }, { name: "options", number: 7, type: 11, label: 1, typeName: ".google.protobuf.MessageOptions" }, { name: "reserved_range", number: 9, type: 11, label: 3, typeName: ".google.protobuf.DescriptorProto.ReservedRange" }, { name: "reserved_name", number: 10, type: 9, label: 3 }, { name: "visibility", number: 11, type: 14, label: 1, typeName: ".google.protobuf.SymbolVisibility" }], nestedType: [{ name: "ExtensionRange", field: [{ name: "start", number: 1, type: 5, label: 1 }, { name: "end", number: 2, type: 5, label: 1 }, { name: "options", number: 3, type: 11, label: 1, typeName: ".google.protobuf.ExtensionRangeOptions" }] }, { name: "ReservedRange", field: [{ name: "start", number: 1, type: 5, label: 1 }, { name: "end", number: 2, type: 5, label: 1 }] }] }, { name: "ExtensionRangeOptions", field: [{ name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }, { name: "declaration", number: 2, type: 11, label: 3, typeName: ".google.protobuf.ExtensionRangeOptions.Declaration", options: { retention: 2 } }, { name: "features", number: 50, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "verification", number: 3, type: 14, label: 1, typeName: ".google.protobuf.ExtensionRangeOptions.VerificationState", defaultValue: "UNVERIFIED", options: { retention: 2 } }], nestedType: [{ name: "Declaration", field: [{ name: "number", number: 1, type: 5, label: 1 }, { name: "full_name", number: 2, type: 9, label: 1 }, { name: "type", number: 3, type: 9, label: 1 }, { name: "reserved", number: 5, type: 8, label: 1 }, { name: "repeated", number: 6, type: 8, label: 1 }] }], enumType: [{ name: "VerificationState", value: [{ name: "DECLARATION", number: 0 }, { name: "UNVERIFIED", number: 1 }] }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "FieldDescriptorProto", field: [{ name: "name", number: 1, type: 9, label: 1 }, { name: "number", number: 3, type: 5, label: 1 }, { name: "label", number: 4, type: 14, label: 1, typeName: ".google.protobuf.FieldDescriptorProto.Label" }, { name: "type", number: 5, type: 14, label: 1, typeName: ".google.protobuf.FieldDescriptorProto.Type" }, { name: "type_name", number: 6, type: 9, label: 1 }, { name: "extendee", number: 2, type: 9, label: 1 }, { name: "default_value", number: 7, type: 9, label: 1 }, { name: "oneof_index", number: 9, type: 5, label: 1 }, { name: "json_name", number: 10, type: 9, label: 1 }, { name: "options", number: 8, type: 11, label: 1, typeName: ".google.protobuf.FieldOptions" }, { name: "proto3_optional", number: 17, type: 8, label: 1 }], enumType: [{ name: "Type", value: [{ name: "TYPE_DOUBLE", number: 1 }, { name: "TYPE_FLOAT", number: 2 }, { name: "TYPE_INT64", number: 3 }, { name: "TYPE_UINT64", number: 4 }, { name: "TYPE_INT32", number: 5 }, { name: "TYPE_FIXED64", number: 6 }, { name: "TYPE_FIXED32", number: 7 }, { name: "TYPE_BOOL", number: 8 }, { name: "TYPE_STRING", number: 9 }, { name: "TYPE_GROUP", number: 10 }, { name: "TYPE_MESSAGE", number: 11 }, { name: "TYPE_BYTES", number: 12 }, { name: "TYPE_UINT32", number: 13 }, { name: "TYPE_ENUM", number: 14 }, { name: "TYPE_SFIXED32", number: 15 }, { name: "TYPE_SFIXED64", number: 16 }, { name: "TYPE_SINT32", number: 17 }, { name: "TYPE_SINT64", number: 18 }] }, { name: "Label", value: [{ name: "LABEL_OPTIONAL", number: 1 }, { name: "LABEL_REPEATED", number: 3 }, { name: "LABEL_REQUIRED", number: 2 }] }] }, { name: "OneofDescriptorProto", field: [{ name: "name", number: 1, type: 9, label: 1 }, { name: "options", number: 2, type: 11, label: 1, typeName: ".google.protobuf.OneofOptions" }] }, { name: "EnumDescriptorProto", field: [{ name: "name", number: 1, type: 9, label: 1 }, { name: "value", number: 2, type: 11, label: 3, typeName: ".google.protobuf.EnumValueDescriptorProto" }, { name: "options", number: 3, type: 11, label: 1, typeName: ".google.protobuf.EnumOptions" }, { name: "reserved_range", number: 4, type: 11, label: 3, typeName: ".google.protobuf.EnumDescriptorProto.EnumReservedRange" }, { name: "reserved_name", number: 5, type: 9, label: 3 }, { name: "visibility", number: 6, type: 14, label: 1, typeName: ".google.protobuf.SymbolVisibility" }], nestedType: [{ name: "EnumReservedRange", field: [{ name: "start", number: 1, type: 5, label: 1 }, { name: "end", number: 2, type: 5, label: 1 }] }] }, { name: "EnumValueDescriptorProto", field: [{ name: "name", number: 1, type: 9, label: 1 }, { name: "number", number: 2, type: 5, label: 1 }, { name: "options", number: 3, type: 11, label: 1, typeName: ".google.protobuf.EnumValueOptions" }] }, { name: "ServiceDescriptorProto", field: [{ name: "name", number: 1, type: 9, label: 1 }, { name: "method", number: 2, type: 11, label: 3, typeName: ".google.protobuf.MethodDescriptorProto" }, { name: "options", number: 3, type: 11, label: 1, typeName: ".google.protobuf.ServiceOptions" }] }, { name: "MethodDescriptorProto", field: [{ name: "name", number: 1, type: 9, label: 1 }, { name: "input_type", number: 2, type: 9, label: 1 }, { name: "output_type", number: 3, type: 9, label: 1 }, { name: "options", number: 4, type: 11, label: 1, typeName: ".google.protobuf.MethodOptions" }, { name: "client_streaming", number: 5, type: 8, label: 1, defaultValue: "false" }, { name: "server_streaming", number: 6, type: 8, label: 1, defaultValue: "false" }] }, { name: "FileOptions", field: [{ name: "java_package", number: 1, type: 9, label: 1 }, { name: "java_outer_classname", number: 8, type: 9, label: 1 }, { name: "java_multiple_files", number: 10, type: 8, label: 1, defaultValue: "false" }, { name: "java_generate_equals_and_hash", number: 20, type: 8, label: 1, options: { deprecated: true } }, { name: "java_string_check_utf8", number: 27, type: 8, label: 1, defaultValue: "false" }, { name: "optimize_for", number: 9, type: 14, label: 1, typeName: ".google.protobuf.FileOptions.OptimizeMode", defaultValue: "SPEED" }, { name: "go_package", number: 11, type: 9, label: 1 }, { name: "cc_generic_services", number: 16, type: 8, label: 1, defaultValue: "false" }, { name: "java_generic_services", number: 17, type: 8, label: 1, defaultValue: "false" }, { name: "py_generic_services", number: 18, type: 8, label: 1, defaultValue: "false" }, { name: "deprecated", number: 23, type: 8, label: 1, defaultValue: "false" }, { name: "cc_enable_arenas", number: 31, type: 8, label: 1, defaultValue: "true" }, { name: "objc_class_prefix", number: 36, type: 9, label: 1 }, { name: "csharp_namespace", number: 37, type: 9, label: 1 }, { name: "swift_prefix", number: 39, type: 9, label: 1 }, { name: "php_class_prefix", number: 40, type: 9, label: 1 }, { name: "php_namespace", number: 41, type: 9, label: 1 }, { name: "php_metadata_namespace", number: 44, type: 9, label: 1 }, { name: "ruby_package", number: 45, type: 9, label: 1 }, { name: "features", number: 50, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }], enumType: [{ name: "OptimizeMode", value: [{ name: "SPEED", number: 1 }, { name: "CODE_SIZE", number: 2 }, { name: "LITE_RUNTIME", number: 3 }] }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "MessageOptions", field: [{ name: "message_set_wire_format", number: 1, type: 8, label: 1, defaultValue: "false" }, { name: "no_standard_descriptor_accessor", number: 2, type: 8, label: 1, defaultValue: "false" }, { name: "deprecated", number: 3, type: 8, label: 1, defaultValue: "false" }, { name: "map_entry", number: 7, type: 8, label: 1 }, { name: "deprecated_legacy_json_field_conflicts", number: 11, type: 8, label: 1, options: { deprecated: true } }, { name: "features", number: 12, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "FieldOptions", field: [{ name: "ctype", number: 1, type: 14, label: 1, typeName: ".google.protobuf.FieldOptions.CType", defaultValue: "STRING" }, { name: "packed", number: 2, type: 8, label: 1 }, { name: "jstype", number: 6, type: 14, label: 1, typeName: ".google.protobuf.FieldOptions.JSType", defaultValue: "JS_NORMAL" }, { name: "lazy", number: 5, type: 8, label: 1, defaultValue: "false" }, { name: "unverified_lazy", number: 15, type: 8, label: 1, defaultValue: "false" }, { name: "deprecated", number: 3, type: 8, label: 1, defaultValue: "false" }, { name: "weak", number: 10, type: 8, label: 1, defaultValue: "false", options: { deprecated: true } }, { name: "debug_redact", number: 16, type: 8, label: 1, defaultValue: "false" }, { name: "retention", number: 17, type: 14, label: 1, typeName: ".google.protobuf.FieldOptions.OptionRetention" }, { name: "targets", number: 19, type: 14, label: 3, typeName: ".google.protobuf.FieldOptions.OptionTargetType" }, { name: "edition_defaults", number: 20, type: 11, label: 3, typeName: ".google.protobuf.FieldOptions.EditionDefault" }, { name: "features", number: 21, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "feature_support", number: 22, type: 11, label: 1, typeName: ".google.protobuf.FieldOptions.FeatureSupport" }, { name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }], nestedType: [{ name: "EditionDefault", field: [{ name: "edition", number: 3, type: 14, label: 1, typeName: ".google.protobuf.Edition" }, { name: "value", number: 2, type: 9, label: 1 }] }, { name: "FeatureSupport", field: [{ name: "edition_introduced", number: 1, type: 14, label: 1, typeName: ".google.protobuf.Edition" }, { name: "edition_deprecated", number: 2, type: 14, label: 1, typeName: ".google.protobuf.Edition" }, { name: "deprecation_warning", number: 3, type: 9, label: 1 }, { name: "edition_removed", number: 4, type: 14, label: 1, typeName: ".google.protobuf.Edition" }] }], enumType: [{ name: "CType", value: [{ name: "STRING", number: 0 }, { name: "CORD", number: 1 }, { name: "STRING_PIECE", number: 2 }] }, { name: "JSType", value: [{ name: "JS_NORMAL", number: 0 }, { name: "JS_STRING", number: 1 }, { name: "JS_NUMBER", number: 2 }] }, { name: "OptionRetention", value: [{ name: "RETENTION_UNKNOWN", number: 0 }, { name: "RETENTION_RUNTIME", number: 1 }, { name: "RETENTION_SOURCE", number: 2 }] }, { name: "OptionTargetType", value: [{ name: "TARGET_TYPE_UNKNOWN", number: 0 }, { name: "TARGET_TYPE_FILE", number: 1 }, { name: "TARGET_TYPE_EXTENSION_RANGE", number: 2 }, { name: "TARGET_TYPE_MESSAGE", number: 3 }, { name: "TARGET_TYPE_FIELD", number: 4 }, { name: "TARGET_TYPE_ONEOF", number: 5 }, { name: "TARGET_TYPE_ENUM", number: 6 }, { name: "TARGET_TYPE_ENUM_ENTRY", number: 7 }, { name: "TARGET_TYPE_SERVICE", number: 8 }, { name: "TARGET_TYPE_METHOD", number: 9 }] }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "OneofOptions", field: [{ name: "features", number: 1, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "EnumOptions", field: [{ name: "allow_alias", number: 2, type: 8, label: 1 }, { name: "deprecated", number: 3, type: 8, label: 1, defaultValue: "false" }, { name: "deprecated_legacy_json_field_conflicts", number: 6, type: 8, label: 1, options: { deprecated: true } }, { name: "features", number: 7, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "EnumValueOptions", field: [{ name: "deprecated", number: 1, type: 8, label: 1, defaultValue: "false" }, { name: "features", number: 2, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "debug_redact", number: 3, type: 8, label: 1, defaultValue: "false" }, { name: "feature_support", number: 4, type: 11, label: 1, typeName: ".google.protobuf.FieldOptions.FeatureSupport" }, { name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "ServiceOptions", field: [{ name: "features", number: 34, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "deprecated", number: 33, type: 8, label: 1, defaultValue: "false" }, { name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "MethodOptions", field: [{ name: "deprecated", number: 33, type: 8, label: 1, defaultValue: "false" }, { name: "idempotency_level", number: 34, type: 14, label: 1, typeName: ".google.protobuf.MethodOptions.IdempotencyLevel", defaultValue: "IDEMPOTENCY_UNKNOWN" }, { name: "features", number: 35, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "uninterpreted_option", number: 999, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption" }], enumType: [{ name: "IdempotencyLevel", value: [{ name: "IDEMPOTENCY_UNKNOWN", number: 0 }, { name: "NO_SIDE_EFFECTS", number: 1 }, { name: "IDEMPOTENT", number: 2 }] }], extensionRange: [{ start: 1000, end: 536870912 }] }, { name: "UninterpretedOption", field: [{ name: "name", number: 2, type: 11, label: 3, typeName: ".google.protobuf.UninterpretedOption.NamePart" }, { name: "identifier_value", number: 3, type: 9, label: 1 }, { name: "positive_int_value", number: 4, type: 4, label: 1 }, { name: "negative_int_value", number: 5, type: 3, label: 1 }, { name: "double_value", number: 6, type: 1, label: 1 }, { name: "string_value", number: 7, type: 12, label: 1 }, { name: "aggregate_value", number: 8, type: 9, label: 1 }], nestedType: [{ name: "NamePart", field: [{ name: "name_part", number: 1, type: 9, label: 2 }, { name: "is_extension", number: 2, type: 8, label: 2 }] }] }, { name: "FeatureSet", field: [{ name: "field_presence", number: 1, type: 14, label: 1, typeName: ".google.protobuf.FeatureSet.FieldPresence", options: { retention: 1, targets: [4, 1], editionDefaults: [{ value: "EXPLICIT", edition: 900 }, { value: "IMPLICIT", edition: 999 }, { value: "EXPLICIT", edition: 1000 }] } }, { name: "enum_type", number: 2, type: 14, label: 1, typeName: ".google.protobuf.FeatureSet.EnumType", options: { retention: 1, targets: [6, 1], editionDefaults: [{ value: "CLOSED", edition: 900 }, { value: "OPEN", edition: 999 }] } }, { name: "repeated_field_encoding", number: 3, type: 14, label: 1, typeName: ".google.protobuf.FeatureSet.RepeatedFieldEncoding", options: { retention: 1, targets: [4, 1], editionDefaults: [{ value: "EXPANDED", edition: 900 }, { value: "PACKED", edition: 999 }] } }, { name: "utf8_validation", number: 4, type: 14, label: 1, typeName: ".google.protobuf.FeatureSet.Utf8Validation", options: { retention: 1, targets: [4, 1], editionDefaults: [{ value: "NONE", edition: 900 }, { value: "VERIFY", edition: 999 }] } }, { name: "message_encoding", number: 5, type: 14, label: 1, typeName: ".google.protobuf.FeatureSet.MessageEncoding", options: { retention: 1, targets: [4, 1], editionDefaults: [{ value: "LENGTH_PREFIXED", edition: 900 }] } }, { name: "json_format", number: 6, type: 14, label: 1, typeName: ".google.protobuf.FeatureSet.JsonFormat", options: { retention: 1, targets: [3, 6, 1], editionDefaults: [{ value: "LEGACY_BEST_EFFORT", edition: 900 }, { value: "ALLOW", edition: 999 }] } }, { name: "enforce_naming_style", number: 7, type: 14, label: 1, typeName: ".google.protobuf.FeatureSet.EnforceNamingStyle", options: { retention: 2, targets: [1, 2, 3, 4, 5, 6, 7, 8, 9], editionDefaults: [{ value: "STYLE_LEGACY", edition: 900 }, { value: "STYLE2024", edition: 1001 }] } }, { name: "default_symbol_visibility", number: 8, type: 14, label: 1, typeName: ".google.protobuf.FeatureSet.VisibilityFeature.DefaultSymbolVisibility", options: { retention: 2, targets: [1], editionDefaults: [{ value: "EXPORT_ALL", edition: 900 }, { value: "EXPORT_TOP_LEVEL", edition: 1001 }] } }], nestedType: [{ name: "VisibilityFeature", enumType: [{ name: "DefaultSymbolVisibility", value: [{ name: "DEFAULT_SYMBOL_VISIBILITY_UNKNOWN", number: 0 }, { name: "EXPORT_ALL", number: 1 }, { name: "EXPORT_TOP_LEVEL", number: 2 }, { name: "LOCAL_ALL", number: 3 }, { name: "STRICT", number: 4 }] }] }], enumType: [{ name: "FieldPresence", value: [{ name: "FIELD_PRESENCE_UNKNOWN", number: 0 }, { name: "EXPLICIT", number: 1 }, { name: "IMPLICIT", number: 2 }, { name: "LEGACY_REQUIRED", number: 3 }] }, { name: "EnumType", value: [{ name: "ENUM_TYPE_UNKNOWN", number: 0 }, { name: "OPEN", number: 1 }, { name: "CLOSED", number: 2 }] }, { name: "RepeatedFieldEncoding", value: [{ name: "REPEATED_FIELD_ENCODING_UNKNOWN", number: 0 }, { name: "PACKED", number: 1 }, { name: "EXPANDED", number: 2 }] }, { name: "Utf8Validation", value: [{ name: "UTF8_VALIDATION_UNKNOWN", number: 0 }, { name: "VERIFY", number: 2 }, { name: "NONE", number: 3 }] }, { name: "MessageEncoding", value: [{ name: "MESSAGE_ENCODING_UNKNOWN", number: 0 }, { name: "LENGTH_PREFIXED", number: 1 }, { name: "DELIMITED", number: 2 }] }, { name: "JsonFormat", value: [{ name: "JSON_FORMAT_UNKNOWN", number: 0 }, { name: "ALLOW", number: 1 }, { name: "LEGACY_BEST_EFFORT", number: 2 }] }, { name: "EnforceNamingStyle", value: [{ name: "ENFORCE_NAMING_STYLE_UNKNOWN", number: 0 }, { name: "STYLE2024", number: 1 }, { name: "STYLE_LEGACY", number: 2 }] }], extensionRange: [{ start: 1000, end: 9995 }, { start: 9995, end: 1e4 }, { start: 1e4, end: 10001 }] }, { name: "FeatureSetDefaults", field: [{ name: "defaults", number: 1, type: 11, label: 3, typeName: ".google.protobuf.FeatureSetDefaults.FeatureSetEditionDefault" }, { name: "minimum_edition", number: 4, type: 14, label: 1, typeName: ".google.protobuf.Edition" }, { name: "maximum_edition", number: 5, type: 14, label: 1, typeName: ".google.protobuf.Edition" }], nestedType: [{ name: "FeatureSetEditionDefault", field: [{ name: "edition", number: 3, type: 14, label: 1, typeName: ".google.protobuf.Edition" }, { name: "overridable_features", number: 4, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }, { name: "fixed_features", number: 5, type: 11, label: 1, typeName: ".google.protobuf.FeatureSet" }] }] }, { name: "SourceCodeInfo", field: [{ name: "location", number: 1, type: 11, label: 3, typeName: ".google.protobuf.SourceCodeInfo.Location" }], nestedType: [{ name: "Location", field: [{ name: "path", number: 1, type: 5, label: 3, options: { packed: true } }, { name: "span", number: 2, type: 5, label: 3, options: { packed: true } }, { name: "leading_comments", number: 3, type: 9, label: 1 }, { name: "trailing_comments", number: 4, type: 9, label: 1 }, { name: "leading_detached_comments", number: 6, type: 9, label: 3 }] }], extensionRange: [{ start: 536000000, end: 536000001 }] }, { name: "GeneratedCodeInfo", field: [{ name: "annotation", number: 1, type: 11, label: 3, typeName: ".google.protobuf.GeneratedCodeInfo.Annotation" }], nestedType: [{ name: "Annotation", field: [{ name: "path", number: 1, type: 5, label: 3, options: { packed: true } }, { name: "source_file", number: 2, type: 9, label: 1 }, { name: "begin", number: 3, type: 5, label: 1 }, { name: "end", number: 4, type: 5, label: 1 }, { name: "semantic", number: 5, type: 14, label: 1, typeName: ".google.protobuf.GeneratedCodeInfo.Annotation.Semantic" }], enumType: [{ name: "Semantic", value: [{ name: "NONE", number: 0 }, { name: "SET", number: 1 }, { name: "ALIAS", number: 2 }] }] }] }], enumType: [{ name: "Edition", value: [{ name: "EDITION_UNKNOWN", number: 0 }, { name: "EDITION_LEGACY", number: 900 }, { name: "EDITION_PROTO2", number: 998 }, { name: "EDITION_PROTO3", number: 999 }, { name: "EDITION_2023", number: 1000 }, { name: "EDITION_2024", number: 1001 }, { name: "EDITION_UNSTABLE", number: 9999 }, { name: "EDITION_1_TEST_ONLY", number: 1 }, { name: "EDITION_2_TEST_ONLY", number: 2 }, { name: "EDITION_99997_TEST_ONLY", number: 99997 }, { name: "EDITION_99998_TEST_ONLY", number: 99998 }, { name: "EDITION_99999_TEST_ONLY", number: 99999 }, { name: "EDITION_MAX", number: 2147483647 }] }, { name: "SymbolVisibility", value: [{ name: "VISIBILITY_UNSET", number: 0 }, { name: "VISIBILITY_LOCAL", number: 1 }, { name: "VISIBILITY_EXPORT", number: 2 }] }] });
var FileDescriptorProtoSchema = /* @__PURE__ */ messageDesc(file_google_protobuf_descriptor, 1);
var ExtensionRangeOptions_VerificationState;
(function(ExtensionRangeOptions_VerificationState2) {
  ExtensionRangeOptions_VerificationState2[ExtensionRangeOptions_VerificationState2["DECLARATION"] = 0] = "DECLARATION";
  ExtensionRangeOptions_VerificationState2[ExtensionRangeOptions_VerificationState2["UNVERIFIED"] = 1] = "UNVERIFIED";
})(ExtensionRangeOptions_VerificationState || (ExtensionRangeOptions_VerificationState = {}));
var FieldDescriptorProto_Type;
(function(FieldDescriptorProto_Type2) {
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["DOUBLE"] = 1] = "DOUBLE";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["FLOAT"] = 2] = "FLOAT";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["INT64"] = 3] = "INT64";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["UINT64"] = 4] = "UINT64";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["INT32"] = 5] = "INT32";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["FIXED64"] = 6] = "FIXED64";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["FIXED32"] = 7] = "FIXED32";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["BOOL"] = 8] = "BOOL";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["STRING"] = 9] = "STRING";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["GROUP"] = 10] = "GROUP";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["MESSAGE"] = 11] = "MESSAGE";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["BYTES"] = 12] = "BYTES";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["UINT32"] = 13] = "UINT32";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["ENUM"] = 14] = "ENUM";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["SFIXED32"] = 15] = "SFIXED32";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["SFIXED64"] = 16] = "SFIXED64";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["SINT32"] = 17] = "SINT32";
  FieldDescriptorProto_Type2[FieldDescriptorProto_Type2["SINT64"] = 18] = "SINT64";
})(FieldDescriptorProto_Type || (FieldDescriptorProto_Type = {}));
var FieldDescriptorProto_Label;
(function(FieldDescriptorProto_Label2) {
  FieldDescriptorProto_Label2[FieldDescriptorProto_Label2["OPTIONAL"] = 1] = "OPTIONAL";
  FieldDescriptorProto_Label2[FieldDescriptorProto_Label2["REPEATED"] = 3] = "REPEATED";
  FieldDescriptorProto_Label2[FieldDescriptorProto_Label2["REQUIRED"] = 2] = "REQUIRED";
})(FieldDescriptorProto_Label || (FieldDescriptorProto_Label = {}));
var FileOptions_OptimizeMode;
(function(FileOptions_OptimizeMode2) {
  FileOptions_OptimizeMode2[FileOptions_OptimizeMode2["SPEED"] = 1] = "SPEED";
  FileOptions_OptimizeMode2[FileOptions_OptimizeMode2["CODE_SIZE"] = 2] = "CODE_SIZE";
  FileOptions_OptimizeMode2[FileOptions_OptimizeMode2["LITE_RUNTIME"] = 3] = "LITE_RUNTIME";
})(FileOptions_OptimizeMode || (FileOptions_OptimizeMode = {}));
var FieldOptions_CType;
(function(FieldOptions_CType2) {
  FieldOptions_CType2[FieldOptions_CType2["STRING"] = 0] = "STRING";
  FieldOptions_CType2[FieldOptions_CType2["CORD"] = 1] = "CORD";
  FieldOptions_CType2[FieldOptions_CType2["STRING_PIECE"] = 2] = "STRING_PIECE";
})(FieldOptions_CType || (FieldOptions_CType = {}));
var FieldOptions_JSType;
(function(FieldOptions_JSType2) {
  FieldOptions_JSType2[FieldOptions_JSType2["JS_NORMAL"] = 0] = "JS_NORMAL";
  FieldOptions_JSType2[FieldOptions_JSType2["JS_STRING"] = 1] = "JS_STRING";
  FieldOptions_JSType2[FieldOptions_JSType2["JS_NUMBER"] = 2] = "JS_NUMBER";
})(FieldOptions_JSType || (FieldOptions_JSType = {}));
var FieldOptions_OptionRetention;
(function(FieldOptions_OptionRetention2) {
  FieldOptions_OptionRetention2[FieldOptions_OptionRetention2["RETENTION_UNKNOWN"] = 0] = "RETENTION_UNKNOWN";
  FieldOptions_OptionRetention2[FieldOptions_OptionRetention2["RETENTION_RUNTIME"] = 1] = "RETENTION_RUNTIME";
  FieldOptions_OptionRetention2[FieldOptions_OptionRetention2["RETENTION_SOURCE"] = 2] = "RETENTION_SOURCE";
})(FieldOptions_OptionRetention || (FieldOptions_OptionRetention = {}));
var FieldOptions_OptionTargetType;
(function(FieldOptions_OptionTargetType2) {
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_UNKNOWN"] = 0] = "TARGET_TYPE_UNKNOWN";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_FILE"] = 1] = "TARGET_TYPE_FILE";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_EXTENSION_RANGE"] = 2] = "TARGET_TYPE_EXTENSION_RANGE";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_MESSAGE"] = 3] = "TARGET_TYPE_MESSAGE";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_FIELD"] = 4] = "TARGET_TYPE_FIELD";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_ONEOF"] = 5] = "TARGET_TYPE_ONEOF";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_ENUM"] = 6] = "TARGET_TYPE_ENUM";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_ENUM_ENTRY"] = 7] = "TARGET_TYPE_ENUM_ENTRY";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_SERVICE"] = 8] = "TARGET_TYPE_SERVICE";
  FieldOptions_OptionTargetType2[FieldOptions_OptionTargetType2["TARGET_TYPE_METHOD"] = 9] = "TARGET_TYPE_METHOD";
})(FieldOptions_OptionTargetType || (FieldOptions_OptionTargetType = {}));
var MethodOptions_IdempotencyLevel;
(function(MethodOptions_IdempotencyLevel2) {
  MethodOptions_IdempotencyLevel2[MethodOptions_IdempotencyLevel2["IDEMPOTENCY_UNKNOWN"] = 0] = "IDEMPOTENCY_UNKNOWN";
  MethodOptions_IdempotencyLevel2[MethodOptions_IdempotencyLevel2["NO_SIDE_EFFECTS"] = 1] = "NO_SIDE_EFFECTS";
  MethodOptions_IdempotencyLevel2[MethodOptions_IdempotencyLevel2["IDEMPOTENT"] = 2] = "IDEMPOTENT";
})(MethodOptions_IdempotencyLevel || (MethodOptions_IdempotencyLevel = {}));
var FeatureSet_VisibilityFeature_DefaultSymbolVisibility;
(function(FeatureSet_VisibilityFeature_DefaultSymbolVisibility2) {
  FeatureSet_VisibilityFeature_DefaultSymbolVisibility2[FeatureSet_VisibilityFeature_DefaultSymbolVisibility2["DEFAULT_SYMBOL_VISIBILITY_UNKNOWN"] = 0] = "DEFAULT_SYMBOL_VISIBILITY_UNKNOWN";
  FeatureSet_VisibilityFeature_DefaultSymbolVisibility2[FeatureSet_VisibilityFeature_DefaultSymbolVisibility2["EXPORT_ALL"] = 1] = "EXPORT_ALL";
  FeatureSet_VisibilityFeature_DefaultSymbolVisibility2[FeatureSet_VisibilityFeature_DefaultSymbolVisibility2["EXPORT_TOP_LEVEL"] = 2] = "EXPORT_TOP_LEVEL";
  FeatureSet_VisibilityFeature_DefaultSymbolVisibility2[FeatureSet_VisibilityFeature_DefaultSymbolVisibility2["LOCAL_ALL"] = 3] = "LOCAL_ALL";
  FeatureSet_VisibilityFeature_DefaultSymbolVisibility2[FeatureSet_VisibilityFeature_DefaultSymbolVisibility2["STRICT"] = 4] = "STRICT";
})(FeatureSet_VisibilityFeature_DefaultSymbolVisibility || (FeatureSet_VisibilityFeature_DefaultSymbolVisibility = {}));
var FeatureSet_FieldPresence;
(function(FeatureSet_FieldPresence2) {
  FeatureSet_FieldPresence2[FeatureSet_FieldPresence2["FIELD_PRESENCE_UNKNOWN"] = 0] = "FIELD_PRESENCE_UNKNOWN";
  FeatureSet_FieldPresence2[FeatureSet_FieldPresence2["EXPLICIT"] = 1] = "EXPLICIT";
  FeatureSet_FieldPresence2[FeatureSet_FieldPresence2["IMPLICIT"] = 2] = "IMPLICIT";
  FeatureSet_FieldPresence2[FeatureSet_FieldPresence2["LEGACY_REQUIRED"] = 3] = "LEGACY_REQUIRED";
})(FeatureSet_FieldPresence || (FeatureSet_FieldPresence = {}));
var FeatureSet_EnumType;
(function(FeatureSet_EnumType2) {
  FeatureSet_EnumType2[FeatureSet_EnumType2["ENUM_TYPE_UNKNOWN"] = 0] = "ENUM_TYPE_UNKNOWN";
  FeatureSet_EnumType2[FeatureSet_EnumType2["OPEN"] = 1] = "OPEN";
  FeatureSet_EnumType2[FeatureSet_EnumType2["CLOSED"] = 2] = "CLOSED";
})(FeatureSet_EnumType || (FeatureSet_EnumType = {}));
var FeatureSet_RepeatedFieldEncoding;
(function(FeatureSet_RepeatedFieldEncoding2) {
  FeatureSet_RepeatedFieldEncoding2[FeatureSet_RepeatedFieldEncoding2["REPEATED_FIELD_ENCODING_UNKNOWN"] = 0] = "REPEATED_FIELD_ENCODING_UNKNOWN";
  FeatureSet_RepeatedFieldEncoding2[FeatureSet_RepeatedFieldEncoding2["PACKED"] = 1] = "PACKED";
  FeatureSet_RepeatedFieldEncoding2[FeatureSet_RepeatedFieldEncoding2["EXPANDED"] = 2] = "EXPANDED";
})(FeatureSet_RepeatedFieldEncoding || (FeatureSet_RepeatedFieldEncoding = {}));
var FeatureSet_Utf8Validation;
(function(FeatureSet_Utf8Validation2) {
  FeatureSet_Utf8Validation2[FeatureSet_Utf8Validation2["UTF8_VALIDATION_UNKNOWN"] = 0] = "UTF8_VALIDATION_UNKNOWN";
  FeatureSet_Utf8Validation2[FeatureSet_Utf8Validation2["VERIFY"] = 2] = "VERIFY";
  FeatureSet_Utf8Validation2[FeatureSet_Utf8Validation2["NONE"] = 3] = "NONE";
})(FeatureSet_Utf8Validation || (FeatureSet_Utf8Validation = {}));
var FeatureSet_MessageEncoding;
(function(FeatureSet_MessageEncoding2) {
  FeatureSet_MessageEncoding2[FeatureSet_MessageEncoding2["MESSAGE_ENCODING_UNKNOWN"] = 0] = "MESSAGE_ENCODING_UNKNOWN";
  FeatureSet_MessageEncoding2[FeatureSet_MessageEncoding2["LENGTH_PREFIXED"] = 1] = "LENGTH_PREFIXED";
  FeatureSet_MessageEncoding2[FeatureSet_MessageEncoding2["DELIMITED"] = 2] = "DELIMITED";
})(FeatureSet_MessageEncoding || (FeatureSet_MessageEncoding = {}));
var FeatureSet_JsonFormat;
(function(FeatureSet_JsonFormat2) {
  FeatureSet_JsonFormat2[FeatureSet_JsonFormat2["JSON_FORMAT_UNKNOWN"] = 0] = "JSON_FORMAT_UNKNOWN";
  FeatureSet_JsonFormat2[FeatureSet_JsonFormat2["ALLOW"] = 1] = "ALLOW";
  FeatureSet_JsonFormat2[FeatureSet_JsonFormat2["LEGACY_BEST_EFFORT"] = 2] = "LEGACY_BEST_EFFORT";
})(FeatureSet_JsonFormat || (FeatureSet_JsonFormat = {}));
var FeatureSet_EnforceNamingStyle;
(function(FeatureSet_EnforceNamingStyle2) {
  FeatureSet_EnforceNamingStyle2[FeatureSet_EnforceNamingStyle2["ENFORCE_NAMING_STYLE_UNKNOWN"] = 0] = "ENFORCE_NAMING_STYLE_UNKNOWN";
  FeatureSet_EnforceNamingStyle2[FeatureSet_EnforceNamingStyle2["STYLE2024"] = 1] = "STYLE2024";
  FeatureSet_EnforceNamingStyle2[FeatureSet_EnforceNamingStyle2["STYLE_LEGACY"] = 2] = "STYLE_LEGACY";
})(FeatureSet_EnforceNamingStyle || (FeatureSet_EnforceNamingStyle = {}));
var GeneratedCodeInfo_Annotation_Semantic;
(function(GeneratedCodeInfo_Annotation_Semantic2) {
  GeneratedCodeInfo_Annotation_Semantic2[GeneratedCodeInfo_Annotation_Semantic2["NONE"] = 0] = "NONE";
  GeneratedCodeInfo_Annotation_Semantic2[GeneratedCodeInfo_Annotation_Semantic2["SET"] = 1] = "SET";
  GeneratedCodeInfo_Annotation_Semantic2[GeneratedCodeInfo_Annotation_Semantic2["ALIAS"] = 2] = "ALIAS";
})(GeneratedCodeInfo_Annotation_Semantic || (GeneratedCodeInfo_Annotation_Semantic = {}));
var Edition;
(function(Edition2) {
  Edition2[Edition2["EDITION_UNKNOWN"] = 0] = "EDITION_UNKNOWN";
  Edition2[Edition2["EDITION_LEGACY"] = 900] = "EDITION_LEGACY";
  Edition2[Edition2["EDITION_PROTO2"] = 998] = "EDITION_PROTO2";
  Edition2[Edition2["EDITION_PROTO3"] = 999] = "EDITION_PROTO3";
  Edition2[Edition2["EDITION_2023"] = 1000] = "EDITION_2023";
  Edition2[Edition2["EDITION_2024"] = 1001] = "EDITION_2024";
  Edition2[Edition2["EDITION_UNSTABLE"] = 9999] = "EDITION_UNSTABLE";
  Edition2[Edition2["EDITION_1_TEST_ONLY"] = 1] = "EDITION_1_TEST_ONLY";
  Edition2[Edition2["EDITION_2_TEST_ONLY"] = 2] = "EDITION_2_TEST_ONLY";
  Edition2[Edition2["EDITION_99997_TEST_ONLY"] = 99997] = "EDITION_99997_TEST_ONLY";
  Edition2[Edition2["EDITION_99998_TEST_ONLY"] = 99998] = "EDITION_99998_TEST_ONLY";
  Edition2[Edition2["EDITION_99999_TEST_ONLY"] = 99999] = "EDITION_99999_TEST_ONLY";
  Edition2[Edition2["EDITION_MAX"] = 2147483647] = "EDITION_MAX";
})(Edition || (Edition = {}));
var SymbolVisibility;
(function(SymbolVisibility2) {
  SymbolVisibility2[SymbolVisibility2["VISIBILITY_UNSET"] = 0] = "VISIBILITY_UNSET";
  SymbolVisibility2[SymbolVisibility2["VISIBILITY_LOCAL"] = 1] = "VISIBILITY_LOCAL";
  SymbolVisibility2[SymbolVisibility2["VISIBILITY_EXPORT"] = 2] = "VISIBILITY_EXPORT";
})(SymbolVisibility || (SymbolVisibility = {}));

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/from-binary.js
var readDefaults = {
  readUnknownFields: true
};
function makeReadOptions(options) {
  return options ? Object.assign(Object.assign({}, readDefaults), options) : readDefaults;
}
function fromBinary(schema, bytes, options) {
  const msg = reflect(schema, undefined, false);
  readMessage(msg, new BinaryReader(bytes), makeReadOptions(options), false, bytes.byteLength);
  return msg.message;
}
function readMessage(message, reader, options, delimited, lengthOrDelimitedFieldNo) {
  var _a;
  const end = delimited ? reader.len : reader.pos + lengthOrDelimitedFieldNo;
  let fieldNo;
  let wireType;
  const unknownFields = (_a = message.getUnknown()) !== null && _a !== undefined ? _a : [];
  while (reader.pos < end) {
    [fieldNo, wireType] = reader.tag();
    if (delimited && wireType == WireType.EndGroup) {
      break;
    }
    const field = message.findNumber(fieldNo);
    if (!field) {
      const data = reader.skip(wireType, fieldNo);
      if (options.readUnknownFields) {
        unknownFields.push({ no: fieldNo, wireType, data });
      }
      continue;
    }
    readField(message, reader, field, wireType, options);
  }
  if (delimited) {
    if (wireType != WireType.EndGroup || fieldNo !== lengthOrDelimitedFieldNo) {
      throw new Error("invalid end group tag");
    }
  }
  if (unknownFields.length > 0) {
    message.setUnknown(unknownFields);
  }
}
function readField(message, reader, field, wireType, options) {
  var _a;
  switch (field.fieldKind) {
    case "scalar":
      message.set(field, readScalar(reader, field.scalar));
      break;
    case "enum":
      const val = readScalar(reader, ScalarType.INT32);
      if (field.enum.open) {
        message.set(field, val);
      } else {
        const ok = field.enum.values.some((v) => v.number === val);
        if (ok) {
          message.set(field, val);
        } else if (options.readUnknownFields) {
          const bytes = [];
          varint32write(val, bytes);
          const unknownFields = (_a = message.getUnknown()) !== null && _a !== undefined ? _a : [];
          unknownFields.push({
            no: field.number,
            wireType,
            data: new Uint8Array(bytes)
          });
          message.setUnknown(unknownFields);
        }
      }
      break;
    case "message":
      message.set(field, readMessageField(reader, options, field, message.get(field)));
      break;
    case "list":
      readListField(reader, wireType, message.get(field), options);
      break;
    case "map":
      readMapEntry(reader, message.get(field), options);
      break;
  }
}
function readMapEntry(reader, map, options) {
  const field = map.field();
  let key;
  let val;
  const len = reader.uint32();
  const end = reader.pos + len;
  while (reader.pos < end) {
    const [fieldNo] = reader.tag();
    switch (fieldNo) {
      case 1:
        key = readScalar(reader, field.mapKey);
        break;
      case 2:
        switch (field.mapKind) {
          case "scalar":
            val = readScalar(reader, field.scalar);
            break;
          case "enum":
            val = reader.int32();
            break;
          case "message":
            val = readMessageField(reader, options, field);
            break;
        }
        break;
    }
  }
  if (key === undefined) {
    key = scalarZeroValue(field.mapKey, false);
  }
  if (val === undefined) {
    switch (field.mapKind) {
      case "scalar":
        val = scalarZeroValue(field.scalar, false);
        break;
      case "enum":
        val = field.enum.values[0].number;
        break;
      case "message":
        val = reflect(field.message, undefined, false);
        break;
    }
  }
  map.set(key, val);
}
function readListField(reader, wireType, list, options) {
  var _a;
  const field = list.field();
  if (field.listKind === "message") {
    list.add(readMessageField(reader, options, field));
    return;
  }
  const scalarType = (_a = field.scalar) !== null && _a !== undefined ? _a : ScalarType.INT32;
  const packed = wireType == WireType.LengthDelimited && scalarType != ScalarType.STRING && scalarType != ScalarType.BYTES;
  if (!packed) {
    list.add(readScalar(reader, scalarType));
    return;
  }
  const e = reader.uint32() + reader.pos;
  while (reader.pos < e) {
    list.add(readScalar(reader, scalarType));
  }
}
function readMessageField(reader, options, field, mergeMessage) {
  const delimited = field.delimitedEncoding;
  const message = mergeMessage !== null && mergeMessage !== undefined ? mergeMessage : reflect(field.message, undefined, false);
  readMessage(message, reader, options, delimited, delimited ? field.number : reader.uint32());
  return message;
}
function readScalar(reader, type) {
  switch (type) {
    case ScalarType.STRING:
      return reader.string();
    case ScalarType.BOOL:
      return reader.bool();
    case ScalarType.DOUBLE:
      return reader.double();
    case ScalarType.FLOAT:
      return reader.float();
    case ScalarType.INT32:
      return reader.int32();
    case ScalarType.INT64:
      return reader.int64();
    case ScalarType.UINT64:
      return reader.uint64();
    case ScalarType.FIXED64:
      return reader.fixed64();
    case ScalarType.BYTES:
      return reader.bytes();
    case ScalarType.FIXED32:
      return reader.fixed32();
    case ScalarType.SFIXED32:
      return reader.sfixed32();
    case ScalarType.SFIXED64:
      return reader.sfixed64();
    case ScalarType.SINT64:
      return reader.sint64();
    case ScalarType.UINT32:
      return reader.uint32();
    case ScalarType.SINT32:
      return reader.sint32();
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/codegenv2/file.js
function fileDesc(b64, imports) {
  var _a;
  const root = fromBinary(FileDescriptorProtoSchema, base64Decode(b64));
  root.messageType.forEach(restoreJsonNames);
  root.dependency = (_a = imports === null || imports === undefined ? undefined : imports.map((f) => f.proto.name)) !== null && _a !== undefined ? _a : [];
  const reg = createFileRegistry(root, (protoFileName) => imports === null || imports === undefined ? undefined : imports.find((f) => f.proto.name === protoFileName));
  return reg.getFile(root.name);
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/to-binary.js
var LEGACY_REQUIRED2 = 3;
var writeDefaults = {
  writeUnknownFields: true
};
function makeWriteOptions(options) {
  return options ? Object.assign(Object.assign({}, writeDefaults), options) : writeDefaults;
}
function toBinary(schema, message, options) {
  return writeFields(new BinaryWriter, makeWriteOptions(options), reflect(schema, message)).finish();
}
function writeFields(writer, opts, msg) {
  var _a;
  for (const f of msg.sortedFields) {
    if (!msg.isSet(f)) {
      if (f.presence == LEGACY_REQUIRED2) {
        throw new Error(`cannot encode ${f} to binary: required field not set`);
      }
      continue;
    }
    writeField(writer, opts, msg, f);
  }
  if (opts.writeUnknownFields) {
    for (const { no, wireType, data } of (_a = msg.getUnknown()) !== null && _a !== undefined ? _a : []) {
      writer.tag(no, wireType).raw(data);
    }
  }
  return writer;
}
function writeField(writer, opts, msg, field) {
  var _a;
  switch (field.fieldKind) {
    case "scalar":
    case "enum":
      writeScalar(writer, msg.desc.typeName, field.name, (_a = field.scalar) !== null && _a !== undefined ? _a : ScalarType.INT32, field.number, msg.get(field));
      break;
    case "list":
      writeListField(writer, opts, field, msg.get(field));
      break;
    case "message":
      writeMessageField(writer, opts, field, msg.get(field));
      break;
    case "map":
      for (const [key, val] of msg.get(field)) {
        writeMapEntry(writer, opts, field, key, val);
      }
      break;
  }
}
function writeScalar(writer, msgName, fieldName, scalarType, fieldNo, value) {
  writeScalarValue(writer.tag(fieldNo, writeTypeOfScalar(scalarType)), msgName, fieldName, scalarType, value);
}
function writeMessageField(writer, opts, field, message) {
  if (field.delimitedEncoding) {
    writeFields(writer.tag(field.number, WireType.StartGroup), opts, message).tag(field.number, WireType.EndGroup);
  } else {
    writeFields(writer.tag(field.number, WireType.LengthDelimited).fork(), opts, message).join();
  }
}
function writeListField(writer, opts, field, list) {
  var _a;
  if (field.listKind == "message") {
    for (const item of list) {
      writeMessageField(writer, opts, field, item);
    }
    return;
  }
  const scalarType = (_a = field.scalar) !== null && _a !== undefined ? _a : ScalarType.INT32;
  if (field.packed) {
    if (!list.size) {
      return;
    }
    writer.tag(field.number, WireType.LengthDelimited).fork();
    for (const item of list) {
      writeScalarValue(writer, field.parent.typeName, field.name, scalarType, item);
    }
    writer.join();
    return;
  }
  for (const item of list) {
    writeScalar(writer, field.parent.typeName, field.name, scalarType, field.number, item);
  }
}
function writeMapEntry(writer, opts, field, key, value) {
  var _a;
  writer.tag(field.number, WireType.LengthDelimited).fork();
  writeScalar(writer, field.parent.typeName, field.name, field.mapKey, 1, key);
  switch (field.mapKind) {
    case "scalar":
    case "enum":
      writeScalar(writer, field.parent.typeName, field.name, (_a = field.scalar) !== null && _a !== undefined ? _a : ScalarType.INT32, 2, value);
      break;
    case "message":
      writeFields(writer.tag(2, WireType.LengthDelimited).fork(), opts, value).join();
      break;
  }
  writer.join();
}
function writeScalarValue(writer, msgName, fieldName, type, value) {
  try {
    switch (type) {
      case ScalarType.STRING:
        writer.string(value);
        break;
      case ScalarType.BOOL:
        writer.bool(value);
        break;
      case ScalarType.DOUBLE:
        writer.double(value);
        break;
      case ScalarType.FLOAT:
        writer.float(value);
        break;
      case ScalarType.INT32:
        writer.int32(value);
        break;
      case ScalarType.INT64:
        writer.int64(value);
        break;
      case ScalarType.UINT64:
        writer.uint64(value);
        break;
      case ScalarType.FIXED64:
        writer.fixed64(value);
        break;
      case ScalarType.BYTES:
        writer.bytes(value);
        break;
      case ScalarType.FIXED32:
        writer.fixed32(value);
        break;
      case ScalarType.SFIXED32:
        writer.sfixed32(value);
        break;
      case ScalarType.SFIXED64:
        writer.sfixed64(value);
        break;
      case ScalarType.SINT64:
        writer.sint64(value);
        break;
      case ScalarType.UINT32:
        writer.uint32(value);
        break;
      case ScalarType.SINT32:
        writer.sint32(value);
        break;
    }
  } catch (e) {
    if (e instanceof Error) {
      throw new Error(`cannot encode field ${msgName}.${fieldName} to binary: ${e.message}`);
    }
    throw e;
  }
}
function writeTypeOfScalar(type) {
  switch (type) {
    case ScalarType.BYTES:
    case ScalarType.STRING:
      return WireType.LengthDelimited;
    case ScalarType.DOUBLE:
    case ScalarType.FIXED64:
    case ScalarType.SFIXED64:
      return WireType.Bit64;
    case ScalarType.FIXED32:
    case ScalarType.SFIXED32:
    case ScalarType.FLOAT:
      return WireType.Bit32;
    default:
      return WireType.Varint;
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/wkt/any.js
function anyIs(any, descOrTypeName) {
  if (any.typeUrl === "") {
    return false;
  }
  const want = typeof descOrTypeName == "string" ? descOrTypeName : descOrTypeName.typeName;
  const got = typeUrlToName(any.typeUrl);
  return want === got;
}
function anyUnpack(any, registryOrMessageDesc) {
  if (any.typeUrl === "") {
    return;
  }
  const desc = registryOrMessageDesc.kind == "message" ? registryOrMessageDesc : registryOrMessageDesc.getMessage(typeUrlToName(any.typeUrl));
  if (!desc || !anyIs(any, desc)) {
    return;
  }
  return fromBinary(desc, any.value);
}
function typeUrlToName(url) {
  const slash = url.lastIndexOf("/");
  const name = slash >= 0 ? url.substring(slash + 1) : url;
  if (!name.length) {
    throw new Error(`invalid type url: ${url}`);
  }
  return name;
}

// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/extensions.js
function getExtension(message, extension) {
  assertExtendee(extension, message);
  const ufs = filterUnknownFields(message.$unknown, extension);
  const [container, field, get] = createExtensionContainer(extension);
  for (const uf of ufs) {
    readField(container, new BinaryReader(uf.data), field, uf.wireType, {
      readUnknownFields: true
    });
  }
  return get();
}
function filterUnknownFields(unknownFields, extension) {
  if (unknownFields === undefined)
    return [];
  if (extension.fieldKind === "enum" || extension.fieldKind === "scalar") {
    for (let i = unknownFields.length - 1;i >= 0; --i) {
      if (unknownFields[i].no == extension.number) {
        return [unknownFields[i]];
      }
    }
    return [];
  }
  return unknownFields.filter((uf) => uf.no === extension.number);
}
function createExtensionContainer(extension, value) {
  const localName = extension.typeName;
  const field = Object.assign(Object.assign({}, extension), { kind: "field", parent: extension.extendee, localName });
  const desc = Object.assign(Object.assign({}, extension.extendee), { fields: [field], members: [field], oneofs: [] });
  const container = create(desc, value !== undefined ? { [localName]: value } : undefined);
  return [
    reflect(desc, container),
    field,
    () => {
      const value2 = container[localName];
      if (value2 === undefined) {
        const desc2 = extension.message;
        if (isWrapperDesc(desc2)) {
          return scalarZeroValue(desc2.fields[0].scalar, desc2.fields[0].longAsString);
        }
        return create(desc2);
      }
      return value2;
    }
  ];
}
function assertExtendee(extension, message) {
  if (extension.extendee.typeName != message.$typeName) {
    throw new Error(`extension ${extension.typeName} can only be applied to message ${extension.extendee.typeName}`);
  }
}
// node_modules/.bun/@bufbuild+protobuf@2.11.0/node_modules/@bufbuild/protobuf/dist/esm/to-json.js
var LEGACY_REQUIRED3 = 3;
var IMPLICIT4 = 2;
var jsonWriteDefaults = {
  alwaysEmitImplicit: false,
  enumAsInteger: false,
  useProtoFieldName: false
};
function makeWriteOptions2(options) {
  return options ? Object.assign(Object.assign({}, jsonWriteDefaults), options) : jsonWriteDefaults;
}
function toJson(schema, message, options) {
  return reflectToJson(reflect(schema, message), makeWriteOptions2(options));
}
function reflectToJson(msg, opts) {
  var _a;
  const wktJson = tryWktToJson(msg, opts);
  if (wktJson !== undefined)
    return wktJson;
  const json = {};
  for (const f of msg.sortedFields) {
    if (!msg.isSet(f)) {
      if (f.presence == LEGACY_REQUIRED3) {
        throw new Error(`cannot encode ${f} to JSON: required field not set`);
      }
      if (!opts.alwaysEmitImplicit || f.presence !== IMPLICIT4) {
        continue;
      }
    }
    const jsonValue = fieldToJson(f, msg.get(f), opts);
    if (jsonValue !== undefined) {
      json[jsonName(f, opts)] = jsonValue;
    }
  }
  if (opts.registry) {
    const tagSeen = new Set;
    for (const { no } of (_a = msg.getUnknown()) !== null && _a !== undefined ? _a : []) {
      if (!tagSeen.has(no)) {
        tagSeen.add(no);
        const extension = opts.registry.getExtensionFor(msg.desc, no);
        if (!extension) {
          continue;
        }
        const value = getExtension(msg.message, extension);
        const [container, field] = createExtensionContainer(extension, value);
        const jsonValue = fieldToJson(field, container.get(field), opts);
        if (jsonValue !== undefined) {
          json[extension.jsonName] = jsonValue;
        }
      }
    }
  }
  return json;
}
function fieldToJson(f, val, opts) {
  switch (f.fieldKind) {
    case "scalar":
      return scalarToJson(f, val);
    case "message":
      return reflectToJson(val, opts);
    case "enum":
      return enumToJsonInternal(f.enum, val, opts.enumAsInteger);
    case "list":
      return listToJson(val, opts);
    case "map":
      return mapToJson(val, opts);
  }
}
function mapToJson(map, opts) {
  const f = map.field();
  const jsonObj = {};
  switch (f.mapKind) {
    case "scalar":
      for (const [entryKey, entryValue] of map) {
        jsonObj[entryKey] = scalarToJson(f, entryValue);
      }
      break;
    case "message":
      for (const [entryKey, entryValue] of map) {
        jsonObj[entryKey] = reflectToJson(entryValue, opts);
      }
      break;
    case "enum":
      for (const [entryKey, entryValue] of map) {
        jsonObj[entryKey] = enumToJsonInternal(f.enum, entryValue, opts.enumAsInteger);
      }
      break;
  }
  return opts.alwaysEmitImplicit || map.size > 0 ? jsonObj : undefined;
}
function listToJson(list, opts) {
  const f = list.field();
  const jsonArr = [];
  switch (f.listKind) {
    case "scalar":
      for (const item of list) {
        jsonArr.push(scalarToJson(f, item));
      }
      break;
    case "enum":
      for (const item of list) {
        jsonArr.push(enumToJsonInternal(f.enum, item, opts.enumAsInteger));
      }
      break;
    case "message":
      for (const item of list) {
        jsonArr.push(reflectToJson(item, opts));
      }
      break;
  }
  return opts.alwaysEmitImplicit || jsonArr.length > 0 ? jsonArr : undefined;
}
function enumToJsonInternal(desc, value, enumAsInteger) {
  var _a;
  if (typeof value != "number") {
    throw new Error(`cannot encode ${desc} to JSON: expected number, got ${formatVal(value)}`);
  }
  if (desc.typeName == "google.protobuf.NullValue") {
    return null;
  }
  if (enumAsInteger) {
    return value;
  }
  const val = desc.value[value];
  return (_a = val === null || val === undefined ? undefined : val.name) !== null && _a !== undefined ? _a : value;
}
function scalarToJson(field, value) {
  var _a, _b, _c, _d, _e, _f;
  switch (field.scalar) {
    case ScalarType.INT32:
    case ScalarType.SFIXED32:
    case ScalarType.SINT32:
    case ScalarType.FIXED32:
    case ScalarType.UINT32:
      if (typeof value != "number") {
        throw new Error(`cannot encode ${field} to JSON: ${(_a = checkField(field, value)) === null || _a === undefined ? undefined : _a.message}`);
      }
      return value;
    case ScalarType.FLOAT:
    case ScalarType.DOUBLE:
      if (typeof value != "number") {
        throw new Error(`cannot encode ${field} to JSON: ${(_b = checkField(field, value)) === null || _b === undefined ? undefined : _b.message}`);
      }
      if (Number.isNaN(value))
        return "NaN";
      if (value === Number.POSITIVE_INFINITY)
        return "Infinity";
      if (value === Number.NEGATIVE_INFINITY)
        return "-Infinity";
      return value;
    case ScalarType.STRING:
      if (typeof value != "string") {
        throw new Error(`cannot encode ${field} to JSON: ${(_c = checkField(field, value)) === null || _c === undefined ? undefined : _c.message}`);
      }
      return value;
    case ScalarType.BOOL:
      if (typeof value != "boolean") {
        throw new Error(`cannot encode ${field} to JSON: ${(_d = checkField(field, value)) === null || _d === undefined ? undefined : _d.message}`);
      }
      return value;
    case ScalarType.UINT64:
    case ScalarType.FIXED64:
    case ScalarType.INT64:
    case ScalarType.SFIXED64:
    case ScalarType.SINT64:
      if (typeof value != "bigint" && typeof value != "string") {
        throw new Error(`cannot encode ${field} to JSON: ${(_e = checkField(field, value)) === null || _e === undefined ? undefined : _e.message}`);
      }
      return value.toString();
    case ScalarType.BYTES:
      if (value instanceof Uint8Array) {
        return base64Encode(value);
      }
      throw new Error(`cannot encode ${field} to JSON: ${(_f = checkField(field, value)) === null || _f === undefined ? undefined : _f.message}`);
  }
}
function jsonName(f, opts) {
  return opts.useProtoFieldName ? f.name : f.jsonName;
}
function tryWktToJson(msg, opts) {
  if (!msg.desc.typeName.startsWith("google.protobuf.")) {
    return;
  }
  switch (msg.desc.typeName) {
    case "google.protobuf.Any":
      return anyToJson(msg.message, opts);
    case "google.protobuf.Timestamp":
      return timestampToJson(msg.message);
    case "google.protobuf.Duration":
      return durationToJson(msg.message);
    case "google.protobuf.FieldMask":
      return fieldMaskToJson(msg.message);
    case "google.protobuf.Struct":
      return structToJson(msg.message);
    case "google.protobuf.Value":
      return valueToJson(msg.message);
    case "google.protobuf.ListValue":
      return listValueToJson(msg.message);
    default:
      if (isWrapperDesc(msg.desc)) {
        const valueField = msg.desc.fields[0];
        return scalarToJson(valueField, msg.get(valueField));
      }
      return;
  }
}
function anyToJson(val, opts) {
  if (val.typeUrl === "") {
    return {};
  }
  const { registry } = opts;
  let message;
  let desc;
  if (registry) {
    message = anyUnpack(val, registry);
    if (message) {
      desc = registry.getMessage(message.$typeName);
    }
  }
  if (!desc || !message) {
    throw new Error(`cannot encode message ${val.$typeName} to JSON: "${val.typeUrl}" is not in the type registry`);
  }
  let json = reflectToJson(reflect(desc, message), opts);
  if (desc.typeName.startsWith("google.protobuf.") || json === null || Array.isArray(json) || typeof json !== "object") {
    json = { value: json };
  }
  json["@type"] = val.typeUrl;
  return json;
}
function durationToJson(val) {
  const seconds = Number(val.seconds);
  const nanos = val.nanos;
  if (seconds > 315576000000 || seconds < -315576000000) {
    throw new Error(`cannot encode message ${val.$typeName} to JSON: value out of range`);
  }
  if (seconds > 0 && nanos < 0 || seconds < 0 && nanos > 0) {
    throw new Error(`cannot encode message ${val.$typeName} to JSON: nanos sign must match seconds sign`);
  }
  let text = val.seconds.toString();
  if (nanos !== 0) {
    let nanosStr = Math.abs(nanos).toString();
    nanosStr = "0".repeat(9 - nanosStr.length) + nanosStr;
    if (nanosStr.substring(3) === "000000") {
      nanosStr = nanosStr.substring(0, 3);
    } else if (nanosStr.substring(6) === "000") {
      nanosStr = nanosStr.substring(0, 6);
    }
    text += "." + nanosStr;
    if (nanos < 0 && seconds == 0) {
      text = "-" + text;
    }
  }
  return text + "s";
}
function fieldMaskToJson(val) {
  return val.paths.map((p) => {
    if (protoSnakeCase(protoCamelCase(p)) !== p) {
      throw new Error(`cannot encode message ${val.$typeName} to JSON: lowerCamelCase of path name "${p}" is irreversible`);
    }
    return protoCamelCase(p);
  }).join(",");
}
function structToJson(val) {
  const json = {};
  for (const [k, v] of Object.entries(val.fields)) {
    json[k] = valueToJson(v);
  }
  return json;
}
function valueToJson(val) {
  switch (val.kind.case) {
    case "nullValue":
      return null;
    case "numberValue":
      if (!Number.isFinite(val.kind.value)) {
        throw new Error(`${val.$typeName} cannot be NaN or Infinity`);
      }
      return val.kind.value;
    case "boolValue":
      return val.kind.value;
    case "stringValue":
      return val.kind.value;
    case "structValue":
      return structToJson(val.kind.value);
    case "listValue":
      return listValueToJson(val.kind.value);
    default:
      throw new Error(`${val.$typeName} must have a value`);
  }
}
function listValueToJson(val) {
  return val.values.map(valueToJson);
}
function timestampToJson(val) {
  const ms = Number(val.seconds) * 1000;
  if (ms < Date.parse("0001-01-01T00:00:00Z") || ms > Date.parse("9999-12-31T23:59:59Z")) {
    throw new Error(`cannot encode message ${val.$typeName} to JSON: must be from 0001-01-01T00:00:00Z to 9999-12-31T23:59:59Z inclusive`);
  }
  if (val.nanos < 0) {
    throw new Error(`cannot encode message ${val.$typeName} to JSON: nanos must not be negative`);
  }
  if (val.nanos > 999999999) {
    throw new Error(`cannot encode message ${val.$typeName} to JSON: nanos must not be greater than 99999999`);
  }
  let z = "Z";
  if (val.nanos > 0) {
    const nanosStr = (val.nanos + 1e9).toString().substring(1);
    if (nanosStr.substring(3) === "000000") {
      z = "." + nanosStr.substring(0, 3) + "Z";
    } else if (nanosStr.substring(6) === "000") {
      z = "." + nanosStr.substring(0, 6) + "Z";
    } else {
      z = "." + nanosStr + "Z";
    }
  }
  return new Date(ms).toISOString().replace(".000Z", z);
}
// proto/typescript/src/gen/bundle_pb.ts
var file_bundle = /* @__PURE__ */ fileDesc("CgxidW5kbGUucHJvdG8SD2hlbG1yLmJ1bmRsZS52MCKVAgoGQnVuZGxlEikKBWltYWdlGAEgASgLMhouaGVsbXIuYnVuZGxlLnYwLkltYWdlU3BlYxItCgdzYW5kYm94GAIgASgLMhwuaGVsbXIuYnVuZGxlLnYwLlNhbmRib3hTcGVjEicKBHRhc2sYAyABKAsyGS5oZWxtci5idW5kbGUudjAuVGFza1NwZWMSOgoKc3ViX2ltYWdlcxgEIAMoCzImLmhlbG1yLmJ1bmRsZS52MC5CdW5kbGUuU3ViSW1hZ2VzRW50cnkaTAoOU3ViSW1hZ2VzRW50cnkSCwoDa2V5GAEgASgJEikKBXZhbHVlGAIgASgLMhouaGVsbXIuYnVuZGxlLnYwLkltYWdlU3BlYzoCOAEiLAoIUGxhdGZvcm0SCgoCb3MYASABKAkSFAoMYXJjaGl0ZWN0dXJlGAIgASgJInsKCUltYWdlU3BlYxIWCg5mb3JtYXRfdmVyc2lvbhgBIAEoDRIrCghwbGF0Zm9ybRgCIAEoCzIZLmhlbG1yLmJ1bmRsZS52MC5QbGF0Zm9ybRIpCgVzdGVwcxgDIAMoCzIaLmhlbG1yLmJ1bmRsZS52MC5JbWFnZVN0ZXAiiwMKCUltYWdlU3RlcBIlCgRmcm9tGAEgASgLMhUuaGVsbXIuYnVuZGxlLnYwLkZyb21IABIjCgNydW4YAiABKAsyFC5oZWxtci5idW5kbGUudjAuUnVuSAASOwoQY29weV9zb3VyY2VfZmlsZRgFIAEoCzIfLmhlbG1yLmJ1bmRsZS52MC5Db3B5U291cmNlRmlsZUgAEjkKD2NvcHlfc291cmNlX2RpchgGIAEoCzIeLmhlbG1yLmJ1bmRsZS52MC5Db3B5U291cmNlRGlySAASOQoPY29weV9mcm9tX2ltYWdlGAcgASgLMh4uaGVsbXIuYnVuZGxlLnYwLkNvcHlGcm9tSW1hZ2VIABIrCgd3b3JrZGlyGAggASgLMhguaGVsbXIuYnVuZGxlLnYwLldvcmtkaXJIABIlCgR1c2VyGAkgASgLMhUuaGVsbXIuYnVuZGxlLnYwLlVzZXJIABIjCgNlbnYYCiABKAsyFC5oZWxtci5idW5kbGUudjAuRW52SABCBgoEa2luZCITCgRGcm9tEgsKA3JlZhgBIAEoCSKJAQoDUnVuEgwKBGFyZ3YYASADKAkSOAoMY2FjaGVfbW91bnRzGAIgAygLMiIuaGVsbXIuYnVuZGxlLnYwLkNhY2hlTW91bnRCaW5kaW5nEjoKDXNlY3JldF9tb3VudHMYAyADKAsyIy5oZWxtci5idW5kbGUudjAuU2VjcmV0TW91bnRCaW5kaW5nIh0KDVNvdXJjZUZpbGVSZWYSDAoEcGF0aBgBIAEoCSIsCgxTb3VyY2VEaXJSZWYSDAoEcGF0aBgBIAEoCRIOCgZpZ25vcmUYAiADKAkiXgoOQ29weVNvdXJjZUZpbGUSCwoDZHN0GAEgASgJEi8KB3NyY19yZWYYAiABKAsyHi5oZWxtci5idW5kbGUudjAuU291cmNlRmlsZVJlZhIOCgZkaWdlc3QYAyABKAkicQoNQ29weVNvdXJjZURpchILCgNkc3QYASABKAkSLgoHc3JjX3JlZhgCIAEoCzIdLmhlbG1yLmJ1bmRsZS52MC5Tb3VyY2VEaXJSZWYSEwoLdHJlZV9kaWdlc3QYAyABKAkSDgoGaWdub3JlGAQgAygJIkUKDUNvcHlGcm9tSW1hZ2USCwoDZHN0GAEgASgJEhUKDXNyY19pbWFnZV9rZXkYAiABKAkSEAoIc3JjX3BhdGgYAyABKAkiFwoHV29ya2RpchIMCgRwYXRoGAEgASgJIhQKBFVzZXISDAoEbmFtZRgBIAEoCSIhCgNFbnYSCwoDa2V5GAEgASgJEg0KBXZhbHVlGAIgASgJIkMKEUNhY2hlTW91bnRCaW5kaW5nEgsKA2RzdBgBIAEoCRIQCghjYWNoZV9pZBgCIAEoCRIPCgdzaGFyaW5nGAMgASgJIhkKCVNlY3JldFJlZhIMCgRuYW1lGAEgASgJIlEKElNlY3JldE1vdW50QmluZGluZxILCgNkc3QYASABKAkSLgoKc2VjcmV0X3JlZhgCIAEoCzIaLmhlbG1yLmJ1bmRsZS52MC5TZWNyZXRSZWYitgEKC1NhbmRib3hTcGVjEgoKAmlkGAEgASgJEjsKCXdvcmtzcGFjZRgCIAEoCzIoLmhlbG1yLmJ1bmRsZS52MC5Xb3Jrc3BhY2VSdW50aW1lQmluZGluZxItCglyZXNvdXJjZXMYAyABKAsyGi5oZWxtci5idW5kbGUudjAuUmVzb3VyY2VzEi8KB25ldHdvcmsYBCABKAsyHi5oZWxtci5idW5kbGUudjAuTmV0d29ya1BvbGljeSItChdXb3Jrc3BhY2VSdW50aW1lQmluZGluZxISCgptb3VudF9wYXRoGAEgASgJIjYKCVJlc291cmNlcxILCgNjcHUYASABKA0SDgoGbWVtb3J5GAIgASgJEgwKBGRpc2sYAyABKAkiPgoNTmV0d29ya1BvbGljeRIQCghpbnRlcm5ldBgBIAEoCBINCgVhbGxvdxgCIAMoCRIMCgRkZW55GAMgAygJIk4KD1NlY3JldFBsYWNlbWVudBIMCgRuYW1lGAEgASgJEi0KCXBsYWNlbWVudBgCIAEoCzIaLmhlbG1yLmJ1bmRsZS52MC5QbGFjZW1lbnQinwEKCVBsYWNlbWVudBIsCgNlbnYYASABKAsyHS5oZWxtci5idW5kbGUudjAuRW52UGxhY2VtZW50SAASLgoEZmlsZRgCIAEoCzIeLmhlbG1yLmJ1bmRsZS52MC5GaWxlUGxhY2VtZW50SAASLAoDZGlyGAMgASgLMh0uaGVsbXIuYnVuZGxlLnYwLkRpclBsYWNlbWVudEgAQgYKBGtpbmQiHAoMRW52UGxhY2VtZW50EgwKBG5hbWUYASABKAkiVwoNRmlsZVBsYWNlbWVudBIMCgRwYXRoGAEgASgJEhEKBG1vZGUYAiABKAlIAIgBARISCgVvd25lchgDIAEoCUgBiAEBQgcKBV9tb2RlQggKBl9vd25lciJWCgxEaXJQbGFjZW1lbnQSDAoEcGF0aBgBIAEoCRIRCgRtb2RlGAIgASgJSACIAQESEgoFb3duZXIYAyABKAlIAYgBAUIHCgVfbW9kZUIICgZfb3duZXIirgIKCFRhc2tTcGVjEgoKAmlkGAEgASgJEhIKCnNhbmRib3hfaWQYAiABKAkSEwoLbW9kdWxlX3BhdGgYAyABKAkSEwoLZXhwb3J0X25hbWUYBCABKAkSHAoUbWF4X2R1cmF0aW9uX3NlY29uZHMYBSABKA0SMQoHc2VjcmV0cxgGIAMoCzIgLmhlbG1yLmJ1bmRsZS52MC5TZWNyZXRQbGFjZW1lbnQSKQoFcXVldWUYByABKAsyGi5oZWxtci5idW5kbGUudjAuUXVldWVTcGVjEgsKA3R0bBgIIAEoCRI0CglzY2hlZHVsZXMYCSADKAsyIS5oZWxtci5idW5kbGUudjAuVGFza1NjaGVkdWxlU3BlYxIZChFyZXRyeV9wb2xpY3lfanNvbhgKIAEoCSJPCglRdWV1ZVNwZWMSDAoEbmFtZRgBIAEoCRIeChFjb25jdXJyZW5jeV9saW1pdBgCIAEoDUgAiAEBQhQKEl9jb25jdXJyZW5jeV9saW1pdCJeChBUYXNrU2NoZWR1bGVTcGVjEgoKAmlkGAEgASgJEgwKBGNyb24YAiABKAkSEAoIdGltZXpvbmUYAyABKAkSEwoGYWN0aXZlGAQgASgISACIAQFCCQoHX2FjdGl2ZUJAWj5naXRodWIuY29tL2hlbG1yZG90ZGV2L2hlbG1yL2ludGVybmFsL3Byb3RvL2J1bmRsZS92MDtidW5kbGV2MGIGcHJvdG8z");
var BundleSchema = /* @__PURE__ */ messageDesc(file_bundle, 0);
var PlatformSchema = /* @__PURE__ */ messageDesc(file_bundle, 1);
var ImageSpecSchema = /* @__PURE__ */ messageDesc(file_bundle, 2);
var ImageStepSchema = /* @__PURE__ */ messageDesc(file_bundle, 3);
var FromSchema = /* @__PURE__ */ messageDesc(file_bundle, 4);
var RunSchema = /* @__PURE__ */ messageDesc(file_bundle, 5);
var SourceFileRefSchema = /* @__PURE__ */ messageDesc(file_bundle, 6);
var SourceDirRefSchema = /* @__PURE__ */ messageDesc(file_bundle, 7);
var CopySourceFileSchema = /* @__PURE__ */ messageDesc(file_bundle, 8);
var CopySourceDirSchema = /* @__PURE__ */ messageDesc(file_bundle, 9);
var CopyFromImageSchema = /* @__PURE__ */ messageDesc(file_bundle, 10);
var WorkdirSchema = /* @__PURE__ */ messageDesc(file_bundle, 11);
var UserSchema = /* @__PURE__ */ messageDesc(file_bundle, 12);
var EnvSchema = /* @__PURE__ */ messageDesc(file_bundle, 13);
var CacheMountBindingSchema = /* @__PURE__ */ messageDesc(file_bundle, 14);
var SecretRefSchema = /* @__PURE__ */ messageDesc(file_bundle, 15);
var SecretMountBindingSchema = /* @__PURE__ */ messageDesc(file_bundle, 16);
var SandboxSpecSchema = /* @__PURE__ */ messageDesc(file_bundle, 17);
var WorkspaceRuntimeBindingSchema = /* @__PURE__ */ messageDesc(file_bundle, 18);
var ResourcesSchema = /* @__PURE__ */ messageDesc(file_bundle, 19);
var NetworkPolicySchema = /* @__PURE__ */ messageDesc(file_bundle, 20);
var SecretPlacementSchema = /* @__PURE__ */ messageDesc(file_bundle, 21);
var PlacementSchema = /* @__PURE__ */ messageDesc(file_bundle, 22);
var EnvPlacementSchema = /* @__PURE__ */ messageDesc(file_bundle, 23);
var FilePlacementSchema = /* @__PURE__ */ messageDesc(file_bundle, 24);
var DirPlacementSchema = /* @__PURE__ */ messageDesc(file_bundle, 25);
var TaskSpecSchema = /* @__PURE__ */ messageDesc(file_bundle, 26);
var QueueSpecSchema = /* @__PURE__ */ messageDesc(file_bundle, 27);
var TaskScheduleSpecSchema = /* @__PURE__ */ messageDesc(file_bundle, 28);
// proto/typescript/src/gen/run_pb.ts
var exports_run_pb = {};
__export(exports_run_pb, {
  file_run: () => file_run,
  WorkspaceArtifactSchema: () => WorkspaceArtifactSchema,
  WaitpointTokenCreateResultSchema: () => WaitpointTokenCreateResultSchema,
  WaitpointTokenCreateRequestedSchema: () => WaitpointTokenCreateRequestedSchema,
  WaitpointRequestedSchema: () => WaitpointRequestedSchema,
  TraceContextSchema: () => TraceContextSchema,
  TaskResultSchema: () => TaskResultSchema,
  TaskErrorSchema: () => TaskErrorSchema,
  SecretInjectSchema: () => SecretInjectSchema,
  RunTaskWorkspaceSchema: () => RunTaskWorkspaceSchema,
  RunTaskRequestSchema: () => RunTaskRequestSchema,
  RunEventSchema: () => RunEventSchema,
  ResumeDecisionSchema: () => ResumeDecisionSchema,
  ResumeConsumedSchema: () => ResumeConsumedSchema,
  ResumeAttachSchema: () => ResumeAttachSchema,
  ResumeAckSchema: () => ResumeAckSchema,
  PlacementSchema: () => PlacementSchema2,
  MetadataUpdatedSchema: () => MetadataUpdatedSchema,
  FilePlacementSchema: () => FilePlacementSchema2,
  EnvPlacementSchema: () => EnvPlacementSchema2,
  DirPlacementSchema: () => DirPlacementSchema2,
  CheckpointPauseRequestSchema: () => CheckpointPauseRequestSchema,
  CheckpointPauseReadySchema: () => CheckpointPauseReadySchema,
  ChannelOutputAppendedSchema: () => ChannelOutputAppendedSchema
});
var file_run = /* @__PURE__ */ fileDesc("CglydW4ucHJvdG8SDGhlbG1yLnJ1bi52MCJ7ChBSdW5UYXNrV29ya3NwYWNlEgwKBHBhdGgYASABKAkSFAoMcHJvamVjdF9wYXRoGAIgASgJEjEKCGFydGlmYWN0GAMgASgLMh8uaGVsbXIucnVuLnYwLldvcmtzcGFjZUFydGlmYWN0EhAKCHdyaXRhYmxlGAUgASgIInIKEVdvcmtzcGFjZUFydGlmYWN0Eg4KBmRpZ2VzdBgBIAEoCRISCgptZWRpYV90eXBlGAIgASgJEhAKCGVuY29kaW5nGAMgASgJEhIKCnNpemVfYnl0ZXMYBCABKAQSEwoLZW50cnlfY291bnQYBSABKA0i/wIKDlJ1blRhc2tSZXF1ZXN0Eg8KB3Rhc2tfaWQYASABKAkSEwoLbW9kdWxlX3BhdGgYAiABKAkSCwoDY3dkGAMgASgJEisKB3NlY3JldHMYBCADKAsyGi5oZWxtci5ydW4udjAuU2VjcmV0SW5qZWN0Eg4KBnJ1bl9pZBgFIAEoCRIUCgxwYXlsb2FkX2pzb24YBiABKAkSMQoJd29ya3NwYWNlGAcgASgLMh4uaGVsbXIucnVuLnYwLlJ1blRhc2tXb3Jrc3BhY2USKQoFdHJhY2UYCCABKAsyGi5oZWxtci5ydW4udjAuVHJhY2VDb250ZXh0EhIKCmF0dGVtcHRfaWQYCSABKAkSFgoOYXR0ZW1wdF9udW1iZXIYCiABKA0SFAoMcnVuX2xlYXNlX2lkGAsgASgJEhgKEHNuYXBzaG90X3ZlcnNpb24YDCABKAQSFwoPdGFza19zZXNzaW9uX2lkGA0gASgJEhQKDHJldHJ5X3JlYXNvbhgOIAEoCSJGCgxUcmFjZUNvbnRleHQSEAoIdHJhY2VfaWQYASABKAkSDwoHc3Bhbl9pZBgCIAEoCRITCgt0cmFjZXBhcmVudBgDIAEoCSJdCgxTZWNyZXRJbmplY3QSDAoEbmFtZRgBIAEoCRIqCglwbGFjZW1lbnQYAiABKAsyFy5oZWxtci5ydW4udjAuUGxhY2VtZW50EhMKC3ZhbHVlX2J5dGVzGAMgASgMIpYBCglQbGFjZW1lbnQSKQoDZW52GAEgASgLMhouaGVsbXIucnVuLnYwLkVudlBsYWNlbWVudEgAEisKBGZpbGUYAiABKAsyGy5oZWxtci5ydW4udjAuRmlsZVBsYWNlbWVudEgAEikKA2RpchgDIAEoCzIaLmhlbG1yLnJ1bi52MC5EaXJQbGFjZW1lbnRIAEIGCgRraW5kIhwKDEVudlBsYWNlbWVudBIMCgRuYW1lGAEgASgJIlcKDUZpbGVQbGFjZW1lbnQSDAoEcGF0aBgBIAEoCRIRCgRtb2RlGAIgASgJSACIAQESEgoFb3duZXIYAyABKAlIAYgBAUIHCgVfbW9kZUIICgZfb3duZXIiVgoMRGlyUGxhY2VtZW50EgwKBHBhdGgYASABKAkSEQoEbW9kZRgCIAEoCUgAiAEBEhIKBW93bmVyGAMgASgJSAGIAQFCBwoFX21vZGVCCAoGX293bmVyIt8DCghSdW5FdmVudBIWCgxzdGRvdXRfY2h1bmsYASABKAxIABIWCgxzdGRlcnJfY2h1bmsYAiABKAxIABITCglsb2dfZW50cnkYAyABKAlIABIvCgt0YXNrX3Jlc3VsdBgEIAEoCzIYLmhlbG1yLnJ1bi52MC5UYXNrUmVzdWx0SAASPwoTd2FpdHBvaW50X3JlcXVlc3RlZBgFIAEoCzIgLmhlbG1yLnJ1bi52MC5XYWl0cG9pbnRSZXF1ZXN0ZWRIABI5ChBtZXRhZGF0YV91cGRhdGVkGAcgASgLMh0uaGVsbXIucnVuLnYwLk1ldGFkYXRhVXBkYXRlZEgAElcKIHdhaXRwb2ludF90b2tlbl9jcmVhdGVfcmVxdWVzdGVkGAggASgLMisuaGVsbXIucnVuLnYwLldhaXRwb2ludFRva2VuQ3JlYXRlUmVxdWVzdGVkSAASRgoXY2hhbm5lbF9vdXRwdXRfYXBwZW5kZWQYCSABKAsyIy5oZWxtci5ydW4udjAuQ2hhbm5lbE91dHB1dEFwcGVuZGVkSAASNwoPcmVzdW1lX2NvbnN1bWVkGAYgASgLMhwuaGVsbXIucnVuLnYwLlJlc3VtZUNvbnN1bWVkSABCBwoFZXZlbnQirgEKClRhc2tSZXN1bHQSEQoJZXhpdF9jb2RlGAEgASgFEhoKDWVycm9yX21lc3NhZ2UYAiABKAlIAIgBARIYCgtvdXRwdXRfanNvbhgDIAEoCUgBiAEBEisKBWVycm9yGAQgASgLMhcuaGVsbXIucnVuLnYwLlRhc2tFcnJvckgCiAEBQhAKDl9lcnJvcl9tZXNzYWdlQg4KDF9vdXRwdXRfanNvbkIICgZfZXJyb3IiYQoJVGFza0Vycm9yEgwKBHR5cGUYASABKAkSDAoEY29kZRgCIAEoCRIPCgdtZXNzYWdlGAMgASgJEhEKCXJldHJ5YWJsZRgEIAEoCBIUCgxkZXRhaWxzX2pzb24YBSABKAki1wEKEldhaXRwb2ludFJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRIMCgRraW5kGAIgASgJEhMKC3BhcmFtc19qc29uGAMgASgJEhoKDW1ldGFkYXRhX2pzb24YBCABKAlIAIgBARIUCgd0aW1lb3V0GAUgASgNSAGIAQESDAoEdGFncxgGIAMoCRIPCgdvcmRpbmFsGAcgASgNEhcKD2FnZ3JlZ2F0ZV9jb3VudBgIIAEoDUIQCg5fbWV0YWRhdGFfanNvbkIKCghfdGltZW91dCK7AQodV2FpdHBvaW50VG9rZW5DcmVhdGVSZXF1ZXN0ZWQSFwoKdGltZW91dF9hdBgBIAEoCUgAiAEBEh8KEnRpbWVvdXRfaW5fc2Vjb25kcxgCIAEoDUgBiAEBEgwKBHRhZ3MYBCADKAkSGgoNbWV0YWRhdGFfanNvbhgFIAEoCUgCiAEBQg0KC190aW1lb3V0X2F0QhUKE190aW1lb3V0X2luX3NlY29uZHNCEAoOX21ldGFkYXRhX2pzb24iqgIKGldhaXRwb2ludFRva2VuQ3JlYXRlUmVzdWx0EgoKAmlkGAEgASgJEhQKDGNhbGxiYWNrX3VybBgCIAEoCRIgChNwdWJsaWNfYWNjZXNzX3Rva2VuGAMgASgJSACIAQESFwoKdGltZW91dF9hdBgEIAEoCUgBiAEBEhMKBnN0YXR1cxgFIAEoCUgCiAEBEgwKBHRhZ3MYBiADKAkSGgoNbWV0YWRhdGFfanNvbhgHIAEoCUgDiAEBEhoKDWVycm9yX21lc3NhZ2UYCSABKAlIBIgBAUIWChRfcHVibGljX2FjY2Vzc190b2tlbkINCgtfdGltZW91dF9hdEIJCgdfc3RhdHVzQhAKDl9tZXRhZGF0YV9qc29uQhAKDl9lcnJvcl9tZXNzYWdlIkUKFkNoZWNrcG9pbnRQYXVzZVJlcXVlc3QSFAoMd2FpdHBvaW50X2lkGAEgASgJEhUKDWNoZWNrcG9pbnRfaWQYAiABKAkiQwoUQ2hlY2twb2ludFBhdXNlUmVhZHkSFAoMd2FpdHBvaW50X2lkGAEgASgJEhUKDWNoZWNrcG9pbnRfaWQYAiABKAkiUQoMUmVzdW1lQXR0YWNoEhUKDWNoZWNrcG9pbnRfaWQYASABKAkSFAoMd2FpdHBvaW50X2lkGAIgASgJEhQKDHJ1bl9sZWFzZV9pZBgDIAEoCSJlCg5SZXN1bWVEZWNpc2lvbhIUCgx3YWl0cG9pbnRfaWQYASABKAkSDAoEa2luZBgCIAEoCRIRCglkYXRhX2pzb24YAyABKAkSHAoUcmVxdWlyZV9jb25zdW1lZF9hY2sYBCABKAgiIQoJUmVzdW1lQWNrEhQKDHdhaXRwb2ludF9pZBgBIAEoCSImCg5SZXN1bWVDb25zdW1lZBIUCgx3YWl0cG9pbnRfaWQYASABKAkinAEKFUNoYW5uZWxPdXRwdXRBcHBlbmRlZBIPCgdjaGFubmVsGAEgASgJEhQKDHBheWxvYWRfanNvbhgCIAEoCRIZCgxjb250ZW50X3R5cGUYAyABKAlIAIgBARIcCg9vYmplY3RfcmVmX2pzb24YBCABKAlIAYgBAUIPCg1fY29udGVudF90eXBlQhIKEF9vYmplY3RfcmVmX2pzb24irgEKD01ldGFkYXRhVXBkYXRlZBIRCglvcGVyYXRpb24YASABKAkSEAoDa2V5GAIgASgJSACIAQESFwoKdmFsdWVfanNvbhgDIAEoCUgBiAEBEhcKCnBhdGNoX2pzb24YBCABKAlIAogBARITCgZhbW91bnQYBSABKAFIA4gBAUIGCgRfa2V5Qg0KC192YWx1ZV9qc29uQg0KC19wYXRjaF9qc29uQgkKB19hbW91bnRCOlo4Z2l0aHViLmNvbS9oZWxtcmRvdGRldi9oZWxtci9pbnRlcm5hbC9wcm90by9ydW4vdjA7cnVudjBiBnByb3RvMw");
var RunTaskWorkspaceSchema = /* @__PURE__ */ messageDesc(file_run, 0);
var WorkspaceArtifactSchema = /* @__PURE__ */ messageDesc(file_run, 1);
var RunTaskRequestSchema = /* @__PURE__ */ messageDesc(file_run, 2);
var TraceContextSchema = /* @__PURE__ */ messageDesc(file_run, 3);
var SecretInjectSchema = /* @__PURE__ */ messageDesc(file_run, 4);
var PlacementSchema2 = /* @__PURE__ */ messageDesc(file_run, 5);
var EnvPlacementSchema2 = /* @__PURE__ */ messageDesc(file_run, 6);
var FilePlacementSchema2 = /* @__PURE__ */ messageDesc(file_run, 7);
var DirPlacementSchema2 = /* @__PURE__ */ messageDesc(file_run, 8);
var RunEventSchema = /* @__PURE__ */ messageDesc(file_run, 9);
var TaskResultSchema = /* @__PURE__ */ messageDesc(file_run, 10);
var TaskErrorSchema = /* @__PURE__ */ messageDesc(file_run, 11);
var WaitpointRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 12);
var WaitpointTokenCreateRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 13);
var WaitpointTokenCreateResultSchema = /* @__PURE__ */ messageDesc(file_run, 14);
var CheckpointPauseRequestSchema = /* @__PURE__ */ messageDesc(file_run, 15);
var CheckpointPauseReadySchema = /* @__PURE__ */ messageDesc(file_run, 16);
var ResumeAttachSchema = /* @__PURE__ */ messageDesc(file_run, 17);
var ResumeDecisionSchema = /* @__PURE__ */ messageDesc(file_run, 18);
var ResumeAckSchema = /* @__PURE__ */ messageDesc(file_run, 19);
var ResumeConsumedSchema = /* @__PURE__ */ messageDesc(file_run, 20);
var ChannelOutputAppendedSchema = /* @__PURE__ */ messageDesc(file_run, 21);
var MetadataUpdatedSchema = /* @__PURE__ */ messageDesc(file_run, 22);
// sdk/typescript/src/schema/payload.ts
var payloadSchemaValidationErrorBrand = Symbol.for("helmr.sdk.PayloadSchemaValidationError");

class PayloadSchemaValidationError extends Error {
  issues;
  constructor(label, issues) {
    super(formatPayloadSchemaValidationMessage(label, issues));
    this.name = "PayloadSchemaValidationError";
    this.issues = issues;
    Object.defineProperty(this, payloadSchemaValidationErrorBrand, { value: true });
  }
  static [Symbol.hasInstance](value) {
    return this === PayloadSchemaValidationError && typeof value === "object" && value !== null && payloadSchemaValidationErrorBrand in value;
  }
}
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
async function parsePayloadWithSchema(schema, payload, label) {
  assertStandardSchema(schema, label);
  const result = await schema["~standard"].validate(payload);
  if ("issues" in result && result.issues !== undefined) {
    throw new PayloadSchemaValidationError(label, result.issues);
  }
  return result.value;
}
function formatPayloadSchemaValidationMessage(label, issues) {
  if (issues.length === 0) {
    return `${label} failed validation`;
  }
  const formattedIssues = issues.slice(0, 5).map(formatPayloadSchemaIssue);
  const suffix = issues.length > formattedIssues.length ? `; and ${issues.length - formattedIssues.length} more` : "";
  return `${label} failed validation: ${formattedIssues.join("; ")}${suffix}`;
}
function formatPayloadSchemaIssue(issue) {
  const path = formatPayloadSchemaIssuePath(issue.path);
  return path === "" ? issue.message : `${path}: ${issue.message}`;
}
function formatPayloadSchemaIssuePath(path) {
  if (path === undefined || path.length === 0) {
    return "";
  }
  let formatted = "payload";
  for (const segment of path) {
    const key = typeof segment === "object" && segment !== null && "key" in segment ? segment.key : segment;
    if (typeof key === "string" && /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(key)) {
      formatted += `.${key}`;
    } else if (typeof key === "string" && isArrayIndexKey(key)) {
      formatted += `[${key}]`;
    } else if (typeof key === "number") {
      formatted += `[${key}]`;
    } else {
      formatted += `[${JSON.stringify(String(key))}]`;
    }
  }
  return formatted;
}
function isArrayIndexKey(value) {
  if (!/^(0|[1-9]\d*)$/.test(value)) {
    return false;
  }
  const parsed = Number(value);
  return Number.isSafeInteger(parsed);
}

// sdk/typescript/src/schema/task.ts
var TASK_ID_PATTERN = "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$";
var TASK_ID_MAX_LENGTH = 128;
var DEFAULT_MAX_DURATION_SECONDS = 900;
var MIN_MAX_DURATION_SECONDS = 5;
var MAX_DURATION_SECONDS = 86400;
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

class TaskMaxDurationError extends Error {
  name = "TaskMaxDurationError";
  value;
  label;
  constructor(value, label = "task maxDuration") {
    super(`${label} must be an integer number of seconds between ${MIN_MAX_DURATION_SECONDS} and ${MAX_DURATION_SECONDS}`);
    this.value = value;
    this.label = label;
  }
}
function readOptionalMaxDurationSeconds(value, label = "task maxDuration") {
  if (value === undefined) {
    return DEFAULT_MAX_DURATION_SECONDS;
  }
  if (typeof value === "number" && Number.isInteger(value) && Number.isFinite(value) && value >= MIN_MAX_DURATION_SECONDS && value <= MAX_DURATION_SECONDS) {
    return value;
  }
  throw new TaskMaxDurationError(value, label);
}
function validateOptionalMaxDurationSeconds(value, label = "task maxDuration") {
  readOptionalMaxDurationSeconds(value, label);
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
  if (typeof value === "number" && Number.isInteger(value) && Number.isFinite(value) && value > 0) {
    return;
  }
  throw new TaskQueueConcurrencyLimitError(value);
}
function isAsciiAlnum(code) {
  return code >= 48 && code <= 57 || code >= 65 && code <= 90 || code >= 97 && code <= 122;
}

// sdk/typescript/src/internal.ts
var CHANNEL_NAME_PATTERN = /^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$/;
function validateChannelName(value, label = "channel name") {
  const normalized = value.trim();
  if (!CHANNEL_NAME_PATTERN.test(normalized)) {
    throw new Error(`${label} must match ${CHANNEL_NAME_PATTERN}`);
  }
  return normalized;
}
var runtimeWaitOperand = Symbol.for("helmr.sdk.runtimeWaitOperand");
function getRuntimeWaitOperand(value) {
  if (value === null || typeof value !== "object" && typeof value !== "function") {
    return;
  }
  return value[runtimeWaitOperand];
}
var runRuntimeSlot = Symbol.for("helmr.sdk.runRuntime");
function enterRunRuntime(runtime) {
  const global = globalThis;
  if (global[runRuntimeSlot] !== undefined) {
    throw new Error("Helmr run runtime is already active");
  }
  global[runRuntimeSlot] = runtime;
  return () => {
    if (global[runRuntimeSlot] === runtime) {
      delete global[runRuntimeSlot];
    }
  };
}
function getRunRuntime() {
  const runtime = globalThis[runRuntimeSlot];
  if (runtime === undefined) {
    throw new Error("Helmr run APIs can only be used while a task is running");
  }
  return runtime;
}

class WaitpointResultImpl {
  ok;
  data;
  error;
  constructor(ok, data, error) {
    this.ok = ok;
    if (data !== undefined) {
      this.data = data;
    }
    if (error !== undefined) {
      this.error = error;
    }
  }
  unwrap() {
    if (this.ok) {
      return this.data;
    }
    if (this.error instanceof Error) {
      throw this.error;
    }
    throw new Error(String(this.error ?? "waitpoint failed"));
  }
}
var concurrentWaitErrorBrand = Symbol.for("helmr.sdk.ConcurrentWaitError");
var waitTimeoutErrorBrand = Symbol.for("helmr.sdk.WaitTimeoutError");

class ConcurrentWaitError extends Error {
  constructor(message) {
    super(message);
    this.name = "ConcurrentWaitError";
    Object.defineProperty(this, concurrentWaitErrorBrand, { value: true });
  }
  static [Symbol.hasInstance](value) {
    return this === ConcurrentWaitError && typeof value === "object" && value !== null && concurrentWaitErrorBrand in value;
  }
}

class WaitTimeoutError extends Error {
  timeout;
  constructor(message, timeout) {
    super(message);
    this.name = "WaitTimeoutError";
    this.timeout = timeout;
    Object.defineProperty(this, waitTimeoutErrorBrand, { value: true });
  }
  static [Symbol.hasInstance](value) {
    return this === WaitTimeoutError && typeof value === "object" && value !== null && waitTimeoutErrorBrand in value;
  }
}
function validateSecretName(name, label = "secret name") {
  if (name.length === 0) {
    throw new Error(`${label} must not be empty`);
  }
  if (name.length > 128) {
    throw new Error(`${label} must be at most 128 characters`);
  }
  if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/.test(name)) {
    throw new Error(`${label} must match /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/`);
  }
}
var taskBrand = Symbol.for("helmr.sdk.Task");
var taskOriginBrand = Symbol.for("helmr.sdk.TaskOrigin");
var configBrand = Symbol.for("helmr.sdk.Config");
var imageBuilderBrand = Symbol.for("helmr.sdk.ImageBuilder");
var sandboxBuilderBrand = Symbol.for("helmr.sdk.SandboxBuilder");
var sourceFileRefBrand = Symbol.for("helmr.sdk.SourceFileRef");
var sourceDirRefBrand = Symbol.for("helmr.sdk.SourceDirRef");
function markScheduledTask(config, schedule) {
  return markTaskInternal(config, schedule);
}
function markTaskInternal(config, schedule) {
  validateTaskId(config.id);
  validateOptionalMaxDurationSeconds(config.maxDuration);
  validateTaskQueue(config.id, config.queue);
  validateTaskSchedule(config.id, schedule);
  validateOptionalTTL(config.ttl, `task ${JSON.stringify(config.id)} ttl`);
  validateRetryPolicy(config.retry, `task ${JSON.stringify(config.id)} retry`);
  assertPayloadSchema(config.payload, `task ${JSON.stringify(config.id)} payload`);
  if (schedule !== undefined) {
    Object.defineProperty(config, "schedule", {
      value: Object.freeze({ ...schedule }),
      enumerable: true
    });
  }
  Object.defineProperty(config, taskBrand, { value: true });
  Object.defineProperty(config, taskOriginBrand, { value: captureTaskOrigin() });
  return config;
}
function validateRetryPolicy(value, label = "retry") {
  if (value === undefined || value === false) {
    return;
  }
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be false or a retry policy object`);
  }
  const record = value;
  for (const key of Object.keys(record)) {
    if (key !== "maxAttempts" && key !== "backoff") {
      throw new Error(`${label}.${key} is not supported`);
    }
  }
  if (typeof record["maxAttempts"] !== "number" || !Number.isInteger(record["maxAttempts"]) || record["maxAttempts"] < 1 || record["maxAttempts"] > 10) {
    throw new Error(`${label}.maxAttempts must be an integer between 1 and 10`);
  }
  const backoff = record["backoff"];
  if (backoff !== undefined) {
    if (backoff === null || typeof backoff !== "object" || Array.isArray(backoff)) {
      throw new Error(`${label}.backoff must be an object`);
    }
    const backoffRecord = backoff;
    for (const key of Object.keys(backoffRecord)) {
      if (key !== "minMs" && key !== "maxMs" && key !== "factor" && key !== "jitter") {
        throw new Error(`${label}.backoff.${key} is not supported`);
      }
    }
    validateOptionalPositiveInteger(backoffRecord["minMs"], `${label}.backoff.minMs`);
    validateOptionalPositiveInteger(backoffRecord["maxMs"], `${label}.backoff.maxMs`);
    const factor = backoffRecord["factor"];
    if (factor !== undefined && (typeof factor !== "number" || !Number.isFinite(factor) || factor <= 0)) {
      throw new Error(`${label}.backoff.factor must be a positive number`);
    }
    const jitter = backoffRecord["jitter"];
    if (jitter !== undefined && jitter !== "none" && jitter !== "full") {
      throw new Error(`${label}.backoff.jitter must be "none" or "full"`);
    }
  }
}
function validateOptionalPositiveInteger(value, label) {
  if (value === undefined) {
    return;
  }
  if (typeof value === "number" && Number.isInteger(value) && Number.isFinite(value) && value > 0) {
    return;
  }
  throw new Error(`${label} must be a positive integer`);
}
function validateTaskSchedule(taskId, value) {
  if (value === undefined) {
    return;
  }
  if (value.cron.trim() === "") {
    throw new Error(`task ${JSON.stringify(taskId)} schedule cron is required`);
  }
  if (value.timezone !== undefined && value.timezone.trim() === "") {
    throw new Error(`task ${JSON.stringify(taskId)} schedule timezone must not be empty`);
  }
}
function validateTaskQueue(taskId, value) {
  if (value === undefined) {
    return;
  }
  if (value.name !== undefined) {
    validateQueueName(value.name);
  }
  validateOptionalQueueConcurrencyLimit(value.concurrencyLimit);
  if (value.name === undefined && value.concurrencyLimit === undefined) {
    throw new Error(`task ${JSON.stringify(taskId)} queue must include name or concurrencyLimit`);
  }
}
function defaultTaskQueueName(taskId) {
  return `task/${taskId}`;
}
function validateOptionalTTL(value, label = "ttl") {
  if (value === undefined) {
    return;
  }
  if (typeof value === "string" && isPositiveDurationString(value)) {
    return;
  }
  throw new Error(`${label} must be a positive duration string`);
}
function isPositiveDurationString(value) {
  const raw = value.trim();
  if (raw === "") {
    return false;
  }
  if (/^[1-9][0-9]*d$/.test(raw)) {
    return true;
  }
  const sign = raw.startsWith("+") || raw.startsWith("-") ? raw.slice(0, 1) : "";
  if (sign === "-") {
    return false;
  }
  const body = sign === "" ? raw : raw.slice(1);
  const tokenPattern = /([0-9]+(?:\.[0-9]*)?|\.[0-9]+)(ns|us|µs|μs|ms|s|m|h)/gy;
  let totalNanoseconds = 0;
  let offset = 0;
  for (;; ) {
    tokenPattern.lastIndex = offset;
    const match = tokenPattern.exec(body);
    if (match === null) {
      return offset === body.length && totalNanoseconds >= 1;
    }
    if (match.index !== offset) {
      return false;
    }
    const amount = Number(match[1]);
    if (!Number.isFinite(amount) || amount < 0) {
      return false;
    }
    totalNanoseconds += amount * durationUnitNanoseconds(match[2] ?? "");
    offset = tokenPattern.lastIndex;
  }
}
function durationUnitNanoseconds(unit) {
  switch (unit) {
    case "ns":
      return 1;
    case "us":
    case "µs":
    case "μs":
      return 1000;
    case "ms":
      return 1e6;
    case "s":
      return 1e9;
    case "m":
      return 60000000000;
    case "h":
      return 3600000000000;
    default:
      return 0;
  }
}
async function parseTaskPayload(task, payload) {
  if (task.payload === undefined) {
    throw new Error(`task ${JSON.stringify(task.id)} does not accept payload`);
  }
  return await parsePayloadWithSchema(task.payload, payload, `task ${JSON.stringify(task.id)} payload`);
}
function isTaskDefinition(value) {
  return hasBrand(value, taskBrand);
}
function isConfigDefinition(value) {
  return hasBrand(value, configBrand);
}
function captureTaskOrigin() {
  const stack = new Error().stack ?? "";
  for (const line of stack.split(`
`).slice(1)) {
    const file = stackFrameFile(line);
    if (file === null || isSdkInternalFrame(file)) {
      continue;
    }
    return file;
  }
  return "unknown";
}
function stackFrameFile(line) {
  const match = /\(?((?:file:\/\/)?\/[^():]+):\d+:\d+\)?$/.exec(line.trim());
  if (!match?.[1]) {
    return null;
  }
  return match[1].startsWith("file://") ? decodeURIComponent(new URL(match[1]).pathname) : match[1];
}
function isSdkInternalFrame(file) {
  return file.includes("/sdk/typescript/src/internal") || file.includes("/sdk/typescript/src/task") || file.includes("/sdk/typescript/src/index") || file.includes("/runtime/typescript/src/");
}
function isImageBuilder(value) {
  return hasBrand(value, imageBuilderBrand);
}
function isSandboxBuilder(value) {
  return hasBrand(value, sandboxBuilderBrand);
}
function isSourceFileRef(value) {
  return hasBrand(value, sourceFileRefBrand);
}
function isSourceDirRef(value) {
  return hasBrand(value, sourceDirRefBrand);
}
function hasBrand(value, brand) {
  return value !== null && typeof value === "object" && value[brand] === true;
}

// sdk/typescript/src/idempotency.ts
import { createHash } from "node:crypto";
var idempotencyKeys = {
  create(key, options = {}) {
    const scope = options.scope ?? "global";
    return {
      value: createIdempotencyKey(key, scope),
      key,
      scope
    };
  }
};
function taskStartIdempotencyRequestFields(input, ttl) {
  if (input === undefined) {
    return {};
  }
  const key = isIdempotencyKey(input) ? input : idempotencyKeys.create(input);
  return {
    idempotency_key: key.value,
    ...ttl === undefined ? {} : { idempotency_key_ttl: ttl }
  };
}
function createIdempotencyKey(key, scope) {
  const material = {
    scope,
    key: Array.isArray(key) ? [...key] : [key]
  };
  return createHash("sha256").update(JSON.stringify(material)).digest("hex");
}
function isIdempotencyKey(value) {
  return typeof value === "object" && value !== null && "value" in value && "key" in value && "scope" in value;
}

// sdk/typescript/src/runtime/errors.ts
class AuthError extends Error {
  constructor(message) {
    super(message);
    this.name = "AuthError";
  }
}

class TimeoutError extends Error {
  constructor(message) {
    super(message);
    this.name = "TimeoutError";
  }
}

class UnsupportedTransportError extends Error {
  constructor(message) {
    super(message);
    this.name = "UnsupportedTransportError";
  }
}

// sdk/typescript/src/version.ts
var HELMR_API_VERSION = "2026-06-06";
var HELMR_API_VERSION_HEADER = "Helmr-API-Version";
var HELMR_SDK_VERSION_HEADER = "Helmr-SDK-Version";
var SOURCE_PACKAGE_VERSION = "0.1.0";
var HELMR_SDK_VERSION = typeof HELMR_SDK_PACKAGE_VERSION === "string" && HELMR_SDK_PACKAGE_VERSION.trim() !== "" ? HELMR_SDK_PACKAGE_VERSION : SOURCE_PACKAGE_VERSION;

// sdk/typescript/src/runtime/run.ts
function runHandle(id, taskId) {
  return { id, taskId };
}
function runSnapshot(snapshot) {
  const status = runStatus(snapshot.status);
  return {
    id: snapshot.id,
    taskId: snapshot.taskId,
    status,
    metadata: snapshot.metadata ?? {},
    exitCode: snapshot.exitCode ?? null,
    createdAt: snapshot.createdAt ?? null,
    updatedAt: snapshot.updatedAt ?? null,
    pendingWaitpoint: snapshot.pendingWaitpoint ?? null,
    ...snapshot.version === undefined && snapshot.deploymentVersion === undefined ? {} : { version: snapshot.version ?? snapshot.deploymentVersion ?? null },
    ...snapshot.deploymentVersion === undefined && snapshot.version === undefined ? {} : { deploymentVersion: snapshot.deploymentVersion ?? snapshot.version ?? null },
    ...snapshot.apiVersion === undefined ? {} : { apiVersion: snapshot.apiVersion },
    ...snapshot.sdkVersion === undefined ? {} : { sdkVersion: snapshot.sdkVersion },
    ...snapshot.cliVersion === undefined ? {} : { cliVersion: snapshot.cliVersion },
    attemptNumber: snapshot.attemptNumber ?? null,
    ...runStateBooleans(status),
    ...snapshot.output === undefined ? {} : { output: snapshot.output }
  };
}
function pendingWaitpointFromResponse(runId, wait) {
  if (wait === undefined || wait === null)
    return null;
  return {
    runId,
    id: wait.id,
    kind: wait.kind ?? "token",
    status: wait.status ?? "pending",
    timeout: wait.timeout ?? null,
    params: wait.params === undefined ? {} : wait.params,
    metadata: wait.metadata ?? {},
    tags: wait.tags ?? [],
    createdAt: wait.created_at
  };
}
function isTerminalRunStatus(status) {
  return status === "succeeded" || status === "failed" || status === "cancelled" || status === "expired";
}
function runId(value) {
  return typeof value === "string" ? value : value.id;
}
function runStateBooleans(status) {
  return {
    isQueued: status === "queued",
    isRunning: status === "running",
    isWaiting: status === "waiting",
    isTerminal: isTerminalRunStatus(status),
    isSuccess: status === "succeeded",
    isFailed: status === "failed",
    isCancelled: status === "cancelled"
  };
}
function runStatus(status) {
  switch (status) {
    case "queued":
    case "running":
    case "waiting":
    case "succeeded":
    case "failed":
    case "cancelled":
    case "expired":
      return status;
    default:
      throw new Error(`unsupported run status ${JSON.stringify(status)}`);
  }
}

// sdk/typescript/src/runtime/client.ts
var MAX_SSE_BUFFER_CHARS = 1024 * 1024;
var RUN_EVENT_RECONNECT_DELAY_MS = 1000;
var RUN_TERMINAL_SNAPSHOT_RETRY_DELAY_MS = 100;
var TASK_START_PENDING_MAX_WAIT_MS = 1e4;
var TASK_START_PENDING_DEFAULT_RETRY_MS = 250;
var startTaskClientMethod = Symbol.for("helmr.sdk.client.startTask");
var waitpointTokenClientMethod = Symbol.for("helmr.sdk.client.waitpointToken");

class WorkspaceStreamTerminalError extends Error {
  terminal;
  constructor(terminal) {
    super(`workspace stream terminal: ${terminal.state}`);
    this.name = "WorkspaceStreamTerminalError";
    this.terminal = terminal;
  }
}

class WorkspaceStreamError extends Error {
  code;
  cursor;
  constructor(code, message, cursor) {
    super(message ?? code);
    this.name = "WorkspaceStreamError";
    this.code = code;
    if (cursor !== undefined)
      this.cursor = cursor;
  }
}

class HelmrClient {
  #baseUrl;
  #apiKey;
  constructor(options = {}) {
    const rawUrl = options.url ?? process.env["HELMR_API_URL"];
    if (rawUrl === undefined || rawUrl.trim() === "") {
      throw new UnsupportedTransportError("HelmrClient requires a url option or HELMR_API_URL; no default transport is used");
    }
    const envApiKey = process.env["HELMR_API_KEY"];
    const apiKey = options.apiKey ?? envApiKey;
    let parsedUrl;
    try {
      parsedUrl = new URL(rawUrl);
    } catch {
      throw new UnsupportedTransportError("HelmrClient requires an http(s) URL");
    }
    if (parsedUrl.protocol === "https:") {
      this.#baseUrl = normalizedBaseUrl(parsedUrl);
      this.#apiKey = apiKey;
    } else if (parsedUrl.protocol === "http:") {
      if (!isLoopbackHost(parsedUrl.hostname)) {
        throw new UnsupportedTransportError(`refusing to send credentials over plaintext non-loopback URL ${parsedUrl.toString()}`);
      }
      console.warn("HelmrClient http:// transport is plaintext and must be explicitly opted into; use https:// for remote services");
      this.#baseUrl = normalizedBaseUrl(parsedUrl);
      this.#apiKey = apiKey;
    } else {
      throw new UnsupportedTransportError(`unsupported HelmrClient transport scheme ${parsedUrl.protocol.replace(/:$/, "")}`);
    }
  }
  tasks = {
    start: async (...args) => {
      const taskId = args[0];
      const hasPayload = args.length === 3;
      const payload = hasPayload ? args[1] : undefined;
      const opts = hasPayload ? args[2] : args[1];
      if (hasPayload && payload === undefined) {
        throw new Error(`task ${JSON.stringify(taskId)} requires payload`);
      }
      return await this.#startTask(taskId, payload, opts);
    },
    startAndWait: async (...args) => {
      const taskId = args[0];
      const hasPayload = args.length === 3;
      const payload = hasPayload ? args[1] : undefined;
      const opts = hasPayload ? args[2] : args[1];
      if (hasPayload && payload === undefined) {
        throw new Error(`task ${JSON.stringify(taskId)} requires payload`);
      }
      return await this.#startTaskAndWait(taskId, payload, opts);
    }
  };
  sessions = {
    open: (idOrHandle) => {
      return this.#openSession(sessionId(idOrHandle));
    },
    retrieve: async (idOrHandle, opts = {}) => {
      return await this.#openSession(sessionId(idOrHandle)).retrieve(opts);
    },
    wait: async (idOrHandle, opts = {}) => {
      return await this.#openSession(sessionId(idOrHandle)).wait(opts);
    },
    list: async (opts = {}) => {
      const response = await this.#json(`${taskSessionCollectionPath(opts)}${taskSessionListQuery(opts)}`, requestSignal(opts.signal));
      return response.sessions.map(taskSessionFromResponse);
    }
  };
  workspaces = {
    create: async (opts) => {
      const response = await this.#json(workspaceCollectionPath(opts), {
        method: "POST",
        body: JSON.stringify(workspaceCreateBody(opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal)
      });
      return workspaceFromResponse(response.workspace);
    },
    open: (id) => {
      return this.#openWorkspace(id);
    },
    retrieve: async (idOrHandle, opts = {}) => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).retrieve(opts);
    },
    list: async (opts = {}) => {
      const response = await this.#json(`${workspaceCollectionPath(opts)}${workspaceListQuery(opts)}`, requestSignal(opts.signal));
      return response.workspaces.map(workspaceFromResponse);
    },
    update: async (idOrHandle, opts) => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).update(opts);
    },
    delete: async (idOrHandle, opts = {}) => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).delete(opts);
    },
    materialize: async (idOrHandle, opts = {}) => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).materialize(opts);
    },
    connect: async (idOrHandle, opts = {}) => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).connect(opts);
    },
    stop: async (idOrHandle, opts = {}) => {
      return await this.#openWorkspace(workspaceId(idOrHandle)).stop(opts);
    }
  };
  waitpoints = {
    tokens: {
      create: async (opts = {}) => {
        return await this[waitpointTokenClientMethod]({ operation: "create", opts });
      },
      retrieve: async (id, opts = {}) => {
        return await this[waitpointTokenClientMethod]({ operation: "retrieve", id, opts });
      },
      listPage: async (opts = {}) => {
        return await this[waitpointTokenClientMethod]({ operation: "listPage", opts });
      },
      list: async (opts = {}) => {
        return [...(await this[waitpointTokenClientMethod]({ operation: "listPage", opts })).tokens];
      },
      complete: async (token, data, opts = {}) => {
        await this[waitpointTokenClientMethod]({ operation: "complete", token, data, opts });
      }
    }
  };
  auth = {
    createPublicToken: async (opts) => {
      const response = await this.#json("/api/public-access-tokens", {
        method: "POST",
        body: JSON.stringify(publicAccessTokenCreateBody(opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal)
      });
      return publicAccessTokenFromResponse(response);
    }
  };
  async[startTaskClientMethod](task, ...args) {
    const hasPayload = task.payload !== undefined;
    const payload = hasPayload ? args[0] : undefined;
    const opts = hasPayload ? args[1] : args[0];
    if (task.payload !== undefined) {
      if (payload === undefined) {
        throw new Error(`task ${JSON.stringify(task.id)} requires payload`);
      }
      await parseTaskPayload(task, payload);
    } else if (args.length > 1) {
      throw new Error(`task ${JSON.stringify(task.id)} does not accept payload`);
    }
    return await this.#startTask(task.id, payload, opts, readOptionalMaxDurationSeconds(task.maxDuration));
  }
  async[waitpointTokenClientMethod](request) {
    switch (request.operation) {
      case "create": {
        const opts = request.opts ?? {};
        const response = await this.#json(waitpointTokenCollectionPath(opts), {
          method: "POST",
          body: JSON.stringify(waitpointTokenCreateBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return waitpointTokenFromResponse(response);
      }
      case "retrieve": {
        const opts = request.opts ?? {};
        const response = await this.#json(`${waitpointTokenCollectionPath(opts)}/${encodeURIComponent(request.id)}`, requestSignal(opts.signal));
        return waitpointTokenFromResponse(response);
      }
      case "listPage": {
        const opts = request.opts ?? {};
        const response = await this.#json(`${waitpointTokenCollectionPath(opts)}${waitpointTokenListQuery(opts)}`, requestSignal(opts.signal));
        return {
          tokens: response.tokens.map(waitpointTokenFromResponse),
          nextCursor: response.next_cursor ?? null
        };
      }
      case "complete": {
        const opts = request.opts ?? {};
        const id = typeof request.token === "string" ? request.token : request.token.id;
        const publicAccessToken = opts.publicAccessToken ?? waitpointTokenPublicAccessToken(request.token);
        await this.#fetch(`/api/waitpoints/tokens/${encodeURIComponent(id)}/complete`, {
          method: "POST",
          body: JSON.stringify(waitpointTokenCompleteBody(request.data, opts)),
          headers: {
            "content-type": "application/json",
            ...publicAccessToken === undefined ? {} : { authorization: `Bearer ${publicAccessToken}` }
          },
          ...requestSignal(opts.signal)
        });
        return;
      }
    }
  }
  async#startTask(taskId, payload, opts, maxDurationSeconds) {
    validateRetryPolicy(opts.retry, "retry");
    const body = taskStartBody(payload, opts, maxDurationSeconds);
    const path = taskStartPath(taskId, opts, "start");
    const startedAt = Date.now();
    for (;; ) {
      const response = await this.#fetch(path, {
        method: "POST",
        body: JSON.stringify(body),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal)
      });
      if (response.status !== 202) {
        const start = await response.json();
        return taskStartFromResponse(start);
      }
      const pendingBody = await response.text();
      if (!taskStartPendingResponse(pendingBody)) {
        throw new HelmrApiError(response.status, pendingBody);
      }
      const retryDelay = taskStartPendingRetryDelay(response);
      if (Date.now() - startedAt + retryDelay > TASK_START_PENDING_MAX_WAIT_MS) {
        throw new HelmrApiError(response.status, pendingBody);
      }
      await delay(retryDelay, opts.signal);
    }
  }
  async#startTaskAndWait(taskId, payload, opts, maxDurationSeconds) {
    validateRetryPolicy(opts.retry, "retry");
    const body = {
      ...taskStartBody(payload, opts, maxDurationSeconds),
      ...opts.timeoutSeconds === undefined ? {} : { timeout_seconds: opts.timeoutSeconds }
    };
    const path = taskStartPath(taskId, opts, "start-and-wait");
    const startedAt = Date.now();
    for (;; ) {
      const response = await this.#fetch(path, {
        method: "POST",
        body: JSON.stringify(body),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal)
      });
      if (response.status !== 202) {
        return taskSessionFromResponse(await response.json());
      }
      const pendingBody = await response.text();
      if (!taskStartPendingResponse(pendingBody)) {
        throw new HelmrApiError(response.status, pendingBody);
      }
      const retryDelay = taskStartPendingRetryDelay(response);
      if (Date.now() - startedAt + retryDelay > TASK_START_PENDING_MAX_WAIT_MS) {
        throw new HelmrApiError(response.status, pendingBody);
      }
      await delay(retryDelay, opts.signal);
    }
  }
  #openWorkspace(id) {
    return {
      id,
      exec: async (command, opts = {}) => {
        return await this.#createWorkspaceExec(id, command, opts);
      },
      execs: {
        retrieve: (execId) => {
          return this.#openWorkspaceExec(id, execId);
        },
        list: async (opts = {}) => {
          const response = await this.#json(`${workspaceResourcePath(id, opts)}/execs${workspacePrimitiveListQuery(opts)}`, requestSignal(opts.signal));
          return response.execs.map(workspaceExecFromResponse);
        }
      },
      pty: {
        create: async (opts = {}) => {
          return await this.#createWorkspacePty(id, opts);
        },
        retrieve: (ptyId) => {
          return this.#openWorkspacePty(id, ptyId);
        },
        list: async (opts = {}) => {
          const response = await this.#json(`${workspaceResourcePath(id, opts)}/pty${workspacePrimitiveListQuery(opts)}`, requestSignal(opts.signal));
          return response.ptys.map(workspacePtyFromResponse);
        }
      },
      retrieve: async (opts = {}) => {
        const response = await this.#json(workspaceResourcePath(id, opts), requestSignal(opts.signal));
        return workspaceFromResponse(response.workspace);
      },
      update: async (opts) => {
        const response = await this.#json(workspaceResourcePath(id, opts), {
          method: "PATCH",
          body: JSON.stringify(workspaceUpdateBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return workspaceFromResponse(response.workspace);
      },
      delete: async (opts = {}) => {
        const response = await this.#json(workspaceResourcePath(id, opts), {
          method: "DELETE",
          ...requestSignal(opts.signal)
        });
        return workspaceFromResponse(response.workspace);
      },
      materialize: async (opts = {}) => {
        const response = await this.#json(`${workspaceResourcePath(id, opts)}/materialize`, {
          method: "POST",
          body: JSON.stringify(workspaceMaterializeBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return workspaceMaterializationFromResponse(response);
      },
      connect: async (opts = {}) => {
        const response = await this.#json(`${workspaceResourcePath(id, opts)}/connect`, {
          method: "POST",
          body: JSON.stringify(workspaceMaterializeBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return workspaceMaterializationFromResponse(response);
      },
      stop: async (opts = {}) => {
        const response = await this.#json(`${workspaceResourcePath(id, opts)}/stop`, {
          method: "POST",
          body: JSON.stringify(workspaceStopBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return workspaceStopFromResponse(response);
      }
    };
  }
  async#createWorkspaceExec(id, command, opts) {
    const response = await this.#json(`${workspaceResourcePath(id, opts)}/execs`, {
      method: "POST",
      body: JSON.stringify(workspaceExecCreateBody(command, opts)),
      headers: { "content-type": "application/json" },
      ...requestSignal(opts.signal)
    });
    return this.#openWorkspaceExec(id, response.exec.id);
  }
  #openWorkspaceExec(workspaceId, execId) {
    const client = this;
    return {
      id: execId,
      workspaceId,
      retrieve: async (opts = {}) => {
        const response = await client.#json(`${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}`, requestSignal(opts.signal));
        return workspaceExecFromResponse(response.exec);
      },
      stdout: client.#workspaceExecReadableStreamApi(workspaceId, execId, "stdout"),
      stderr: client.#workspaceExecReadableStreamApi(workspaceId, execId, "stderr"),
      stdin: {
        write: async (data, opts) => {
          const response = await client.#json(`${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}/stdin`, {
            method: "POST",
            body: JSON.stringify(workspaceStreamWriteBody(data, opts)),
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal)
          });
          return workspaceStreamChunkFromResponse(response);
        },
        close: async (opts = {}) => {
          const response = await client.#json(`${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}/stdin/close`, {
            method: "POST",
            body: "{}",
            headers: { "content-type": "application/json" },
            ...requestSignal(opts.signal)
          });
          return workspaceExecFromResponse(response.exec);
        }
      },
      wait: async (opts = {}) => {
        return await client.#waitWorkspaceExec(workspaceId, execId, opts);
      }
    };
  }
  async#waitWorkspaceExec(workspaceId, execId, opts) {
    const pollIntervalMs = opts.pollIntervalMs ?? 250;
    for (;; ) {
      const response = await this.#json(`${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}`, requestSignal(opts.signal));
      const exec = workspaceExecFromResponse(response.exec);
      if (workspaceExecTerminal(exec.state))
        return exec;
      await delay(pollIntervalMs, opts.signal);
    }
  }
  async#createWorkspacePty(id, opts) {
    const response = await this.#json(`${workspaceResourcePath(id, opts)}/pty`, {
      method: "POST",
      body: JSON.stringify(workspacePtyCreateBody(opts)),
      headers: { "content-type": "application/json" },
      ...requestSignal(opts.signal)
    });
    return this.#openWorkspacePty(id, response.pty.id);
  }
  #openWorkspacePty(workspaceId, ptyId) {
    const client = this;
    return {
      id: ptyId,
      workspaceId,
      retrieve: async (opts = {}) => {
        const response = await client.#json(`${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}`, requestSignal(opts.signal));
        return workspacePtyFromResponse(response.pty);
      },
      output: client.#workspacePtyOutputApi(workspaceId, ptyId),
      input: async (data, opts) => {
        const response = await client.#json(`${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/input`, {
          method: "POST",
          body: JSON.stringify(workspaceStreamWriteBody(data, opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return workspaceStreamChunkFromResponse(response);
      },
      resize: async (cols, rows, opts = {}) => {
        const response = await client.#json(`${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/resize`, {
          method: "POST",
          body: JSON.stringify({ cols, rows }),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return workspacePtyFromResponse(response.pty);
      },
      close: async (opts = {}) => {
        return await client.#requestWorkspacePtyClose(workspaceId, ptyId, opts);
      }
    };
  }
  async#requestWorkspacePtyClose(workspaceId, ptyId, opts) {
    const response = await this.#json(`${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/close`, {
      method: "POST",
      body: "{}",
      headers: { "content-type": "application/json" },
      ...requestSignal(opts.signal)
    });
    return workspacePtyFromResponse(response.pty);
  }
  #workspaceExecReadableStreamApi(workspaceId, execId, stream) {
    return {
      list: async (opts = {}) => {
        const response = await this.#json(`${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}/${stream}${workspaceStreamListQuery(opts)}`, requestSignal(opts.signal));
        return response.chunks.map(workspaceStreamChunkFromResponse);
      },
      stream: (opts = {}) => this.#streamWorkspaceReadable(`${workspaceResourcePath(workspaceId, opts)}/execs/${encodeURIComponent(execId)}/${stream}`, opts)
    };
  }
  #workspacePtyOutputApi(workspaceId, ptyId) {
    return {
      list: async (opts = {}) => {
        const response = await this.#json(`${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/output${workspaceStreamListQuery(opts)}`, requestSignal(opts.signal));
        return response.chunks.map(workspaceStreamChunkFromResponse);
      },
      stream: (opts = {}) => this.#streamWorkspaceReadable(`${workspaceResourcePath(workspaceId, opts)}/pty/${encodeURIComponent(ptyId)}/output`, opts)
    };
  }
  async* #streamWorkspaceReadable(path, opts) {
    let cursor = opts.fromCursor;
    for (;; ) {
      try {
        let terminal = false;
        for await (const event of this.#streamWorkspaceReadableOnce(path, { cursor, limit: opts.limit, signal: opts.signal })) {
          if (event.kind === "chunk") {
            cursor = event.chunk.offsetEnd;
            yield event.chunk;
            continue;
          }
          if (event.kind === "terminal") {
            terminal = true;
            if (event.terminal.state === "lost" || event.terminal.state === "failed") {
              throw new WorkspaceStreamTerminalError(event.terminal);
            }
            break;
          }
        }
        if (terminal) {
          return;
        }
      } catch (error) {
        throwIfAborted(opts.signal);
        if (workspaceStreamErrorIsFatal(error)) {
          throw error;
        }
      }
      await delay(RUN_EVENT_RECONNECT_DELAY_MS, opts.signal);
    }
  }
  async* #streamWorkspaceReadableOnce(path, opts) {
    const query = new URLSearchParams;
    query.set("follow", "1");
    if (opts.cursor !== undefined)
      query.set("cursor", String(opts.cursor));
    if (opts.limit !== undefined)
      query.set("limit", String(opts.limit));
    const response = await this.#fetch(`${path}?${query}`, {
      headers: { accept: "text/event-stream" },
      ...requestSignal(opts.signal)
    });
    yield* parseWorkspaceStreamSse(response);
  }
  #openSession(id) {
    return {
      id,
      retrieve: async (opts = {}) => {
        const response = await this.#json(taskSessionResourcePath(id, opts, ""), requestSignal(opts.signal));
        return taskSessionFromResponse(response);
      },
      wait: async (opts = {}) => {
        const response = await this.#json(taskSessionResourcePath(id, opts, "/wait"), {
          method: "POST",
          body: JSON.stringify(taskSessionWaitBody(opts)),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return taskSessionFromResponse(response);
      },
      close: async (opts = {}) => {
        const response = await this.#json(taskSessionResourcePath(id, opts, "/close"), {
          method: "POST",
          body: JSON.stringify(opts.reason === undefined ? {} : { reason: opts.reason }),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return taskSessionFromResponse(response);
      },
      cancel: async (opts = {}) => {
        const response = await this.#json(taskSessionResourcePath(id, opts, "/cancel"), {
          method: "POST",
          body: JSON.stringify(opts.reason === undefined ? {} : { reason: opts.reason }),
          headers: { "content-type": "application/json" },
          ...requestSignal(opts.signal)
        });
        return taskSessionFromResponse(response);
      },
      runs: async (opts = {}) => {
        const response = await this.#json(taskSessionResourcePath(id, opts, "/runs"), requestSignal(opts.signal));
        return response.runs.map(taskSessionRunFromResponse);
      },
      input: (channelInput) => {
        const channel = validateChannelName(channelInput);
        return {
          send: async (data, opts = {}) => {
            const response = await this.#json(taskSessionResourcePath(id, opts, `/channels/${encodeURIComponent(channel)}/inputs`), {
              method: "POST",
              body: JSON.stringify(channelInputAppendBody(data, opts)),
              headers: { "content-type": "application/json" },
              ...requestSignal(opts.signal)
            });
            return appendChannelRecordFromResponse(response);
          },
          list: async (opts = {}) => {
            return await this.#listSessionChannelRecords(id, channel, "inputs", opts);
          }
        };
      },
      output: (channelInput) => {
        const channel = validateChannelName(channelInput);
        return {
          list: async (opts = {}) => {
            return await this.#listSessionChannelRecords(id, channel, "outputs", opts);
          },
          stream: async (opts = {}) => {
            return this.#streamSessionChannelOutputs(id, channel, opts);
          }
        };
      }
    };
  }
  async#listSessionChannelRecords(sessionID, channel, direction, opts) {
    const response = await this.#json(`${taskSessionResourcePath(sessionID, opts, `/channels/${encodeURIComponent(channel)}/${direction}`)}${sessionChannelQuery(opts)}`, requestSignal(opts.signal));
    return response.records.map(channelRecordFromResponse);
  }
  async#streamSessionChannelOutputs(sessionID, channel, opts) {
    const client = this;
    const stream = async function* () {
      let cursor = opts.cursor;
      for (;; ) {
        try {
          const response = await client.#fetch(`${taskSessionResourcePath(sessionID, opts, `/channels/${encodeURIComponent(channel)}/outputs/stream`)}${sessionChannelStreamQuery(opts, cursor)}`, {
            headers: { accept: "text/event-stream" },
            ...requestSignal(opts.signal)
          });
          for await (const record of parseChannelRecordSse(response)) {
            cursor = Math.max(cursor ?? 0, record.sequence);
            yield record;
          }
        } catch (error) {
          throwIfAborted(opts.signal);
          if (runEventStreamErrorIsFatal(error)) {
            throw error;
          }
        }
        try {
          const session = await client.sessions.retrieve(sessionID, taskSessionRetrieveOptions(opts, opts.signal));
          if (taskSessionTerminal(session.status)) {
            return;
          }
        } catch (error) {
          throwIfAborted(opts.signal);
          if (runSnapshotErrorIsFatal(error)) {
            throw error;
          }
        }
        await delay(RUN_EVENT_RECONNECT_DELAY_MS, opts.signal);
      }
    };
    return stream();
  }
  runs = {
    retrieve: async (idOrHandle, opts = {}) => {
      const response = await this.#json(runResourcePath(runId(idOrHandle), opts, ""), requestSignal(opts.signal));
      return runResponseToSnapshot(response);
    },
    wait: async (idOrHandle, opts = {}) => {
      const id = runId(idOrHandle);
      const timeoutMs = opts.timeoutMs;
      const wait = waitSignal(opts.signal, timeoutMs, () => new TimeoutError(`run ${id} did not finish within ${timeoutMs}ms`));
      const scopedRetrieveOptions = runRetrieveOptions(opts, wait.signal);
      try {
        let run = await this.#retrieveRunSnapshotWithRetry(id, scopedRetrieveOptions, RUN_EVENT_RECONNECT_DELAY_MS);
        if (isTerminalRunStatus(run.status)) {
          return run;
        }
        let cursor;
        for (;; ) {
          throwIfAborted(wait.signal);
          let terminalEventSeen = false;
          try {
            for await (const event of this.#streamEventRecordsOnce(id, runEventOptions(scopedRetrieveOptions, cursor))) {
              cursor = nextRunEventCursor(cursor, event);
              if (runEventRecordIsTerminal(event)) {
                terminalEventSeen = true;
                break;
              }
            }
          } catch (error) {
            throwIfAborted(wait.signal);
            if (error instanceof SseProtocolError && error.cursor !== undefined) {
              cursor = advanceRunEventCursor(cursor, error.cursor);
            } else if (runEventWaitStreamErrorIsFatal(error)) {
              throw error;
            }
          }
          if (terminalEventSeen) {
            return await this.#waitForTerminalSnapshot(id, scopedRetrieveOptions);
          }
          try {
            run = await this.runs.retrieve(id, scopedRetrieveOptions);
          } catch (error) {
            throwIfAborted(wait.signal);
            if (runSnapshotErrorIsFatal(error)) {
              throw error;
            }
            await delay(RUN_EVENT_RECONNECT_DELAY_MS, wait.signal);
            continue;
          }
          if (isTerminalRunStatus(run.status)) {
            return run;
          }
          await delay(RUN_EVENT_RECONNECT_DELAY_MS, wait.signal);
        }
      } finally {
        wait.cleanup();
      }
    },
    cancel: async (idOrHandle, opts = {}) => {
      const response = await this.#json(runResourcePath(runId(idOrHandle), opts, "/cancel"), {
        method: "POST",
        body: JSON.stringify(cancelRunBody(opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal)
      });
      return runResponseToSnapshot(response.run);
    },
    list: async (opts = {}) => {
      const query = new URLSearchParams;
      if (opts.status !== undefined)
        query.set("status", opts.status);
      if (opts.limit !== undefined)
        query.set("limit", String(opts.limit));
      const suffix = query.size === 0 ? "" : `?${query}`;
      const response = await this.#json(`${runCollectionPath(opts)}${suffix}`, requestSignal(opts.signal));
      return response.runs.map((run) => runResponseToSnapshot(run));
    },
    waitpoints: {
      listPage: async (idOrHandle, opts = {}) => {
        const id = runId(idOrHandle);
        const response = await this.#json(`${runResourcePath(id, opts, "/waitpoints")}${runWaitpointListQuery(opts)}`, requestSignal(opts.signal));
        return {
          waitpoints: response.waitpoints.map((request) => pendingWaitpointFromResponse(id, request)).filter((request) => request !== null),
          nextCursor: response.next_cursor ?? null
        };
      },
      list: async (idOrHandle, opts = {}) => {
        return [...(await this.runs.waitpoints.listPage(idOrHandle, opts)).waitpoints];
      }
    },
    logs: {
      retrieve: async (idOrHandle, opts = {}) => {
        return await this.#retrieveLogs(runId(idOrHandle), opts);
      }
    },
    events: {
      list: async (idOrHandle, opts = {}) => {
        return await this.#listEvents(runId(idOrHandle), opts);
      },
      subscribe: async (idOrHandle, opts = {}) => {
        return await this.#subscribeEvents(runId(idOrHandle), opts);
      }
    }
  };
  schedules = {
    create: async (opts) => {
      const response = await this.#json("/api/schedules", {
        method: "POST",
        body: JSON.stringify(scheduleCreateBody(opts)),
        headers: { "content-type": "application/json" }
      });
      return scheduleFromResponse(response);
    },
    list: async (opts = {}) => {
      const response = await this.#json("/api/schedules", requestSignal(opts.signal));
      return response.schedules.map(scheduleFromResponse);
    },
    update: async (id, opts) => {
      const response = await this.#json(`/api/schedules/${encodeURIComponent(id)}`, {
        method: "PUT",
        body: JSON.stringify(scheduleCreateBody(opts)),
        headers: { "content-type": "application/json" },
        ...requestSignal(opts.signal)
      });
      return scheduleFromResponse(response);
    },
    retrieve: async (id, opts = {}) => {
      return scheduleFromResponse(await this.#json(`/api/schedules/${encodeURIComponent(id)}`, requestSignal(opts.signal)));
    },
    activate: async (id, opts = {}) => {
      return scheduleFromResponse(await this.#json(`/api/schedules/${encodeURIComponent(id)}/activate`, {
        method: "POST",
        ...requestSignal(opts.signal)
      }));
    },
    deactivate: async (id, opts = {}) => {
      return scheduleFromResponse(await this.#json(`/api/schedules/${encodeURIComponent(id)}/deactivate`, {
        method: "POST",
        ...requestSignal(opts.signal)
      }));
    },
    delete: async (id, opts = {}) => {
      await this.#fetch(`/api/schedules/${encodeURIComponent(id)}`, {
        method: "DELETE",
        ...requestSignal(opts.signal)
      });
    }
  };
  async#retrieveLogs(id, opts = {}) {
    const response = await this.#json(runResourcePath(id, opts, "/logs"), requestSignal(opts.signal));
    return {
      stdout: decodeBase64Text(response.stdout_base64),
      stderr: decodeBase64Text(response.stderr_base64),
      cursor: response.cursor,
      truncated: response.truncated
    };
  }
  async#listEvents(id, opts) {
    const events = [];
    let cursor = opts.cursor;
    for (;; ) {
      const query = new URLSearchParams;
      if (cursor !== undefined)
        query.set("cursor", String(cursor));
      if (opts.pageSize !== undefined)
        query.set("limit", String(opts.pageSize));
      const suffix = query.size === 0 ? "" : `?${query}`;
      const page = await this.#json(`${runResourcePath(id, opts, "/events")}${suffix}`, requestSignal(opts.signal));
      events.push(...page.events);
      if (page.next_cursor === undefined || page.next_cursor === null) {
        break;
      }
      cursor = page.next_cursor;
    }
    return events.map((event) => runEventRecordToRunEvent(event)).filter((event) => event !== undefined);
  }
  async#subscribeEvents(id, opts) {
    const stream = async function* (client) {
      let cursor = opts.cursor;
      for (;; ) {
        let checkSnapshot = false;
        try {
          for await (const record of client.#streamEventRecordsOnce(id, runEventOptions(opts, cursor))) {
            cursor = nextRunEventCursor(cursor, record);
            const terminal = runEventRecordIsTerminal(record);
            const event = runEventRecordToRunEvent(record);
            if (event !== undefined) {
              yield event;
            }
            if (terminal) {
              return;
            }
          }
          checkSnapshot = true;
        } catch (error) {
          throwIfAborted(opts.signal);
          if (error instanceof SseProtocolError && error.cursor !== undefined) {
            cursor = advanceRunEventCursor(cursor, error.cursor);
          } else if (runEventStreamErrorIsFatal(error)) {
            throw error;
          }
          checkSnapshot = true;
        }
        if (checkSnapshot) {
          try {
            const run = await client.runs.retrieve(id, runRetrieveOptions(opts, opts.signal));
            if (isTerminalRunStatus(run.status)) {
              for await (const record of client.#listEventRecordsAfter(id, cursor, opts)) {
                cursor = nextRunEventCursor(cursor, record);
                const terminal = runEventRecordIsTerminal(record);
                const event = runEventRecordToRunEvent(record);
                if (event !== undefined) {
                  yield event;
                }
                if (terminal) {
                  break;
                }
              }
              return;
            }
          } catch (error) {
            throwIfAborted(opts.signal);
            if (runSnapshotErrorIsFatal(error)) {
              throw error;
            }
          }
        }
        await delay(RUN_EVENT_RECONNECT_DELAY_MS, opts.signal);
      }
    };
    return stream(this);
  }
  async* #streamEventRecordsOnce(id, opts) {
    const query = new URLSearchParams;
    query.set("follow", "1");
    if (opts.cursor !== undefined)
      query.set("cursor", String(opts.cursor));
    const response = await this.#fetch(`${runResourcePath(id, opts, "/events")}?${query}`, {
      headers: { accept: "text/event-stream" },
      ...requestSignal(opts.signal)
    });
    yield* parseSse(response);
  }
  async* #listEventRecordsAfter(id, cursor, opts) {
    let nextCursor = cursor;
    for (;; ) {
      const query = new URLSearchParams;
      if (nextCursor !== undefined)
        query.set("cursor", String(nextCursor));
      const suffix = query.size === 0 ? "" : `?${query}`;
      const page = await this.#json(`${runResourcePath(id, opts, "/events")}${suffix}`, requestSignal(opts.signal));
      for (const event of page.events) {
        yield event;
      }
      if (page.next_cursor === undefined || page.next_cursor === null) {
        return;
      }
      if (nextCursor !== undefined && page.next_cursor <= nextCursor) {
        return;
      }
      nextCursor = page.next_cursor;
    }
  }
  async#waitForTerminalSnapshot(id, opts) {
    let retryDelayMs = RUN_TERMINAL_SNAPSHOT_RETRY_DELAY_MS;
    for (;; ) {
      const run = await this.#retrieveRunSnapshotWithRetry(id, opts, retryDelayMs);
      if (isTerminalRunStatus(run.status)) {
        return run;
      }
      await delay(retryDelayMs, opts.signal);
      retryDelayMs = Math.min(retryDelayMs * 2, RUN_EVENT_RECONNECT_DELAY_MS);
    }
  }
  async#retrieveRunSnapshotWithRetry(id, opts, retryDelayMs) {
    for (;; ) {
      try {
        return await this.runs.retrieve(id, opts);
      } catch (error) {
        throwIfAborted(opts.signal);
        if (runSnapshotErrorIsFatal(error)) {
          throw error;
        }
        await delay(retryDelayMs, opts.signal);
      }
    }
  }
  async#json(path, init = {}) {
    return (await this.#fetch(path, init)).json();
  }
  async#fetch(path, init = {}) {
    const headers = new Headers(init.headers);
    headers.set(HELMR_API_VERSION_HEADER, HELMR_API_VERSION);
    headers.set(HELMR_SDK_VERSION_HEADER, HELMR_SDK_VERSION);
    if (this.#apiKey !== undefined && !headers.has("authorization")) {
      headers.set("authorization", `Bearer ${this.#apiKey}`);
    }
    const request = {
      ...init,
      headers
    };
    const response = await fetch(endpointUrl(this.#baseUrl, path), request);
    if (response.status === 401) {
      throw new AuthError("Helmr authentication failed");
    }
    if (!response.ok) {
      throw new HelmrApiError(response.status, await response.text());
    }
    return response;
  }
}
function normalizedBaseUrl(url) {
  if (url.search !== "" || url.hash !== "") {
    throw new UnsupportedTransportError("HelmrClient URL must not include query or fragment");
  }
  return url;
}
function isLoopbackHost(hostname) {
  const host = hostname.trim().toLowerCase().replace(/^\[/, "").replace(/\]$/, "");
  if (host === "localhost" || host === "::1") {
    return true;
  }
  const ipv4 = /^(\d+)\.(\d+)\.(\d+)\.(\d+)$/.exec(host);
  if (ipv4 === null) {
    return false;
  }
  return ipv4[1] === "127" && ipv4.slice(2).every((part) => Number(part) >= 0 && Number(part) <= 255);
}
function endpointUrl(baseUrl, path) {
  const endpoint = new URL(baseUrl.toString());
  const queryStart = path.indexOf("?");
  const pathOnly = queryStart === -1 ? path : path.slice(0, queryStart);
  const query = queryStart === -1 ? "" : path.slice(queryStart + 1);
  endpoint.pathname = joinUrlPath(endpoint.pathname, pathOnly);
  endpoint.search = query;
  endpoint.hash = "";
  return endpoint;
}
function joinUrlPath(basePath, path) {
  const base = basePath.replace(/\/+$/, "");
  const suffix = `/${path.replace(/^\/+/, "")}`;
  return base === "" ? suffix : `${base}${suffix}`;
}
function runResponseToSnapshot(response) {
  return runSnapshot({
    id: response.id,
    taskId: response.task_id,
    ...response.version === undefined && response.deployment_version === undefined ? {} : { version: response.version ?? response.deployment_version ?? null },
    ...response.deployment_version === undefined && response.version === undefined ? {} : { deploymentVersion: response.deployment_version ?? response.version ?? null },
    ...response.api_version === undefined ? {} : { apiVersion: response.api_version },
    ...response.sdk_version === undefined ? {} : { sdkVersion: response.sdk_version },
    ...response.cli_version === undefined ? {} : { cliVersion: response.cli_version },
    attemptNumber: response.attempt_number ?? null,
    status: response.status,
    metadata: response.metadata ?? {},
    exitCode: response.exit_code ?? null,
    ...response.created_at === undefined ? {} : { createdAt: response.created_at },
    ...response.updated_at === undefined ? {} : { updatedAt: response.updated_at },
    pendingWaitpoint: pendingWaitpointFromResponse(response.id, response.pending_waitpoint),
    ..."output" in response ? { output: response.output } : {}
  });
}
function taskStartFromResponse(response) {
  return {
    session: taskSessionFromResponse(response.session),
    run: runHandle(response.run.id, response.run.task_id),
    isCached: response.is_cached ?? false
  };
}
function taskSessionFromResponse(response) {
  return {
    id: response.id,
    projectId: response.project_id,
    environmentId: response.environment_id,
    taskId: response.task_id,
    initialDeploymentId: response.initial_deployment_id,
    activeDeploymentId: response.active_deployment_id,
    ...response.external_id === undefined || response.external_id === "" ? {} : { externalId: response.external_id },
    status: response.status,
    currentRunId: response.current_run_id ?? null,
    workspaceId: response.workspace_id ?? null,
    metadata: response.metadata ?? {},
    tags: response.tags ?? [],
    ..."result" in response ? { result: response.result } : {},
    ..."error" in response ? { error: response.error } : {},
    timedOut: response.timed_out ?? false,
    ..."terminal_reason" in response ? { terminalReason: response.terminal_reason } : {},
    expiresAt: response.expires_at ?? null,
    createdAt: response.created_at,
    updatedAt: response.updated_at
  };
}
function taskSessionRunFromResponse(response) {
  return {
    id: response.id,
    runId: response.run_id,
    deploymentId: response.deployment_id,
    ...response.previous_run_id === undefined || response.previous_run_id === "" ? {} : { previousRunId: response.previous_run_id },
    turnIndex: response.turn_index,
    status: response.status,
    executionStatus: response.execution_status,
    ...response.terminal_outcome === undefined || response.terminal_outcome === "" ? {} : { terminalOutcome: response.terminal_outcome },
    createdAt: response.created_at,
    ...response.ended_at === undefined ? {} : { endedAt: response.ended_at }
  };
}
function channelRecordFromResponse(response) {
  return {
    id: response.id,
    channelId: response.channel_id,
    sequence: response.sequence,
    data: response.data,
    ...response.correlation_id === undefined || response.correlation_id === "" ? {} : { correlationId: response.correlation_id },
    contentType: response.content_type,
    createdAt: response.created_at
  };
}
function appendChannelRecordFromResponse(response) {
  return {
    ...channelRecordFromResponse(response.record),
    idempotencyStatus: response.idempotency_status === "duplicate" ? "duplicate" : "created"
  };
}
function sessionId(idOrHandle) {
  return typeof idOrHandle === "string" ? idOrHandle : idOrHandle.id;
}
function taskStartBody(payload, opts, maxDurationSeconds) {
  const runOptions = {
    ...opts.queue === undefined ? {} : { queue: { name: opts.queue } },
    ...opts.concurrencyKey === undefined ? {} : { concurrency_key: opts.concurrencyKey },
    ...opts.priority === undefined ? {} : { priority: opts.priority },
    ...opts.ttl === undefined ? {} : { ttl: opts.ttl },
    ...opts.retry === undefined ? {} : { retry: opts.retry },
    ...opts.metadata === undefined ? {} : { metadata: opts.metadata },
    ...opts.tags === undefined ? {} : { tags: opts.tags },
    ...opts.expiresAt === undefined ? {} : { expires_at: isoDateString(opts.expiresAt, "expiresAt") },
    ...opts.workspaceId === undefined ? {} : { workspace_id: opts.workspaceId },
    ...maxDurationSeconds === undefined ? {} : { max_duration_seconds: maxDurationSeconds },
    ...taskStartIdempotencyRequestFields(opts.idempotencyKey, opts.idempotencyKeyTTL)
  };
  return {
    ...payload === undefined ? {} : { payload },
    ...opts.externalId === undefined ? {} : { external_id: opts.externalId },
    ...Object.keys(runOptions).length === 0 ? {} : { options: runOptions }
  };
}
function taskStartPath(taskId, opts, operation) {
  const encodedTaskId = encodeURIComponent(taskId);
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    if (opts.projectId === undefined || opts.environmentId === undefined) {
      throw new Error("projectId and environmentId must be provided together");
    }
    return `/api/projects/${encodeURIComponent(opts.projectId)}/environments/${encodeURIComponent(opts.environmentId)}/tasks/${encodedTaskId}/${operation}`;
  }
  return `/api/tasks/${encodedTaskId}/${operation}`;
}
function taskSessionWaitBody(opts) {
  return {
    ...opts.timeoutSeconds === undefined ? {} : { timeout_seconds: opts.timeoutSeconds }
  };
}
function channelInputAppendBody(data, opts) {
  return {
    data,
    ...opts.correlationId === undefined ? {} : { correlation_id: opts.correlationId },
    ...opts.externalEventId === undefined ? {} : { external_event_id: opts.externalEventId }
  };
}
function taskSessionListQuery(opts) {
  const query = new URLSearchParams;
  if (opts.status !== undefined)
    query.set("status", opts.status);
  if (opts.taskId !== undefined)
    query.set("task_id", opts.taskId);
  if (opts.limit !== undefined)
    query.set("limit", String(opts.limit));
  return query.size === 0 ? "" : `?${query}`;
}
function taskSessionCollectionPath(opts) {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    if (opts.projectId === undefined || opts.environmentId === undefined) {
      throw new Error("projectId and environmentId must be provided together");
    }
    return `/api/projects/${encodeURIComponent(opts.projectId)}/environments/${encodeURIComponent(opts.environmentId)}/sessions`;
  }
  return "/api/sessions";
}
function taskSessionResourcePath(id, opts, suffix) {
  return `${taskSessionCollectionPath(opts)}/${encodeURIComponent(id)}${suffix}`;
}
function taskSessionRetrieveOptions(opts, signal) {
  return {
    ...opts.projectId === undefined ? {} : { projectId: opts.projectId },
    ...opts.environmentId === undefined ? {} : { environmentId: opts.environmentId },
    ...signal === undefined ? {} : { signal }
  };
}
function runCollectionPath(opts) {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    if (opts.projectId === undefined || opts.environmentId === undefined) {
      throw new Error("projectId and environmentId must be provided together");
    }
    return `/api/projects/${encodeURIComponent(opts.projectId)}/environments/${encodeURIComponent(opts.environmentId)}/runs`;
  }
  return "/api/runs";
}
function runResourcePath(id, opts, suffix) {
  return `${runCollectionPath(opts)}/${encodeURIComponent(id)}${suffix}`;
}
function runRetrieveOptions(opts, signal) {
  return {
    ...opts.projectId === undefined ? {} : { projectId: opts.projectId },
    ...opts.environmentId === undefined ? {} : { environmentId: opts.environmentId },
    ...signal === undefined ? {} : { signal }
  };
}
function runEventOptions(opts, cursor) {
  return {
    ...opts.projectId === undefined ? {} : { projectId: opts.projectId },
    ...opts.environmentId === undefined ? {} : { environmentId: opts.environmentId },
    ...opts.signal === undefined ? {} : { signal: opts.signal },
    ...cursor === undefined ? {} : { cursor }
  };
}
function sessionChannelQuery(opts) {
  const query = new URLSearchParams;
  if (opts.cursor !== undefined)
    query.set("after_sequence", String(opts.cursor));
  if (opts.limit !== undefined)
    query.set("limit", String(opts.limit));
  if (opts.correlationId !== undefined)
    query.set("correlation_id", opts.correlationId);
  return query.size === 0 ? "" : `?${query}`;
}
function sessionChannelStreamQuery(opts, cursor = opts.cursor) {
  const query = new URLSearchParams;
  if (cursor !== undefined)
    query.set("after_sequence", String(cursor));
  if (opts.correlationId !== undefined)
    query.set("correlation_id", opts.correlationId);
  return query.size === 0 ? "" : `?${query}`;
}
function publicAccessTokenCreateBody(opts) {
  return {
    scope: {
      type: opts.scope.type,
      session_id: sessionId(opts.scope.sessionId),
      channel: validateChannelName(opts.scope.channel),
      ...opts.scope.correlationId === undefined ? {} : { correlation_id: opts.scope.correlationId }
    },
    ...opts.expiresAt === undefined ? {} : { expires_at: isoDateString(opts.expiresAt, "expiresAt") },
    ...opts.maxUses === undefined ? {} : { max_uses: opts.maxUses }
  };
}
function publicAccessTokenFromResponse(response) {
  return {
    id: response.id,
    publicAccessToken: response.public_access_token,
    scope: {
      type: response.scope.type,
      sessionId: response.scope.session_id,
      channel: response.scope.channel,
      ...response.scope.correlation_id === undefined ? {} : { correlationId: response.scope.correlation_id }
    },
    expiresAt: response.expires_at,
    ...response.max_uses === undefined ? {} : { maxUses: response.max_uses },
    createdAt: response.created_at
  };
}
function isoDateString(value, label) {
  const date = value instanceof Date ? value : new Date(value);
  if (!Number.isFinite(date.getTime())) {
    throw new Error(`${label} must be a valid date`);
  }
  return date.toISOString();
}
function cancelRunBody(opts) {
  return {
    ...opts.reason === undefined ? {} : { reason: opts.reason },
    ...opts.force === undefined ? {} : { force: opts.force },
    ...opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }
  };
}
function scheduleCreateBody(opts) {
  return {
    ..."deduplicationKey" in opts && opts.deduplicationKey !== undefined ? { deduplication_key: opts.deduplicationKey } : {},
    ...opts.externalId === undefined ? {} : { external_id: opts.externalId },
    task: opts.task,
    cron: opts.cron,
    ...opts.timezone === undefined ? {} : { timezone: opts.timezone },
    ...opts.active === undefined ? {} : { active: opts.active },
    ...opts.options === undefined ? {} : { options: runOptionsBody(opts.options) }
  };
}
function runOptionsBody(opts) {
  if (opts === undefined)
    return {};
  return {
    ...opts.queue === undefined ? {} : { queue: { name: opts.queue } },
    ...opts.concurrencyKey === undefined ? {} : { concurrency_key: opts.concurrencyKey },
    ...opts.priority === undefined ? {} : { priority: opts.priority },
    ...opts.ttl === undefined ? {} : { ttl: opts.ttl },
    ...opts.maxDurationSeconds === undefined ? {} : { max_duration_seconds: opts.maxDurationSeconds }
  };
}
function workspaceId(idOrHandle) {
  return typeof idOrHandle === "string" ? idOrHandle : idOrHandle.id;
}
function workspaceCollectionPath(opts) {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    if (opts.projectId === undefined || opts.environmentId === undefined) {
      throw new Error("projectId and environmentId must be provided together");
    }
    return `/api/projects/${encodeURIComponent(opts.projectId)}/environments/${encodeURIComponent(opts.environmentId)}/workspaces`;
  }
  return "/api/workspaces";
}
function workspaceResourcePath(id, opts) {
  return `${workspaceCollectionPath(opts)}/${encodeURIComponent(id)}`;
}
function workspaceCreateBody(opts) {
  return {
    sandbox_id: opts.sandboxId,
    ...opts.deploymentId === undefined ? {} : { deployment_id: opts.deploymentId },
    ...opts.externalId === undefined ? {} : { external_id: opts.externalId },
    ...opts.metadata === undefined ? {} : { metadata: opts.metadata },
    ...opts.tags === undefined ? {} : { tags: opts.tags },
    ...opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey },
    ...opts.idempotencyKeyTTL === undefined ? {} : { idempotency_key_ttl: opts.idempotencyKeyTTL }
  };
}
function workspaceUpdateBody(opts) {
  return {
    ...opts.metadata === undefined ? {} : { metadata: opts.metadata },
    ...opts.tags === undefined ? {} : { tags: opts.tags }
  };
}
function workspaceMaterializeBody(opts) {
  return {};
}
function workspaceStopBody(opts) {
  return {
    ...opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey },
    ...opts.idempotencyKeyTTL === undefined ? {} : { idempotency_key_ttl: opts.idempotencyKeyTTL }
  };
}
function workspaceExecCreateBody(command, opts) {
  if (command.length === 0) {
    throw new Error("workspace.exec command must not be empty");
  }
  return {
    command: [...command],
    ...opts.cwd === undefined ? {} : { cwd: opts.cwd },
    ...opts.env === undefined ? {} : { env: opts.env },
    ...opts.detached === undefined ? {} : { detached: opts.detached },
    ...opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }
  };
}
function workspacePtyCreateBody(opts) {
  return {
    ...opts.cwd === undefined ? {} : { cwd: opts.cwd },
    ...opts.cols === undefined ? {} : { cols: opts.cols },
    ...opts.rows === undefined ? {} : { rows: opts.rows },
    ...opts.idempotencyKey === undefined ? {} : { idempotency_key: opts.idempotencyKey }
  };
}
function workspaceStreamWriteBody(data, opts) {
  if (opts.offset < 0 || !Number.isSafeInteger(opts.offset)) {
    throw new Error("workspace stream offset must be a non-negative integer");
  }
  return {
    offset: opts.offset,
    data: base64Encode2(data)
  };
}
function workspaceListQuery(opts) {
  const query = new URLSearchParams;
  if (opts.state !== undefined)
    query.set("state", opts.state);
  if (opts.externalId !== undefined)
    query.set("external_id", opts.externalId);
  if (opts.tag !== undefined)
    query.set("tag", opts.tag);
  if (opts.limit !== undefined)
    query.set("limit", String(opts.limit));
  return query.size === 0 ? "" : `?${query}`;
}
function workspacePrimitiveListQuery(opts) {
  const query = new URLSearchParams;
  if (opts.state !== undefined)
    query.set("state", opts.state);
  if (opts.limit !== undefined)
    query.set("limit", String(opts.limit));
  return query.size === 0 ? "" : `?${query}`;
}
function workspaceStreamListQuery(opts) {
  const query = new URLSearchParams;
  if (opts.cursor !== undefined)
    query.set("cursor", String(opts.cursor));
  if (opts.limit !== undefined)
    query.set("limit", String(opts.limit));
  return query.size === 0 ? "" : `?${query}`;
}
function workspaceFromResponse(response) {
  return {
    id: response.id,
    projectId: response.project_id,
    environmentId: response.environment_id,
    deploymentSandboxId: response.deployment_sandbox_id,
    sandboxId: response.sandbox_id,
    sandboxFingerprint: response.sandbox_fingerprint,
    ...response.external_id === undefined || response.external_id === "" ? {} : { externalId: response.external_id },
    currentVersionId: response.current_version_id ?? null,
    state: response.state,
    desiredState: response.desired_state,
    dirtyState: response.dirty_state,
    lastMaterializationId: response.last_materialization_id ?? null,
    metadata: response.metadata ?? {},
    tags: response.tags ?? [],
    autoStopAt: response.auto_stop_at ?? null,
    autoArchiveAt: response.auto_archive_at ?? null,
    autoDeleteAt: response.auto_delete_at ?? null,
    lastActivityAt: response.last_activity_at,
    createdAt: response.created_at,
    updatedAt: response.updated_at,
    archivedAt: response.archived_at ?? null,
    deletedAt: response.deleted_at ?? null
  };
}
function workspaceMaterializationFromResponse(response) {
  return {
    id: response.id,
    projectId: response.project_id,
    environmentId: response.environment_id,
    workspaceId: response.workspace_id,
    deploymentSandboxId: response.deployment_sandbox_id,
    baseVersionId: response.base_version_id ?? null,
    workerInstanceId: response.worker_instance_id ?? null,
    state: response.state,
    fencingGeneration: response.fencing_generation,
    dirtyGeneration: response.dirty_generation,
    reservationExpiresAt: response.reservation_expires_at ?? null,
    lastHeartbeatAt: response.last_heartbeat_at ?? null,
    createdAt: response.created_at,
    updatedAt: response.updated_at
  };
}
function workspaceStopFromResponse(response) {
  return {
    workspaceId: response.workspace_id,
    state: response.state,
    materialization: response.materialization == null ? null : workspaceMaterializationFromResponse(response.materialization)
  };
}
function workspaceExecFromResponse(response) {
  return {
    id: response.id,
    workspaceId: response.workspace_id,
    materializationId: response.materialization_id ?? null,
    command: response.command,
    cwd: response.cwd,
    envShape: response.env_shape ?? {},
    filesystemMode: workspaceFilesystemModeFromResponse(response.filesystem_mode),
    state: response.state,
    detached: response.detached,
    processId: response.process_id ?? null,
    exitCode: response.exit_code ?? null,
    signal: response.signal ?? null,
    error: response.error ?? {},
    stdoutCursor: response.stdout_cursor,
    stderrCursor: response.stderr_cursor,
    stdinCursor: response.stdin_cursor,
    stdinClosedAt: response.stdin_closed_at ?? null,
    createdAt: response.created_at,
    startedAt: response.started_at ?? null,
    exitedAt: response.exited_at ?? null,
    updatedAt: response.updated_at
  };
}
function workspacePtyFromResponse(response) {
  return {
    id: response.id,
    workspaceId: response.workspace_id,
    materializationId: response.materialization_id ?? null,
    cwd: response.cwd,
    cols: response.cols,
    rows: response.rows,
    filesystemMode: workspaceFilesystemModeFromResponse(response.filesystem_mode),
    state: response.state,
    processId: response.process_id ?? null,
    outputCursor: response.output_cursor,
    inputCursor: response.input_cursor,
    error: response.error ?? {},
    createdAt: response.created_at,
    startedAt: response.started_at ?? null,
    closedAt: response.closed_at ?? null,
    updatedAt: response.updated_at
  };
}
function workspaceStreamChunkFromResponse(response) {
  return {
    id: response.id,
    stream: response.stream,
    offsetStart: response.offset_start,
    offsetEnd: response.offset_end,
    data: base64Decode2(response.data),
    observedAt: response.observed_at,
    createdAt: response.created_at
  };
}
function workspaceStreamTerminalFromResponse(response) {
  return {
    resourceKind: response.resource_kind,
    resourceId: response.resource_id,
    stream: response.stream,
    state: response.state,
    cursor: response.cursor,
    error: response.error ?? null
  };
}
function workspaceFilesystemModeFromResponse(value) {
  if (value === "write") {
    return "write";
  }
  throw new Error(`unsupported workspace filesystem mode ${JSON.stringify(value)}`);
}
function workspaceExecTerminal(state) {
  return state === "exited" || state === "terminated" || state === "lost" || state === "failed";
}
function scheduleFromResponse(response) {
  return {
    id: response.id,
    type: response.type,
    projectId: response.project_id,
    environmentId: response.environment_id,
    task: response.task,
    ...response.deduplication_key === undefined || response.deduplication_key === "" ? {} : { deduplicationKey: response.deduplication_key },
    ...response.external_id === undefined || response.external_id === "" ? {} : { externalId: response.external_id },
    cron: response.cron,
    timezone: response.timezone,
    active: response.active,
    status: response.status,
    ...response.last_error === undefined || response.last_error === "" ? {} : { lastError: response.last_error },
    ...response.next_fire_at === undefined ? {} : { nextFireAt: response.next_fire_at },
    ...response.last_fire_at === undefined ? {} : { lastFireAt: response.last_fire_at },
    createdAt: response.created_at,
    updatedAt: response.updated_at
  };
}
function runWaitpointListQuery(opts) {
  const query = new URLSearchParams;
  if (opts.cursor !== undefined)
    query.set("cursor", opts.cursor);
  if (opts.limit !== undefined)
    query.set("limit", String(opts.limit));
  if (opts.status !== undefined)
    query.set("status", opts.status);
  return query.size === 0 ? "" : `?${query}`;
}
function waitpointTokenCreateBody(opts) {
  return {
    ...opts.timeoutInSeconds === undefined ? {} : { timeout_in_seconds: opts.timeoutInSeconds },
    ...opts.timeoutAt === undefined ? {} : { timeout_at: opts.timeoutAt },
    ...opts.tags === undefined ? {} : { tags: opts.tags },
    ...opts.metadata === undefined ? {} : { metadata: opts.metadata }
  };
}
function waitpointTokenCompleteBody(data, opts) {
  return {
    data
  };
}
function waitpointTokenCollectionPath(opts) {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    if (opts.projectId === undefined || opts.environmentId === undefined) {
      throw new Error("projectId and environmentId must be provided together");
    }
    return `/api/projects/${encodeURIComponent(opts.projectId)}/environments/${encodeURIComponent(opts.environmentId)}/waitpoints/tokens`;
  }
  return "/api/waitpoints/tokens";
}
function waitpointTokenListQuery(opts) {
  const query = new URLSearchParams;
  if (opts.cursor !== undefined)
    query.set("cursor", opts.cursor);
  if (opts.limit !== undefined)
    query.set("limit", String(opts.limit));
  if (opts.status !== undefined)
    query.set("status", opts.status);
  return query.size === 0 ? "" : `?${query}`;
}
function waitpointTokenFromResponse(response) {
  return {
    id: response.id,
    ...response.status === undefined ? {} : { status: response.status },
    callbackUrl: response.callback_url,
    ...response.public_access_token === undefined ? {} : { publicAccessToken: response.public_access_token },
    timeoutAt: response.timeout_at ?? null,
    ...response.data === undefined ? {} : { data: response.data },
    ...response.tags === undefined ? {} : { tags: response.tags },
    ...response.metadata === undefined ? {} : { metadata: response.metadata }
  };
}
function waitpointTokenPublicAccessToken(target) {
  if (typeof target === "string" || !("publicAccessToken" in target)) {
    return;
  }
  return target.publicAccessToken;
}
function requestSignal(signal) {
  return signal === undefined ? {} : { signal };
}
function base64Encode2(data) {
  const bytes = typeof data === "string" ? new TextEncoder().encode(data) : data;
  if (typeof Buffer !== "undefined") {
    return Buffer.from(bytes).toString("base64");
  }
  let binary = "";
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary);
}
function base64Decode2(data) {
  if (typeof Buffer !== "undefined") {
    return new Uint8Array(Buffer.from(data, "base64"));
  }
  const binary = atob(data);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0;index < binary.length; index += 1) {
    bytes[index] = binary.charCodeAt(index);
  }
  return bytes;
}
function waitSignal(signal, timeoutMs, timeoutError) {
  if (timeoutMs === undefined) {
    return { signal, cleanup: () => {} };
  }
  const controller = new AbortController;
  const abortFromParent = () => {
    controller.abort(signal?.reason);
  };
  if (signal?.aborted === true) {
    abortFromParent();
  } else {
    signal?.addEventListener("abort", abortFromParent, { once: true });
  }
  const timeout = setTimeout(() => controller.abort(timeoutError()), timeoutMs);
  return {
    signal: controller.signal,
    cleanup: () => {
      clearTimeout(timeout);
      signal?.removeEventListener("abort", abortFromParent);
    }
  };
}
function throwIfAborted(signal) {
  if (signal?.aborted !== true)
    return;
  if (signal.reason instanceof Error) {
    throw signal.reason;
  }
  throw new Error("operation aborted");
}
function delay(ms, signal) {
  throwIfAborted(signal);
  return new Promise((resolve, reject) => {
    const cleanup = () => {
      clearTimeout(timeout);
      signal?.removeEventListener("abort", onAbort);
    };
    const timeout = setTimeout(() => {
      cleanup();
      resolve();
    }, ms);
    const onAbort = () => {
      cleanup();
      reject(signal?.reason instanceof Error ? signal.reason : new Error("operation aborted"));
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}
function taskStartPendingRetryDelay(response) {
  const retryAfter = response.headers.get("retry-after");
  if (retryAfter === null) {
    return TASK_START_PENDING_DEFAULT_RETRY_MS;
  }
  const retryAfterSeconds = Number(retryAfter);
  if (Number.isFinite(retryAfterSeconds)) {
    if (retryAfterSeconds > 0) {
      return Math.min(retryAfterSeconds * 1000, TASK_START_PENDING_MAX_WAIT_MS);
    }
    return TASK_START_PENDING_DEFAULT_RETRY_MS;
  }
  const retryAt = Date.parse(retryAfter);
  if (Number.isFinite(retryAt)) {
    const delayMs = retryAt - Date.now();
    if (delayMs <= 0) {
      return TASK_START_PENDING_DEFAULT_RETRY_MS;
    }
    return Math.min(delayMs, TASK_START_PENDING_MAX_WAIT_MS);
  }
  return TASK_START_PENDING_DEFAULT_RETRY_MS;
}
function taskStartPendingResponse(body) {
  try {
    const decoded = JSON.parse(body);
    return decoded.code === "idempotency_pending";
  } catch {
    return false;
  }
}

class HelmrApiError extends Error {
  status;
  constructor(status, body) {
    super(`Helmr API ${status}: ${body}`);
    this.name = "HelmrApiError";
    this.status = status;
  }
}

class SseFrameTooLargeError extends Error {
  constructor() {
    super("SSE event exceeded the maximum buffer size");
    this.name = "SseFrameTooLargeError";
  }
}

class SseProtocolError extends Error {
  cursor;
  constructor(message, cursor) {
    super(message);
    this.name = "SseProtocolError";
    if (cursor !== undefined) {
      this.cursor = cursor;
    }
  }
}
async function* parseSse(response) {
  const reader = response.body?.getReader();
  if (reader === undefined) {
    return;
  }
  const decoder = new TextDecoder;
  let buffer = "";
  try {
    for (;; ) {
      const { value, done } = await reader.read();
      if (done) {
        buffer += decoder.decode();
        const finalEvent = parseSseFrame(buffer);
        if (finalEvent !== undefined) {
          yield finalEvent;
        }
        return;
      }
      buffer += decoder.decode(value, { stream: true });
      let boundary = findSseBoundary(buffer);
      while (boundary !== -1) {
        const delimiter = buffer.startsWith(`\r
\r
`, boundary) ? 4 : 2;
        const raw = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + delimiter);
        const event = parseSseFrame(raw);
        if (event !== undefined) {
          yield event;
        }
        boundary = findSseBoundary(buffer);
      }
      if (buffer.length > MAX_SSE_BUFFER_CHARS) {
        throw new SseFrameTooLargeError;
      }
    }
  } finally {
    reader.releaseLock();
  }
}
async function* parseWorkspaceStreamSse(response) {
  const reader = response.body?.getReader();
  if (reader === undefined) {
    return;
  }
  const decoder = new TextDecoder;
  let buffer = "";
  try {
    for (;; ) {
      const { value, done } = await reader.read();
      if (done) {
        buffer += decoder.decode();
        const finalEvent = parseWorkspaceStreamSseFrame(buffer);
        if (finalEvent !== undefined) {
          yield finalEvent;
        }
        return;
      }
      buffer += decoder.decode(value, { stream: true });
      let boundary = findSseBoundary(buffer);
      while (boundary !== -1) {
        const delimiter = buffer.startsWith(`\r
\r
`, boundary) ? 4 : 2;
        const raw = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + delimiter);
        const event = parseWorkspaceStreamSseFrame(raw);
        if (event !== undefined) {
          yield event;
        }
        boundary = findSseBoundary(buffer);
      }
      if (buffer.length > MAX_SSE_BUFFER_CHARS) {
        throw new SseFrameTooLargeError;
      }
    }
  } finally {
    reader.releaseLock();
  }
}
async function* parseChannelRecordSse(response) {
  const reader = response.body?.getReader();
  if (reader === undefined) {
    return;
  }
  const decoder = new TextDecoder;
  let buffer = "";
  try {
    for (;; ) {
      const { value, done } = await reader.read();
      if (done) {
        buffer += decoder.decode();
        const finalRecord = parseChannelRecordSseFrame(buffer);
        if (finalRecord !== undefined) {
          yield finalRecord;
        }
        return;
      }
      buffer += decoder.decode(value, { stream: true });
      let boundary = findSseBoundary(buffer);
      while (boundary !== -1) {
        const delimiter = buffer.startsWith(`\r
\r
`, boundary) ? 4 : 2;
        const raw = buffer.slice(0, boundary);
        buffer = buffer.slice(boundary + delimiter);
        const record = parseChannelRecordSseFrame(raw);
        if (record !== undefined) {
          yield record;
        }
        boundary = findSseBoundary(buffer);
      }
      if (buffer.length > MAX_SSE_BUFFER_CHARS) {
        throw new SseFrameTooLargeError;
      }
    }
  } finally {
    reader.releaseLock();
  }
}
function parseWorkspaceStreamSseFrame(raw) {
  const data = raw.split(/\r?\n/).filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join(`
`);
  if (data === "") {
    return;
  }
  let parsed;
  try {
    parsed = JSON.parse(data);
  } catch {
    throw new SseProtocolError("SSE workspace stream data must be valid JSON", sseFrameCursor(raw));
  }
  const event = sseFrameEvent(raw);
  if (event === "workspace_stream_chunk") {
    return { kind: "chunk", chunk: workspaceStreamChunkFromResponse(parsed) };
  }
  if (event === "workspace_stream_terminal" || event === "workspace_stream_lost") {
    return { kind: "terminal", terminal: workspaceStreamTerminalFromResponse(parsed) };
  }
  if (event === "workspace_stream_error") {
    const record = parsed;
    throw new WorkspaceStreamError(record.code, record.message, record.cursor);
  }
  throw new SseProtocolError(`unsupported workspace stream SSE event ${event}`, sseFrameCursor(raw));
}
function parseChannelRecordSseFrame(raw) {
  const data = raw.split(/\r?\n/).filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join(`
`);
  if (data === "") {
    return;
  }
  let parsed;
  try {
    parsed = JSON.parse(data);
  } catch {
    throw new SseProtocolError("SSE channel record data must be valid JSON", sseFrameCursor(raw));
  }
  const record = objectRecord(parsed);
  if (record === undefined) {
    throw new SseProtocolError("SSE channel record data must be a JSON object", sseFrameCursor(raw));
  }
  return channelRecordFromResponse(record);
}
function parseSseFrame(raw) {
  const frameCursor = sseFrameCursor(raw);
  const data = raw.split(/\r?\n/).filter((line) => line.startsWith("data:")).map((line) => line.slice(5).trimStart()).join(`
`);
  if (data === "") {
    return;
  }
  let parsed;
  try {
    parsed = JSON.parse(data);
  } catch {
    throw new SseProtocolError("SSE event data must be valid JSON", frameCursor);
  }
  const record = objectRecord(parsed);
  if (record === undefined) {
    throw new SseProtocolError("SSE event data must be a JSON object", frameCursor);
  }
  const id = stringValue(record["id"]);
  if (id === undefined) {
    throw new SseProtocolError("SSE event data must include a string id", frameCursor);
  }
  const eventCursor = parseRunEventCursor(id);
  if (eventCursor === undefined) {
    throw new SseProtocolError("SSE event data id must be a safe numeric string", frameCursor);
  }
  if (stringValue(record["kind"]) === undefined) {
    throw new SseProtocolError("SSE event data must include a string kind", eventCursor);
  }
  if (stringValue(record["message"]) === undefined) {
    throw new SseProtocolError("SSE event data must include a string message", eventCursor);
  }
  if (stringValue(record["at"]) === undefined) {
    throw new SseProtocolError("SSE event data must include a string at", eventCursor);
  }
  return parsed;
}
function sseFrameCursor(raw) {
  for (const line of raw.split(/\r?\n/)) {
    if (!line.startsWith("id:")) {
      continue;
    }
    return parseRunEventCursor(line.slice(3).trim());
  }
  return;
}
function sseFrameEvent(raw) {
  for (const line of raw.split(/\r?\n/)) {
    if (!line.startsWith("event:")) {
      continue;
    }
    return line.slice(6).trim();
  }
  return "message";
}
function findSseBoundary(buffer) {
  const lf = buffer.indexOf(`

`);
  const crlf = buffer.indexOf(`\r
\r
`);
  if (lf === -1)
    return crlf;
  if (crlf === -1)
    return lf;
  return Math.min(lf, crlf);
}
function nextRunEventCursor(cursor, event) {
  const parsed = parseRunEventCursor(event.id);
  if (parsed === undefined) {
    return cursor;
  }
  return advanceRunEventCursor(cursor, parsed);
}
function advanceRunEventCursor(cursor, parsed) {
  return cursor === undefined || parsed > cursor ? parsed : cursor;
}
function parseRunEventCursor(value) {
  if (!/^\d+$/.test(value)) {
    return;
  }
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) ? parsed : undefined;
}
function runEventRecordIsTerminal(event) {
  return runEventKindIsTerminal(event.message) || runEventKindIsTerminal(event.kind);
}
function taskSessionTerminal(status) {
  return status !== "open";
}
function runEventKindIsTerminal(kind) {
  return kind === "run.completed" || kind === "run.failed" || kind === "run.cancelled" || kind === "run.expired";
}
function runEventStreamErrorIsFatal(error) {
  if (error instanceof AuthError) {
    return true;
  }
  if (error instanceof HelmrApiError) {
    return helmrApiErrorIsFatal(error);
  }
  if (error instanceof SyntaxError) {
    return true;
  }
  if (error instanceof SseFrameTooLargeError) {
    return true;
  }
  if (error instanceof SseProtocolError) {
    return true;
  }
  return !transportErrorIsRetryable(error);
}
function workspaceStreamErrorIsFatal(error) {
  if (error instanceof WorkspaceStreamError) {
    return true;
  }
  if (error instanceof WorkspaceStreamTerminalError) {
    return true;
  }
  if (error instanceof AuthError) {
    return true;
  }
  if (error instanceof HelmrApiError) {
    return helmrApiErrorIsFatal(error);
  }
  if (error instanceof SyntaxError) {
    return true;
  }
  if (error instanceof SseFrameTooLargeError) {
    return true;
  }
  if (error instanceof SseProtocolError) {
    return true;
  }
  return !transportErrorIsRetryable(error);
}
function runEventWaitStreamErrorIsFatal(error) {
  if (error instanceof SyntaxError) {
    return false;
  }
  if (error instanceof SseFrameTooLargeError) {
    return false;
  }
  if (error instanceof SseProtocolError) {
    return false;
  }
  return runEventStreamErrorIsFatal(error);
}
function runSnapshotErrorIsFatal(error) {
  if (error instanceof AuthError) {
    return true;
  }
  if (error instanceof HelmrApiError) {
    return helmrApiErrorIsFatal(error);
  }
  if (error instanceof SyntaxError) {
    return true;
  }
  return !transportErrorIsRetryable(error);
}
function helmrApiErrorIsFatal(error) {
  return error.status >= 400 && error.status < 500 && error.status !== 408 && error.status !== 429;
}
function transportErrorIsRetryable(error) {
  if (error instanceof TypeError) {
    return true;
  }
  if (typeof DOMException !== "undefined" && error instanceof DOMException) {
    return error.name === "NetworkError" || error.name === "AbortError" || error.name === "TimeoutError";
  }
  const record = objectRecord(error);
  const cause = objectRecord(record?.["cause"]);
  const code = stringValue(record?.["code"]) ?? stringValue(cause?.["code"]);
  if (code === undefined) {
    return false;
  }
  return code === "ECONNRESET" || code === "ECONNREFUSED" || code === "EPIPE" || code === "ETIMEDOUT" || code.startsWith("UND_ERR_");
}
function runEventRecordToRunEvent(event) {
  const record = objectRecord(event);
  const message = stringValue(record?.["message"]);
  const at = stringValue(record?.["at"]);
  if (record === undefined || message === undefined || at === undefined) {
    return;
  }
  const attributes = objectRecord(record["attributes"]);
  const runId2 = stringValue(record["run_id"]) ?? stringValue(attributes?.["run_id"]) ?? "";
  if (message === "log.stdout" || message === "log.stderr") {
    const stream = message === "log.stdout" ? "stdout" : "stderr";
    return {
      type: "log",
      run_id: runId2,
      stream,
      bytes: numberValue(attributes?.["bytes"]) ?? 0,
      observed_seq: numberValue(attributes?.["observed_seq"]) ?? 0,
      at
    };
  }
  if (message === "waitpoint.created") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"]);
    const kind = stringValue(attributes?.["kind"]);
    if (waitpointId === undefined)
      return;
    if (kind === undefined)
      return;
    return {
      type: "waitpoint",
      run_id: runId2,
      waitpoint_id: waitpointId,
      kind,
      params: attributes?.["params"] ?? {},
      metadata: objectRecord(attributes?.["metadata"]) ?? {},
      tags: stringArrayValue(attributes?.["tags"]) ?? [],
      ...optionalNumber("timeout", attributes?.["timeout"]),
      at
    };
  }
  if (message === "waitpoint.completed") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"]);
    const kind = stringValue(attributes?.["kind"]);
    if (waitpointId === undefined)
      return;
    if (kind === undefined)
      return;
    return {
      type: "waitpoint_completed",
      run_id: runId2,
      waitpoint_id: waitpointId,
      kind,
      payload: attributes?.["payload"],
      at
    };
  }
  if (message === "waitpoint.timed_out") {
    const waitpointId = stringValue(attributes?.["waitpoint_id"]);
    const kind = stringValue(attributes?.["kind"]);
    if (waitpointId === undefined || kind === undefined)
      return;
    return {
      type: "waitpoint_timed_out",
      run_id: runId2,
      waitpoint_id: waitpointId,
      kind,
      at
    };
  }
  if (message === "run.completed") {
    return {
      type: "task_result",
      run_id: runId2,
      exit_code: numberValue(attributes?.["exit_code"]) ?? 0,
      at
    };
  }
  if (message === "run.failed") {
    return {
      type: "run_failed",
      run_id: runId2,
      failure_kind: stringValue(attributes?.["failure_kind"]) ?? "task_failed",
      detail: attributes?.["detail"],
      at
    };
  }
  if (message === "run.cancelled") {
    return {
      type: "run_cancelled",
      run_id: runId2,
      ...optionalString("reason", attributes?.["reason"]),
      at
    };
  }
  if (message === "run.expired") {
    return {
      type: "run_expired",
      run_id: runId2,
      ...optionalString("ttl", attributes?.["ttl"]),
      ...optionalString("message", attributes?.["message"]),
      at
    };
  }
  return;
}
function optionalString(key, value) {
  const text = stringValue(value);
  return text === undefined ? {} : { [key]: text };
}
function optionalNumber(key, value) {
  return typeof value === "number" ? { [key]: value } : {};
}
function objectRecord(value) {
  return value !== null && typeof value === "object" ? value : undefined;
}
function stringValue(value) {
  return typeof value === "string" ? value : undefined;
}
function stringArrayValue(value) {
  return Array.isArray(value) && value.every((item) => typeof item === "string") ? value : undefined;
}
function numberValue(value) {
  return typeof value === "number" ? value : undefined;
}
function decodeBase64Text(value) {
  const binary = atob(value);
  const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
  return new TextDecoder().decode(bytes);
}

// sdk/typescript/src/start.ts
var defaultClient;
function getDefaultClient() {
  defaultClient ??= new HelmrClient;
  return defaultClient;
}
function startTask(task, ...args) {
  return getDefaultClient()[startTaskClientMethod](task, ...args);
}

// sdk/typescript/src/schedules.ts
function task(config) {
  const { cron, ...taskConfig } = config;
  const schedule = cron === undefined ? undefined : {
    cron: typeof cron === "string" ? cron : cron.pattern,
    ...typeof cron === "string" || cron.timezone === undefined ? {} : { timezone: cron.timezone }
  };
  const marked = markScheduledTask({ ...taskConfig, payload: scheduledTaskPayloadSchema }, schedule);
  Object.defineProperty(marked, "start", {
    value: (...args) => startTask(marked, ...args)
  });
  return marked;
}
var scheduledTaskPayloadSchema = {
  "~standard": {
    version: 1,
    vendor: "helmr",
    validate(value) {
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        return { issues: [{ message: "expected scheduled task payload object" }] };
      }
      const input = value;
      const timestamp = parseDateField(input["timestamp"], "timestamp");
      const lastTimestamp = parseOptionalDateField(input["lastTimestamp"], "lastTimestamp");
      const timezone = input["timezone"];
      const scheduleId = input["scheduleId"];
      const scheduleType = input["scheduleType"];
      const externalId = input["externalId"];
      const upcoming = input["upcoming"];
      const issues = [
        ...timestamp.issues,
        ...lastTimestamp.issues,
        ...typeof timezone === "string" && timezone.trim() !== "" ? [] : [{ message: "expected string", path: ["timezone"] }],
        ...typeof scheduleId === "string" && scheduleId.trim() !== "" ? [] : [{ message: "expected string", path: ["scheduleId"] }],
        ...scheduleType === "declarative" || scheduleType === "imperative" ? [] : [{ message: "expected declarative or imperative", path: ["scheduleType"] }],
        ...externalId === undefined || typeof externalId === "string" ? [] : [{ message: "expected string", path: ["externalId"] }],
        ...Array.isArray(upcoming) ? [] : [{ message: "expected array", path: ["upcoming"] }]
      ];
      const upcomingDates = Array.isArray(upcoming) ? upcoming.map((item, index) => parseDateField(item, `upcoming.${index}`)) : [];
      issues.push(...upcomingDates.flatMap((item) => item.issues));
      if (issues.length > 0) {
        return { issues };
      }
      return {
        value: {
          timestamp: timestamp.value,
          ...lastTimestamp.value === undefined ? {} : { lastTimestamp: lastTimestamp.value },
          timezone,
          scheduleId,
          scheduleType,
          ...externalId === undefined ? {} : { externalId },
          upcoming: upcomingDates.map((item) => item.value)
        }
      };
    }
  }
};
function parseDateField(value, path) {
  if (typeof value !== "string") {
    return { value: new Date(0), issues: [{ message: "expected ISO timestamp", path: path.split(".") }] };
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return { value: new Date(0), issues: [{ message: "expected ISO timestamp", path: path.split(".") }] };
  }
  return { value: date, issues: [] };
}
function parseOptionalDateField(value, path) {
  if (value === undefined || value === null) {
    return { issues: [] };
  }
  return parseDateField(value, path);
}
// sdk/typescript/src/index.ts
var runs = new Proxy({}, {
  get(_target, property, receiver) {
    return Reflect.get(getDefaultClient().runs, property, receiver);
  }
});
var channel = Object.freeze({
  input(id, opts) {
    return Object.freeze({ id: validateChannelName(id), schema: opts.schema });
  },
  output(id, opts) {
    return Object.freeze({ id: validateChannelName(id), schema: opts.schema });
  }
});
var metadata = Object.freeze({
  set(key, value) {
    return getRunRuntime().metadataSet(key, value);
  },
  patch(value) {
    return getRunRuntime().metadataPatch(value);
  },
  increment(key, amount = 1) {
    return getRunRuntime().metadataIncrement(key, amount);
  }
});
var wait = Object.freeze({
  async createToken(opts = {}) {
    if (runRuntimeIsActive()) {
      return await getRunRuntime().createWaitpointToken(normalizeRuntimeWaitpointTokenCreateOptions(opts));
    }
    return await getDefaultClient().waitpoints.tokens.create(normalizeWaitpointTokenCreateOptions(opts));
  },
  retrieveToken(id, opts) {
    return getDefaultClient().waitpoints.tokens.retrieve(id, opts);
  },
  async listTokens(opts) {
    return await getDefaultClient().waitpoints.tokens.list(opts);
  },
  listTokensPage(opts) {
    return getDefaultClient().waitpoints.tokens.listPage(opts);
  },
  for(input) {
    return waitDelayHandle({ type: "for", input }, () => getRunRuntime().waitFor(input));
  },
  until(input) {
    return waitDelayHandle({ type: "until", input }, () => getRunRuntime().waitUntil(input));
  },
  forToken(token, opts) {
    const tokenId = typeof token === "string" ? token : token.id;
    const tokenTimeoutAt = typeof token === "string" ? undefined : ("timeoutAt" in token) ? token.timeoutAt : undefined;
    const timeout = opts?.timeout ?? waitpointTokenTimeoutInput(tokenTimeoutAt);
    const schema = opts?.schema;
    const options = {
      params: {
        token_id: tokenId
      },
      ...timeout === undefined ? {} : { timeout },
      ...opts?.metadata === undefined ? {} : { metadata: opts.metadata },
      ...opts?.tags === undefined ? {} : { tags: opts.tags },
      ...schema === undefined ? {} : { schema }
    };
    const operand = { type: "waitpoint", options };
    return waitpointHandle(operand, () => getRunRuntime().waitpoint(options));
  },
  async all(operands) {
    const normalized = operands.map((operand, index) => {
      const waitOperand = getRuntimeWaitOperand(operand);
      if (waitOperand === undefined) {
        throw new Error(`wait.all operand at index ${index} is not a Helmr wait handle`);
      }
      return waitOperand;
    });
    if (normalized.length === 0) {
      throw new Error("wait.all requires at least one operand");
    }
    return await getRunRuntime().waitAll(normalized);
  },
  completeToken(token, data, opts) {
    return getDefaultClient().waitpoints.tokens.complete(token, data, opts);
  }
});
var logger = Object.freeze({
  info(...values) {
    getRunRuntime().log("info", values);
  },
  warn(...values) {
    getRunRuntime().log("warn", values);
  },
  error(...values) {
    getRunRuntime().log("error", values);
  }
});
var schedules = Object.freeze({
  task,
  create: (...args) => getDefaultClient().schedules.create(...args),
  update: (...args) => getDefaultClient().schedules.update(...args),
  list: (...args) => getDefaultClient().schedules.list(...args),
  retrieve: (...args) => getDefaultClient().schedules.retrieve(...args),
  activate: (...args) => getDefaultClient().schedules.activate(...args),
  deactivate: (...args) => getDefaultClient().schedules.deactivate(...args),
  delete: (...args) => getDefaultClient().schedules.delete(...args)
});
function waitpointHandle(operand, factory) {
  let promise;
  const getPromise = () => {
    promise ??= factory();
    return promise;
  };
  const handle = {
    then(onfulfilled, onrejected) {
      return getPromise().then(onfulfilled, onrejected);
    },
    unwrap: async () => (await getPromise()).unwrap()
  };
  Object.defineProperty(handle, runtimeWaitOperand, { value: operand });
  return handle;
}
function waitDelayHandle(operand, factory) {
  let promise;
  const getPromise = () => {
    promise ??= factory();
    return promise;
  };
  const handle = {
    then(onfulfilled, onrejected) {
      return getPromise().then(onfulfilled, onrejected);
    },
    unwrap: () => getPromise()
  };
  Object.defineProperty(handle, runtimeWaitOperand, { value: operand });
  return handle;
}
function runRuntimeIsActive() {
  try {
    getRunRuntime();
    return true;
  } catch {
    return false;
  }
}
function normalizeWaitpointTokenCreateOptions(opts) {
  const { timeout, ...clientOpts } = opts;
  if (timeout === undefined) {
    return clientOpts;
  }
  if (clientOpts.timeoutAt !== undefined || clientOpts.timeoutInSeconds !== undefined) {
    throw new Error("wait.createToken timeout cannot be combined with timeoutAt or timeoutInSeconds");
  }
  const { timeoutAt: _timeoutAt, timeoutInSeconds: _timeoutInSeconds, ...baseOpts } = clientOpts;
  return {
    ...baseOpts,
    timeoutInSeconds: Math.ceil(waitDurationMilliseconds(timeout) / 1000)
  };
}
function normalizeRuntimeWaitpointTokenCreateOptions(opts) {
  if (opts.projectId !== undefined || opts.environmentId !== undefined) {
    throw new Error("wait.createToken cannot override projectId or environmentId inside a running task");
  }
  if (opts.signal !== undefined) {
    throw new Error("wait.createToken signal is not supported inside a running task");
  }
  return normalizeWaitpointTokenCreateOptions(opts);
}
function waitpointTokenTimeoutInput(timeoutAt) {
  if (timeoutAt === null || timeoutAt === undefined) {
    return;
  }
  const at = timeoutAt instanceof Date ? timeoutAt : new Date(timeoutAt);
  const milliseconds = at.getTime() - Date.now();
  if (!Number.isFinite(milliseconds)) {
    throw new Error("wait.forToken token timeoutAt must be a valid date");
  }
  return { milliseconds: Math.max(1, milliseconds) };
}
function waitDurationMilliseconds(input) {
  if (typeof input === "number")
    return positiveMilliseconds(input * 1000);
  if (typeof input === "string")
    return parseDurationMilliseconds(input);
  if (input.milliseconds !== undefined)
    return positiveMilliseconds(input.milliseconds);
  if (input.seconds !== undefined)
    return positiveMilliseconds(input.seconds * 1000);
  if (input.minutes !== undefined)
    return positiveMilliseconds(input.minutes * 60000);
  if (input.hours !== undefined)
    return positiveMilliseconds(input.hours * 3600000);
  if (input.duration !== undefined)
    return parseDurationMilliseconds(input.duration);
  throw new Error("duration requires milliseconds, seconds, minutes, hours, or duration");
}
function parseDurationMilliseconds(value) {
  const match = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(value.trim());
  if (match === null) {
    throw new Error("duration must use ms, s, m, or h units");
  }
  const amount = Number(match[1]);
  const unit = match[2];
  return positiveMilliseconds(amount * (unit === "ms" ? 1 : unit === "s" ? 1000 : unit === "m" ? 60000 : 3600000));
}
function positiveMilliseconds(value) {
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`duration must be positive: ${value}`);
  }
  return Math.ceil(value);
}

// runtime/typescript/src/main.ts
import { createWriteStream } from "node:fs";
import { createConnection } from "node:net";
import { resolve as resolve2 } from "node:path";
import { inspect } from "node:util";

// sdk/typescript/src/compile.ts
import { createHash as createHash2 } from "node:crypto";
var IMAGE_FORMAT_VERSION = 0;
var IMAGE_KEY_DOMAIN = `helmr.image.v0
`;
function compile(opts) {
  const task3 = opts.task;
  if (!isSandboxBuilder(task3.sandbox)) {
    throw new Error(`task "${task3.id}" must declare sandbox: sandbox(...)`);
  }
  const compiler = new BundleCompiler;
  const imageSpec = compiler.compileSandboxImage(task3.sandbox);
  const subImages = compiler.compileSubImages(imageSpec);
  const workspace = compiler.compileWorkspace(task3.sandbox);
  const resources = task3.sandbox.resourceSpec;
  const network = task3.sandbox.networkSpec;
  const maxDurationSeconds = readOptionalMaxDurationSeconds(task3.maxDuration, `task "${task3.id}" maxDuration`);
  const sandboxSpec = create(SandboxSpecSchema, {
    id: task3.sandbox.id,
    workspace,
    ...resources ? {
      resources: create(ResourcesSchema, {
        ...resources.cpu === undefined ? {} : { cpu: resources.cpu },
        ...resources.memory === undefined ? {} : { memory: resources.memory },
        ...resources.disk === undefined ? {} : { disk: resources.disk }
      })
    } : {},
    ...network ? { network: compileNetwork(network) } : {}
  });
  return create(BundleSchema, {
    image: imageSpec,
    sandbox: sandboxSpec,
    subImages,
    task: create(TaskSpecSchema, {
      id: task3.id,
      sandboxId: task3.sandbox.id,
      modulePath: opts.modulePath,
      exportName: opts.exportName ?? "default",
      maxDurationSeconds,
      queue: create(QueueSpecSchema, {
        name: task3.queue?.name ?? defaultTaskQueueName(task3.id),
        ...task3.queue?.concurrencyLimit === undefined || task3.queue.concurrencyLimit === null ? {} : { concurrencyLimit: task3.queue.concurrencyLimit }
      }),
      ...task3.ttl === undefined ? {} : { ttl: task3.ttl },
      retryPolicyJson: JSON.stringify(task3.retry ?? false),
      schedules: compileTaskSchedules(task3.schedule),
      secrets: Object.entries(readSecretDecls(task3.secrets)).map(([name, placement]) => create(SecretPlacementSchema, {
        name,
        placement: compilePlacement(placement)
      }))
    })
  });
}
function compileNetwork(network) {
  return create(NetworkPolicySchema, {
    internet: network.internet,
    allow: [...network.allow],
    deny: [...network.deny]
  });
}
function compileTaskSchedules(schedule) {
  if (schedule === undefined) {
    return [];
  }
  return [
    create(TaskScheduleSpecSchema, {
      id: "",
      cron: schedule.cron,
      timezone: schedule.timezone ?? "UTC"
    })
  ];
}
function compilePlacement(placement) {
  if ("env" in placement) {
    return create(PlacementSchema, {
      kind: {
        case: "env",
        value: create(EnvPlacementSchema, {
          name: placement.env
        })
      }
    });
  }
  if ("file" in placement) {
    return create(PlacementSchema, {
      kind: {
        case: "file",
        value: create(FilePlacementSchema, {
          path: placement.file,
          ...placement.mode === undefined ? {} : { mode: placement.mode },
          ...placement.owner === undefined ? {} : { owner: placement.owner }
        })
      }
    });
  }
  return create(PlacementSchema, {
    kind: {
      case: "dir",
      value: create(DirPlacementSchema, {
        path: placement.dir,
        ...placement.mode === undefined ? {} : { mode: placement.mode },
        ...placement.owner === undefined ? {} : { owner: placement.owner }
      })
    }
  });
}

class BundleCompiler {
  imageSpecs = new Map;
  compileSandboxImage(sandbox2) {
    const image = sandbox2.imageBuilder;
    if (!image) {
      throw new Error(`sandbox "${sandbox2.id}" must declare image(...)`);
    }
    return this.compileImage(image);
  }
  compileWorkspace(sandbox2) {
    const workspace = sandbox2.workspaceBinding ?? {
      mountPath: "/workspace"
    };
    return create(WorkspaceRuntimeBindingSchema, {
      mountPath: workspace.mountPath
    });
  }
  compileImage(image) {
    const existing = this.imageSpecs.get(image);
    if (existing) {
      return existing;
    }
    if (image.steps.length === 0) {
      throw new Error(`image "${image.id}" must contain at least one operation`);
    }
    const spec = create(ImageSpecSchema, {
      formatVersion: IMAGE_FORMAT_VERSION,
      platform: create(PlatformSchema, { os: "linux", architecture: currentArchitecture() }),
      steps: image.steps.map((step) => this.compileBuildStep(step))
    });
    this.imageSpecs.set(image, spec);
    return spec;
  }
  compileSubImages(root) {
    const values = {};
    for (const spec of this.imageSpecs.values()) {
      if (spec === root) {
        continue;
      }
      values[compileProvisionalImageKey(spec)] = spec;
    }
    return values;
  }
  compileBuildStep(step) {
    switch (step.kind) {
      case "from":
        return create(ImageStepSchema, {
          kind: { case: "from", value: create(FromSchema, { ref: step.ref }) }
        });
      case "run":
        return create(ImageStepSchema, {
          kind: {
            case: "run",
            value: create(RunSchema, {
              argv: [...step.argv],
              cacheMounts: step.cache.map((binding) => create(CacheMountBindingSchema, {
                dst: binding.mountPath,
                cacheId: binding.cache.id,
                sharing: "locked"
              })),
              secretMounts: step.secrets.map((binding) => create(SecretMountBindingSchema, {
                dst: binding.mountPath,
                secretRef: create(SecretRefSchema, { name: binding.secret })
              }))
            })
          }
        });
      case "copy":
        return this.compileCopyStep(step.dest, step.source);
      case "copyFrom":
        return create(ImageStepSchema, {
          kind: {
            case: "copyFromImage",
            value: create(CopyFromImageSchema, {
              dst: step.dest,
              srcImageKey: compileProvisionalImageKey(this.compileImage(step.source)),
              srcPath: step.srcPath
            })
          }
        });
      case "workdir":
        return create(ImageStepSchema, {
          kind: { case: "workdir", value: create(WorkdirSchema, { path: step.path }) }
        });
      case "env":
        return create(ImageStepSchema, {
          kind: { case: "env", value: create(EnvSchema, { key: step.key, value: step.value }) }
        });
      case "user":
        return create(ImageStepSchema, {
          kind: { case: "user", value: create(UserSchema, { name: step.name }) }
        });
    }
  }
  compileCopyStep(dest, source) {
    if (isSourceFileRef(source)) {
      return create(ImageStepSchema, {
        kind: {
          case: "copySourceFile",
          value: create(CopySourceFileSchema, {
            dst: dest,
            srcRef: create(SourceFileRefSchema, { path: source.path })
          })
        }
      });
    }
    if (isSourceDirRef(source)) {
      return create(ImageStepSchema, {
        kind: {
          case: "copySourceDir",
          value: create(CopySourceDirSchema, {
            dst: dest,
            srcRef: this.compileSourceDirRef(source),
            ignore: [...source.ignore]
          })
        }
      });
    }
    if (isImageBuilder(source)) {
      return create(ImageStepSchema, {
        kind: {
          case: "copyFromImage",
          value: create(CopyFromImageSchema, {
            dst: dest,
            srcImageKey: compileProvisionalImageKey(this.compileImage(source)),
            srcPath: "/"
          })
        }
      });
    }
    throw new Error("image.copy() source must be source.file(), source.directory(), or image()");
  }
  compileSourceDirRef(ref) {
    return create(SourceDirRefSchema, { path: ref.path, ignore: [...ref.ignore] });
  }
}
function currentArchitecture() {
  switch (process.arch) {
    case "arm64":
      return "arm64";
    case "x64":
      return "amd64";
    default:
      return process.arch;
  }
}
function compileProvisionalImageKey(image) {
  return canonicalImageKey(image);
}
function canonicalImageKey(image) {
  const hash = createHash2("sha256");
  hash.update(IMAGE_KEY_DOMAIN);
  hash.update(u32be(image.formatVersion));
  hashLenPrefixedBytes(hash, image.platform ? toBinary(PlatformSchema, image.platform) : new Uint8Array);
  hashLenPrefixedBytes(hash, encodeImageSteps(image.steps));
  hashLenPrefixedBytes(hash, encodeDigestList(sourceInputDigests(image.steps)));
  hashLenPrefixedBytes(hash, encodeDigestList(subImageKeys(image.steps)));
  return `sha256:${hash.digest("hex")}`;
}
function encodeImageSteps(steps) {
  const chunks = [u64be(steps.length)];
  for (const step of steps) {
    chunks.push(lenPrefixedBytes(toBinary(ImageStepSchema, step)));
  }
  return concatBytes(chunks);
}
function encodeDigestList(values) {
  const chunks = [u64be(values.length)];
  for (const value of values) {
    chunks.push(lenPrefixedBytes(new TextEncoder().encode(value)));
  }
  return concatBytes(chunks);
}
function sourceInputDigests(steps) {
  const values = [];
  for (const step of steps) {
    switch (step.kind.case) {
      case "copySourceFile":
        values.push(step.kind.value.digest);
        break;
      case "copySourceDir":
        values.push(step.kind.value.treeDigest);
        break;
    }
  }
  return values;
}
function subImageKeys(steps) {
  const values = [];
  for (const step of steps) {
    if (step.kind.case === "copyFromImage") {
      values.push(step.kind.value.srcImageKey);
    }
  }
  return values;
}
function hashLenPrefixedBytes(hash, bytes) {
  hash.update(u64be(bytes.byteLength));
  hash.update(bytes);
}
function lenPrefixedBytes(bytes) {
  return concatBytes([u64be(bytes.byteLength), bytes]);
}
function u32be(value) {
  const buffer = Buffer.alloc(4);
  buffer.writeUInt32BE(value);
  return buffer;
}
function u64be(value) {
  const buffer = Buffer.alloc(8);
  buffer.writeBigUInt64BE(BigInt(value));
  return buffer;
}
function concatBytes(chunks) {
  const total = chunks.reduce((sum, chunk) => sum + chunk.byteLength, 0);
  const out = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    out.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return out;
}
function readSecretDecls(value) {
  if (value === undefined) {
    return {};
  }
  if (!Array.isArray(value)) {
    throw new Error("task secrets must be an array");
  }
  const output = {};
  value.forEach((item, index) => {
    if (item === null || typeof item !== "object" || Array.isArray(item)) {
      throw new Error(`task secrets.${index} must be a secret object`);
    }
    const record = item;
    const name = record["name"];
    if (typeof name !== "string") {
      throw new Error(`task secrets.${index}.name must be a string`);
    }
    validateSecretName(name, `task secrets.${index}.name`);
    if (Object.hasOwn(output, name)) {
      throw new Error(`task secrets contains duplicate secret ${JSON.stringify(name)}`);
    }
    const { name: _name, ...placement } = record;
    output[name] = readPlacement(placement, `task secrets.${index}`);
  });
  return output;
}
function readPlacement(value, label) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be a placement object`);
  }
  const record = value;
  if ("env" in record) {
    const env = record["env"];
    if (Object.keys(record).length !== 1 || !isNonEmptyPlacementString(env)) {
      throw new Error(`${label} must be { env: string }`);
    }
    validateEnvPlacementName(env, `${label}.env`);
    return { env };
  }
  if ("file" in record) {
    const file = record["file"];
    const mode = readOptionalPlacementString(record, "mode");
    const owner = readOptionalPlacementString(record, "owner");
    if (!hasOnlyKeys(record, ["file", "mode", "owner"]) || !isNonEmptyPlacementString(file)) {
      throw new Error(`${label} must be { file: string, mode?: string, owner?: string }`);
    }
    if (mode === INVALID_PLACEMENT_STRING || owner === INVALID_PLACEMENT_STRING) {
      throw new Error(`${label} must be { file: string, mode?: string, owner?: string }`);
    }
    validatePlacementPath(file, `${label}.file`);
    validatePlacementMode(mode, `${label}.mode`);
    return {
      file,
      ...mode === undefined ? {} : { mode },
      ...owner === undefined ? {} : { owner }
    };
  }
  if ("dir" in record) {
    const dir = record["dir"];
    const mode = readOptionalPlacementString(record, "mode");
    const owner = readOptionalPlacementString(record, "owner");
    if (!hasOnlyKeys(record, ["dir", "mode", "owner"]) || !isNonEmptyPlacementString(dir)) {
      throw new Error(`${label} must be { dir: string, mode?: string, owner?: string }`);
    }
    if (mode === INVALID_PLACEMENT_STRING || owner === INVALID_PLACEMENT_STRING) {
      throw new Error(`${label} must be { dir: string, mode?: string, owner?: string }`);
    }
    validatePlacementPath(dir, `${label}.dir`);
    validatePlacementMode(mode, `${label}.mode`);
    return {
      dir,
      ...mode === undefined ? {} : { mode },
      ...owner === undefined ? {} : { owner }
    };
  }
  throw new Error(`${label} must be one of { env }, { file, mode?, owner? }, or { dir, mode?, owner? }`);
}
var INVALID_PLACEMENT_STRING = Symbol("invalid placement string");
function isNonEmptyPlacementString(value) {
  return typeof value === "string" && value.trim() !== "";
}
function readOptionalPlacementString(record, key) {
  const value = record[key];
  if (value === undefined) {
    return;
  }
  return typeof value === "string" ? value : INVALID_PLACEMENT_STRING;
}
function validatePlacementMode(mode, label) {
  if (mode === undefined) {
    return;
  }
  const normalized = mode.trim().replace(/^0[oO]/, "");
  if (!/^[0-7]+$/.test(normalized)) {
    throw new Error(`${label} must be an octal permission mode`);
  }
  if (Number.parseInt(normalized, 8) > 511) {
    throw new Error(`${label} must only contain permission bits`);
  }
}
function validateEnvPlacementName(value, label) {
  if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(value)) {
    throw new Error(`${label} must match /^[A-Za-z_][A-Za-z0-9_]*$/`);
  }
}
function validatePlacementPath(path, label) {
  const normalized = path.trim().replaceAll("\\", "/");
  if (path !== path.trim()) {
    throw new Error(`${label} must not contain leading or trailing whitespace`);
  }
  if (normalized === "." || normalized === "/") {
    throw new Error(`${label} must target a file or directory`);
  }
  if (normalized.split("/").includes("..")) {
    throw new Error(`${label} must not contain parent components`);
  }
}
function hasOnlyKeys(record, allowed) {
  return Object.keys(record).every((key) => allowed.includes(key));
}

// runtime/typescript/src/config.ts
import { readdir, stat } from "node:fs/promises";
import { relative, resolve, sep } from "node:path";
import { pathToFileURL } from "node:url";

class MissingConfigError extends Error {
  constructor(cwd) {
    super(`no helmr.config.ts found at ${resolve(cwd, "helmr.config.ts")}`);
    this.name = "MissingConfigError";
  }
}
var nextImportVersion = 0;
var TASK_FILE_EXTENSION = /\.(?:ts|mts|cts|js|mjs|cjs)$/;
var DECLARATION_FILE_EXTENSION = /\.d\.(?:ts|mts|cts)$/;
var DEFAULT_IGNORE_PATTERNS = [
  "**/*.test.*",
  "**/*.spec.*",
  "**/_*.*"
];
var HARD_IGNORE_PATTERNS = [
  "**/node_modules/**",
  "**/.git/**",
  "**/.helmr/**",
  "**/.next/**"
];
async function loadConfigTaskRefs(cwd) {
  const config = await loadConfig(cwd);
  const taskFiles = await discoverTaskFiles(cwd, config);
  return collectTaskRefs(cwd, await importDiscoveredTaskModules(taskFiles));
}
async function loadConfig(cwd) {
  const configPath = resolve(cwd, "helmr.config.ts");
  await assertConfigFileExists(cwd, configPath);
  let moduleValue;
  try {
    moduleValue = await importProjectModule(configPath, "helmr.config.ts");
  } catch (error) {
    const message = formatConfigLoadError(error);
    const duplicate = parseDuplicateTaskIdError(message);
    if (duplicate !== null) {
      throw duplicate;
    }
    throw new Error(`failed to load helmr.config.ts: ${message}`);
  }
  return readDefaultConfig(moduleValue);
}
async function loadTaskRegistry(cwd) {
  return buildTaskRegistry(await loadConfigTaskRefs(cwd));
}
function buildTaskRegistry(refs) {
  const registry2 = new Map;
  for (const ref of refs) {
    const existing = registry2.get(ref.id);
    if (existing) {
      throw new DuplicateTaskIdError(ref.id, [existing.originFile, ref.originFile]);
    }
    registry2.set(ref.id, {
      originFile: ref.originFile,
      modulePath: ref.modulePath,
      exportName: ref.exportName,
      task: ref.task,
      bundle: compile({ task: ref.task, modulePath: ref.modulePath, exportName: ref.exportName })
    });
  }
  return registry2;
}
function readDefaultConfig(moduleValue) {
  if (moduleValue === null || typeof moduleValue !== "object" || !("default" in moduleValue)) {
    throw new Error("helmr.config.ts must default export defineConfig({ project, dirs: [...] })");
  }
  const config = moduleValue.default;
  if (!isConfigDefinition(config)) {
    throw new Error("helmr.config.ts must default export defineConfig({ project, dirs: [...] })");
  }
  return config;
}
async function discoverTaskFiles(cwd, config) {
  const matchers = compileIgnoreMatchers([
    ...config.ignorePatterns ?? DEFAULT_IGNORE_PATTERNS,
    ...HARD_IGNORE_PATTERNS
  ]);
  const files = [];
  for (const dir of config.dirs) {
    const root = resolve(cwd, dir);
    assertInsideProjectRoot(cwd, root, dir);
    await assertTaskDirExists(root, dir);
    await appendTaskFiles(cwd, root, matchers, files);
  }
  const uniqueFiles = [...new Set(files)];
  uniqueFiles.sort((left, right) => compareAscii(projectRelativePath(cwd, left), projectRelativePath(cwd, right)));
  if (uniqueFiles.length === 0) {
    throw new Error(`no task files found in configured dirs:
${config.dirs.map((dir) => `  - ${dir}`).join(`
`)}`);
  }
  return uniqueFiles;
}
function compareAscii(left, right) {
  if (left < right)
    return -1;
  if (left > right)
    return 1;
  return 0;
}
function assertInsideProjectRoot(cwd, path, configuredDir) {
  const rel = relative(cwd, path);
  if (rel === ".." || rel.startsWith(`..${sep}`)) {
    throw new Error(`configured task dir must be inside the project root: ${configuredDir}`);
  }
}
async function assertTaskDirExists(path, configuredDir) {
  let metadata2;
  try {
    metadata2 = await stat(path);
  } catch (error) {
    if (error?.code === "ENOENT") {
      throw new Error(`configured task dir not found: ${configuredDir}`);
    }
    throw error;
  }
  if (!metadata2.isDirectory()) {
    throw new Error(`configured task dir is not a directory: ${configuredDir}`);
  }
}
async function appendTaskFiles(cwd, dir, ignoreMatchers, files) {
  const entries = await readdir(dir, { withFileTypes: true });
  for (const entry of entries) {
    const path = resolve(dir, entry.name);
    const rel = projectRelativePath(cwd, path);
    if (entry.isDirectory()) {
      if (!isIgnored(`${rel}/`, ignoreMatchers)) {
        await appendTaskFiles(cwd, path, ignoreMatchers, files);
      }
      continue;
    }
    if (!entry.isFile() || !isTaskFile(path) || isIgnored(rel, ignoreMatchers)) {
      continue;
    }
    files.push(path);
  }
}
function isTaskFile(path) {
  return TASK_FILE_EXTENSION.test(path) && !DECLARATION_FILE_EXTENSION.test(path);
}
function compileIgnoreMatchers(patterns) {
  return patterns.map((pattern) => globPatternToRegExp(pattern));
}
function isIgnored(path, matchers) {
  return matchers.some((matcher) => matcher.test(path));
}
function globPatternToRegExp(pattern) {
  const normalized = pattern.split(sep).join("/");
  let source = "^";
  for (let index = 0;index < normalized.length; ) {
    const char = normalized[index];
    const next = normalized[index + 1];
    const afterNext = normalized[index + 2];
    if (char === "*" && next === "*" && afterNext === "/") {
      source += "(?:.*/)?";
      index += 3;
      continue;
    }
    if (char === "*" && next === "*") {
      source += ".*";
      index += 2;
      continue;
    }
    if (char === "*") {
      source += "[^/]*";
      index += 1;
      continue;
    }
    if (char === "?") {
      source += "[^/]";
      index += 1;
      continue;
    }
    source += escapeRegExp(char ?? "");
    index += 1;
  }
  return new RegExp(`${source}$`);
}
function escapeRegExp(value) {
  return /[\\^$.*+?()[\]{}|]/.test(value) ? `\\${value}` : value;
}
async function importDiscoveredTaskModules(files) {
  return Promise.all(files.map(async (file) => ({
    path: file,
    exports: await importProjectModule(file, `task module ${file}`)
  })));
}
async function importProjectModule(path, label) {
  const moduleValue = await import(`${pathToFileURL(path).href}?helmr=${Date.now()}-${mintImportVersion()}`);
  if (moduleValue === null || typeof moduleValue !== "object") {
    throw new Error(`${label} did not export an object`);
  }
  return moduleValue;
}
function mintImportVersion() {
  nextImportVersion += 1;
  return String(nextImportVersion);
}
function collectTaskRefs(cwd, modules) {
  const refs = [];
  for (const mod of modules) {
    const seen = new WeakSet;
    for (const [exportName, value] of Object.entries(mod.exports)) {
      if (!isTaskDefinition(value)) {
        continue;
      }
      if (exportName === "default") {
        throw new Error(`task file ${projectRelativePath(cwd, mod.path)} default-exports task "${value.id}"; use a named export instead`);
      }
      if (seen.has(value)) {
        continue;
      }
      seen.add(value);
      refs.push({
        id: value.id,
        originFile: mod.path,
        modulePath: projectRelativePath(cwd, mod.path),
        exportName,
        task: value
      });
    }
  }
  if (refs.length === 0) {
    throw new Error("no named exports created by task(...) were found in configured dirs");
  }
  return refs;
}
async function assertConfigFileExists(cwd, configPath) {
  try {
    const metadata2 = await stat(configPath);
    if (!metadata2.isFile()) {
      throw new MissingConfigError(cwd);
    }
  } catch (error) {
    if (error?.code === "ENOENT") {
      throw new MissingConfigError(cwd);
    }
    throw error;
  }
}
function projectRelativePath(cwd, path) {
  if (path === "unknown") {
    return "unknown";
  }
  for (const root of equivalentRoots(cwd)) {
    const rel = relative(root, path);
    if (!rel.startsWith("..") && rel !== "" && !rel.startsWith(`..${sep}`)) {
      return rel.split(sep).join("/");
    }
  }
  return path;
}
function equivalentRoots(path) {
  const roots = [path];
  if (path.startsWith("/var/")) {
    roots.push(`/private${path}`);
  } else if (path.startsWith("/private/var/")) {
    roots.push(path.slice("/private".length));
  }
  return roots;
}
function formatConfigLoadError(error) {
  if (error instanceof Error) {
    return error.message;
  }
  return String(error);
}
function parseDuplicateTaskIdError(message) {
  const match = /^duplicate task id "([^"]+)":\n  - ([^\n]+)\n  - ([^\n]+)/.exec(message);
  if (!match?.[1] || !match[2] || !match[3]) {
    return null;
  }
  return new DuplicateTaskIdError(match[1], [match[2], match[3]]);
}

class DuplicateTaskIdError extends Error {
  id;
  originFiles;
  constructor(id, originFiles) {
    super(`duplicate task id "${id}":
  - ${originFiles[0]}
  - ${originFiles[1]}`);
    this.name = "DuplicateTaskIdError";
    this.id = id;
    this.originFiles = originFiles;
  }
}

// sdk/typescript/src/fuzzy.ts
function levenshteinDistance(left, right) {
  if (left === right) {
    return 0;
  }
  if (left.length === 0) {
    return right.length;
  }
  if (right.length === 0) {
    return left.length;
  }
  const previous = new Array(right.length + 1);
  const current = new Array(right.length + 1);
  for (let column = 0;column <= right.length; column += 1) {
    previous[column] = column;
  }
  for (let row = 1;row <= left.length; row += 1) {
    current[0] = row;
    for (let column = 1;column <= right.length; column += 1) {
      const cost = left[row - 1] === right[column - 1] ? 0 : 1;
      current[column] = Math.min(current[column - 1] + 1, previous[column] + 1, previous[column - 1] + cost);
    }
    previous.splice(0, previous.length, ...current);
  }
  return previous[right.length] ?? right.length;
}

// runtime/typescript/src/registry.ts
class TaskNotFoundError extends Error {
  taskId;
  available;
  suggestion;
  constructor(taskId, available, suggestion) {
    super(formatMissingTaskMessage(taskId, available, suggestion));
    this.name = "TaskNotFoundError";
    this.taskId = taskId;
    this.available = available;
    this.suggestion = suggestion;
  }
}
function lookupRegisteredTask(registry2, taskId) {
  const task3 = registry2.get(taskId);
  if (task3) {
    return task3;
  }
  const available = [...registry2.keys()].sort();
  throw new TaskNotFoundError(taskId, available, closestTaskId(taskId, available));
}
function closestTaskId(taskId, available) {
  let bestId = null;
  let bestDistance = Number.POSITIVE_INFINITY;
  for (const candidate of available) {
    const distance = levenshteinDistance(taskId, candidate);
    if (distance < bestDistance) {
      bestId = candidate;
      bestDistance = distance;
    }
  }
  if (bestId === null) {
    return null;
  }
  const threshold = Math.min(Math.max(2, Math.ceil(Math.max(taskId.length, bestId.length) * 0.34)), Math.floor(Math.min(taskId.length, bestId.length) / 2));
  return bestDistance <= threshold ? bestId : null;
}
function formatMissingTaskMessage(taskId, available, suggestion) {
  const hint = suggestion === null ? "" : ` (did you mean "${suggestion}"?)`;
  const availableLine = available.length === 0 ? "available: (none)" : `available: ${available.join(", ")}`;
  return `task "${taskId}" not found${hint}
${availableLine}`;
}

// runtime/typescript/src/main.ts
var processIo = {
  stdin: process.stdin,
  stdout: process.stdout,
  stderr: process.stderr
};
var RUNTIME_CONTENT_JSON_MAX_BYTES = 256 * 1024;
var CHANNEL_NAME_MAX_BYTES = 256;
var ADAPTER_MAX_FRAME_BYTES = 256 * 1024 * 1024;
var LOG_ENTRY_MAX_BYTES = 64 * 1024;
var WAIT_METADATA_JSON_MAX_BYTES = 64 * 1024;
var WAIT_TAGS_MAX_COUNT = 32;
var WAIT_TAG_MAX_BYTES = 128;
var TRUNCATED_LOG_ENTRY_MARKER = `
...[truncated logger entry]`;
async function runAdapterCli(argv = process.argv.slice(2), io = processIo) {
  try {
    const args = parseArgs(argv);
    switch (args.command) {
      case "parse":
        await parseCommand(args, io);
        break;
      case "run":
        await runCommand(args, io);
        break;
      case "inspect-config":
        await inspectConfigCommand(args, io);
        break;
      default:
        throw new Error(`unknown adapter command: ${args.command}`);
    }
    return 0;
  } catch (error) {
    writeSerializedError(io.stderr, serializeError(error));
    return 1;
  }
}
async function parseCommand(args, io) {
  const cwd = resolve2(requireArg(args, "cwd"));
  const output = args.options["output"] ?? "json";
  const registry2 = await loadTaskRegistry(cwd);
  switch (output) {
    case "json": {
      io.stdout.write(`${JSON.stringify(serializeRegistry(registry2))}
`);
      break;
    }
    case "binary": {
      const taskId = requireArg(args, "task");
      const bytes = toBinary(BundleSchema, lookupRegisteredTask(registry2, taskId).bundle);
      io.stdout.write(bytes);
      break;
    }
    default:
      throw new Error(`unsupported --output value: ${output}`);
  }
}
async function inspectConfigCommand(args, io) {
  const cwd = resolve2(requireArg(args, "cwd"));
  const config = await loadConfig(cwd);
  io.stdout.write(`${JSON.stringify({
    project: config.project,
    dirs: config.dirs,
    ignorePatterns: config.ignorePatterns ?? null
  })}
`);
}
function classifyAdapterParseErrorKind(error) {
  if (error instanceof MissingConfigError) {
    return "missing_config";
  }
  if (error instanceof TaskNotFoundError) {
    return "task_not_found";
  }
  if (error instanceof DuplicateTaskIdError) {
    return "duplicate_task_id";
  }
  return "bad_request";
}
function serializeError(error) {
  if (error instanceof Error) {
    return {
      level: "error",
      kind: classifyAdapterParseErrorKind(error),
      message: error.message,
      stack: error.stack ?? null
    };
  }
  return { level: "error", kind: "bad_request", message: String(error) };
}
function writeSerializedError(sink, error) {
  sink.write(`${JSON.stringify(error)}
`);
}
async function runCommand(args, io) {
  const cwd = resolve2(requireArg(args, "cwd"));
  process.chdir(cwd);
  const taskCwd = resolve2(args.options["task-cwd"] ?? cwd);
  const taskId = requireArg(args, "task");
  const runId2 = requireArg(args, "run-id");
  const control = await AdapterControlWriter.open(io.control);
  const responses = new AdapterResponseReader(io.stdin);
  let leaveRuntime;
  try {
    const registry2 = await loadTaskRegistry(taskCwd);
    const registeredTask = lookupRegisteredTask(registry2, taskId);
    const task3 = registeredTask.task;
    const controller = new AbortController;
    const rawPayload = parsePayload(args.options["payload-json"]);
    const taskContext = parseTaskContext(requireArg(args, "task-context-json"), runId2, taskId);
    const mintCorrelationId = createCorrelationIdMint();
    const waitGate = new WaitGate;
    leaveRuntime = enterRunRuntime({
      createWaitpointToken: (opts) => createWaitpointToken(responses, control, opts),
      waitpoint: (opts) => waitInput(responses, control, mintCorrelationId, waitGate, opts),
      waitAll: (operands) => waitAll(responses, control, mintCorrelationId, waitGate, operands),
      channelOutputAppend: (channel2, payload2, opts) => writeChannelOutput(control, channel2, payload2, opts),
      waitFor: (input) => waitFor(responses, control, mintCorrelationId, waitGate, input),
      waitUntil: (input) => waitUntil(responses, control, mintCorrelationId, waitGate, input),
      metadataSet: (key, value) => writeMetadataSet(control, key, value),
      metadataPatch: (value) => writeMetadataPatch(control, value),
      metadataIncrement: (key, amount = 1) => writeMetadataIncrement(control, key, amount),
      log: (level, values) => writeLog(control, level, values)
    });
    const ctx = {
      signal: controller.signal,
      run: taskContext.run,
      task: taskContext.task,
      workspace: taskContext.workspace,
      session: createTaskSessionContext(taskContext.session.id, responses, control, mintCorrelationId, waitGate)
    };
    let result;
    const payload = task3.payload === undefined ? undefined : await parseTaskPayload(task3, rawPayload);
    try {
      if (task3.payload === undefined) {
        result = await task3.run(ctx);
      } else {
        result = await task3.run(payload, ctx);
      }
    } catch (error) {
      const serialized = serializeError(error);
      writeSerializedError(io.stderr, serialized);
      await drainProcessOutputStreams();
      writeTaskResult(control, { exitCode: 1, errorMessage: serialized.message });
      return;
    } finally {
      leaveRuntime();
    }
    const outputJson = stringifyTaskOutput(result);
    await drainProcessOutputStreams();
    writeTaskResult(control, outputJson === undefined ? { exitCode: 0 } : { exitCode: 0, outputJson });
  } catch (error) {
    const serialized = serializeError(error);
    writeSerializedError(io.stderr, serialized);
    await drainProcessOutputStreams();
    writeTaskResult(control, { exitCode: 1, errorMessage: serialized.message });
  } finally {
    leaveRuntime?.();
    responses.close();
    await control.close();
  }
}
function createCorrelationIdMint() {
  let nextCorrelationId = 0;
  return () => {
    nextCorrelationId += 1;
    return String(nextCorrelationId);
  };
}
function parsePayload(value) {
  if (value === undefined || value === "") {
    return {};
  }
  return JSON.parse(value);
}
function parseTaskContext(json, runId2, taskId) {
  const parsed = JSON.parse(json);
  if (parsed === null || typeof parsed !== "object") {
    throw new Error("task context json must be an object");
  }
  const record = parsed;
  const contextRunId = readStringField(record, "run", "id", "task context run.id");
  const contextTaskId = readStringField(record, "task", "id", "task context task.id");
  if (contextRunId !== runId2) {
    throw new Error(`task context run.id ${JSON.stringify(contextRunId)} does not match --run-id ${JSON.stringify(runId2)}`);
  }
  if (contextTaskId !== taskId) {
    throw new Error(`task context task.id ${JSON.stringify(contextTaskId)} does not match --task ${JSON.stringify(taskId)}`);
  }
  const workspace = parseTaskWorkspace(record["workspace"]);
  const session = parseTaskSession(record["session"]);
  const runRecord = record["run"];
  const run = {
    id: contextRunId,
    ...optionalProperty("attemptId", readOptionalStringField(runRecord, "attemptId", "task context run.attemptId")),
    ...optionalProperty("attemptNumber", readOptionalPositiveIntegerField(runRecord, "attemptNumber", "task context run.attemptNumber")),
    ...optionalProperty("runLeaseId", readOptionalStringField(runRecord, "runLeaseId", "task context run.runLeaseId")),
    ...optionalProperty("snapshotVersion", readOptionalPositiveIntegerField(runRecord, "snapshotVersion", "task context run.snapshotVersion"))
  };
  return {
    run: Object.freeze(run),
    task: Object.freeze({ id: contextTaskId }),
    workspace: Object.freeze(workspace),
    session: Object.freeze(session)
  };
}
function readStringField(value, objectKey, fieldKey, label) {
  const objectValue = value[objectKey];
  if (objectValue === null || typeof objectValue !== "object") {
    throw new Error(`${label} is required`);
  }
  const fieldValue = objectValue[fieldKey];
  if (typeof fieldValue !== "string" || fieldValue.trim() === "") {
    throw new Error(`${label} is required`);
  }
  return fieldValue;
}
function readOptionalStringField(value, fieldKey, label) {
  const fieldValue = value[fieldKey];
  if (fieldValue === undefined) {
    return;
  }
  if (typeof fieldValue !== "string" || fieldValue.trim() === "") {
    throw new Error(`${label} must be a non-empty string`);
  }
  return fieldValue;
}
function readOptionalPositiveIntegerField(value, fieldKey, label) {
  const fieldValue = value[fieldKey];
  if (fieldValue === undefined) {
    return;
  }
  if (typeof fieldValue !== "number" || !Number.isInteger(fieldValue) || fieldValue <= 0) {
    throw new Error(`${label} must be a positive integer`);
  }
  return fieldValue;
}
function optionalProperty(key, value) {
  return value === undefined ? {} : { [key]: value };
}
function parseTaskWorkspace(value) {
  if (value === null || typeof value !== "object") {
    throw new Error("task context workspace is required");
  }
  const record = value;
  return {
    path: readRequiredString(record, "path", "task context workspace.path"),
    projectPath: readRequiredString(record, "projectPath", "task context workspace.projectPath")
  };
}
function parseTaskSession(value) {
  if (value === null || typeof value !== "object") {
    throw new Error("task context session is required");
  }
  const record = value;
  return {
    id: readRequiredString(record, "id", "task context session.id")
  };
}
function readRequiredString(record, key, label) {
  const value = record[key];
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`${label} is required`);
  }
  return value;
}
function serializeRegistry(registry2) {
  return {
    tasks: Object.fromEntries([...registry2.entries()].sort(([leftId], [rightId]) => compareAscii2(leftId, rightId)).map(([taskId, task3]) => [
      taskId,
      {
        originFile: task3.originFile,
        modulePath: task3.modulePath,
        exportName: task3.exportName,
        bundle: toJson(BundleSchema, task3.bundle)
      }
    ]))
  };
}
function compareAscii2(left, right) {
  if (left < right)
    return -1;
  if (left > right)
    return 1;
  return 0;
}

class AdapterResponseReader {
  #iterator;
  #buffer = Buffer.alloc(0);
  #closed = false;
  constructor(stdin) {
    this.#iterator = stdin[Symbol.asyncIterator]();
  }
  close() {
    this.#closed = true;
  }
  async readDecision() {
    if (this.#closed) {
      throw new Error("adapter response stream closed");
    }
    const body = await this.#readFrameBody();
    return fromBinary(exports_run_pb.ResumeDecisionSchema, body);
  }
  async readWaitpointTokenCreateResult() {
    if (this.#closed) {
      throw new Error("adapter response stream closed");
    }
    const body = await this.#readFrameBody();
    return fromBinary(exports_run_pb.WaitpointTokenCreateResultSchema, body);
  }
  async#readFrameBody() {
    await this.#fill(4);
    const len = this.#buffer.readUInt32BE(0);
    this.#buffer = this.#buffer.subarray(4);
    if (len > ADAPTER_MAX_FRAME_BYTES) {
      throw new Error(`adapter response frame length ${len} exceeds max ${ADAPTER_MAX_FRAME_BYTES}`);
    }
    await this.#fill(len);
    const body = this.#buffer.subarray(0, len);
    this.#buffer = this.#buffer.subarray(len);
    return body;
  }
  async#fill(bytes) {
    while (this.#buffer.length < bytes) {
      const next = await this.#iterator.next();
      if (next.done === true) {
        this.#closed = true;
        throw new Error("adapter response stream closed");
      }
      this.#buffer = Buffer.concat([this.#buffer, Buffer.from(next.value)]);
    }
  }
}

class WaitGate {
  #inFlight = false;
  async run(fn) {
    if (this.#inFlight) {
      throw new ConcurrentWaitError("concurrent blocking run I/O calls are not supported");
    }
    this.#inFlight = true;
    try {
      return await fn();
    } finally {
      this.#inFlight = false;
    }
  }
}
function createTaskSessionContext(id, responses, control, mintCorrelationId, waitGate) {
  return Object.freeze({
    id,
    input(target) {
      const channel2 = channelTargetName(target);
      const schema = typeof target === "string" ? undefined : target.schema;
      return Object.freeze({
        id: channel2,
        wait: (waitOpts = {}) => {
          const operand = {
            type: "channel",
            channel: channel2,
            ...schema === undefined ? {} : { schema },
            options: waitOpts
          };
          return waitpointHandle2(operand, () => waitChannelInput(responses, control, mintCorrelationId, waitGate, channel2, schema, waitOpts));
        }
      });
    },
    output(target) {
      const channel2 = channelTargetName(target);
      const schema = typeof target === "string" ? undefined : target.schema;
      return Object.freeze({
        id: channel2,
        append: async (payload, appendOpts) => {
          const parsed = schema === undefined ? payload : await parsePayloadWithSchema(schema, payload, `channel ${JSON.stringify(channel2)} payload`);
          return writeChannelOutput(control, channel2, parsed, appendOpts);
        },
        pipe: async (source, appendOpts) => {
          for await (const item of source) {
            const parsed = schema === undefined ? item : await parsePayloadWithSchema(schema, item, `channel ${JSON.stringify(channel2)} payload`);
            await writeChannelOutput(control, channel2, parsed, appendOpts);
          }
        }
      });
    }
  });
}
function channelTargetName(target) {
  return validateChannelName(typeof target === "string" ? target : target.id);
}

class AdapterControlWriter {
  static async open(sink) {
    if (sink !== undefined) {
      return new AdapterControlWriter({ sink });
    }
    const fd = process.env["HELMR_CONTROL_FD"]?.trim();
    delete process.env["HELMR_CONTROL_FD"];
    if (fd) {
      const controlFd = Number.parseInt(fd, 10);
      if (!Number.isSafeInteger(controlFd) || controlFd < 3) {
        throw new Error(`invalid HELMR_CONTROL_FD: ${fd}`);
      }
      return new AdapterControlWriter({ stream: createWriteStream("/dev/null", { fd: controlFd }) });
    }
    const socketPath = process.env["HELMR_CONTROL_SOCKET"]?.trim();
    delete process.env["HELMR_CONTROL_SOCKET"];
    if (!socketPath) {
      throw new Error("HELMR_CONTROL_SOCKET is required");
    }
    return new AdapterControlWriter({ socket: await connectControlSocket(socketPath) });
  }
  #target;
  constructor(target) {
    this.#target = target;
  }
  write(event) {
    const body = Buffer.from(toBinary(exports_run_pb.RunEventSchema, event));
    const header = Buffer.alloc(4);
    header.writeUInt32BE(body.length, 0);
    const frame = Buffer.concat([header, body]);
    if ("socket" in this.#target) {
      this.#target.socket.write(frame);
    } else if ("stream" in this.#target) {
      this.#target.stream.write(frame);
    } else {
      this.#target.sink.write(frame);
    }
  }
  close() {
    const target = this.#target;
    if ("socket" in target) {
      return new Promise((resolveClose) => {
        target.socket.end(resolveClose);
      });
    }
    if ("stream" in target) {
      return new Promise((resolveClose) => {
        target.stream.end(resolveClose);
      });
    }
    return Promise.resolve();
  }
}
function connectControlSocket(socketPath) {
  return new Promise((resolveConnection, rejectConnection) => {
    const socket = createConnection(socketPath);
    const onError = (error) => {
      socket.destroy();
      rejectConnection(error);
    };
    socket.once("error", onError);
    socket.once("connect", () => {
      socket.off("error", onError);
      resolveConnection(socket);
    });
  });
}
async function createWaitpointToken(responses, control, opts) {
  const metadata2 = opts.metadata === undefined ? undefined : normalizeWaitMetadata(opts.metadata);
  const metadataJson = metadata2 === undefined ? undefined : JSON.stringify(metadata2);
  const tags = opts.tags === undefined ? undefined : normalizeWaitTags(opts.tags);
  const timeoutInSeconds = opts.timeoutInSeconds === undefined ? undefined : positiveDelaySeconds(opts.timeoutInSeconds);
  control.write(create(exports_run_pb.RunEventSchema, {
    event: {
      case: "waitpointTokenCreateRequested",
      value: create(exports_run_pb.WaitpointTokenCreateRequestedSchema, {
        ...opts.timeoutAt === undefined ? {} : { timeoutAt: opts.timeoutAt },
        ...timeoutInSeconds === undefined ? {} : { timeoutInSeconds },
        ...tags === undefined ? {} : { tags },
        ...metadataJson === undefined ? {} : { metadataJson }
      })
    }
  }));
  const result = await responses.readWaitpointTokenCreateResult();
  if (result.errorMessage !== undefined && result.errorMessage.trim() !== "") {
    throw new Error(result.errorMessage);
  }
  if (result.id.trim() === "") {
    throw new Error("waitpoint token create response id is required");
  }
  if (result.callbackUrl.trim() === "") {
    throw new Error("waitpoint token create response callback_url is required");
  }
  const resultMetadata = result.metadataJson === undefined || result.metadataJson.trim() === "" ? undefined : parseWaitpointTokenMetadata(result.metadataJson);
  const status = waitpointTokenStatus(result.status);
  return {
    id: result.id,
    callbackUrl: result.callbackUrl,
    ...result.publicAccessToken === undefined ? {} : { publicAccessToken: result.publicAccessToken },
    timeoutAt: result.timeoutAt ?? null,
    ...status === undefined ? {} : { status },
    ...result.tags.length === 0 ? {} : { tags: result.tags },
    ...resultMetadata === undefined ? {} : { metadata: resultMetadata }
  };
}
function waitpointTokenStatus(value) {
  switch (value) {
    case "waiting":
    case "completed":
    case "timed_out":
    case "cancelled":
      return value;
    case undefined:
    case "":
      return;
    default:
      throw new Error(`waitpoint token create response status is invalid: ${value}`);
  }
}
async function waitFor(responses, control, mintCorrelationId, waitGate, input) {
  const seconds = waitDurationSeconds(input);
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest("timer", normalizeWaitDurationInput(input), { timeout: seconds }));
  if (!(decision.kind === "timed_out" || decision.kind === "completed")) {
    throw new Error(`unexpected wait.for resume decision kind ${JSON.stringify(decision.kind)}`);
  }
  maybeWriteResumeConsumed(control, decision);
}
async function waitUntil(responses, control, mintCorrelationId, waitGate, input) {
  const until = waitUntilInputDate(input);
  const seconds = Math.max(1, Math.ceil((until.getTime() - Date.now()) / 1000));
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest("timer", normalizeWaitUntilInput(input), { timeout: seconds }));
  if (!(decision.kind === "timed_out" || decision.kind === "completed")) {
    throw new Error(`unexpected wait.until resume decision kind ${JSON.stringify(decision.kind)}`);
  }
  maybeWriteResumeConsumed(control, decision);
}
async function waitInput(responses, control, mintCorrelationId, waitGate, opts) {
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest(opts.kind ?? "token", waitpointData(opts), {
    ...opts.timeout === undefined ? {} : { timeout: waitDurationSeconds(opts.timeout) },
    ...opts.metadata === undefined ? {} : { metadata: opts.metadata },
    ...opts.tags === undefined ? {} : { tags: opts.tags }
  }));
  if (decision.kind === "timed_out") {
    const seconds = opts.timeout === undefined ? undefined : waitDurationSeconds(opts.timeout);
    maybeWriteResumeConsumed(control, decision);
    return new WaitpointResultImpl(false, undefined, new WaitTimeoutError(`waitpoint timed out${formatTimeoutSuffix(seconds)}`, seconds));
  }
  if (decision.kind !== "completed") {
    throw new Error(`unexpected waitpoint resume decision kind ${JSON.stringify(decision.kind)}`);
  }
  const data = parseResumeData(decision.dataJson);
  if (opts.schema === undefined) {
    maybeWriteResumeConsumed(control, decision);
    return new WaitpointResultImpl(true, data);
  }
  const payload = await parsePayloadWithSchema(opts.schema, data, "waitpoint data");
  maybeWriteResumeConsumed(control, decision);
  return new WaitpointResultImpl(true, payload);
}
async function waitChannelInput(responses, control, mintCorrelationId, waitGate, channel2, schema, opts = {}) {
  const correlationId = normalizeOptionalCorrelationId(opts.correlationId);
  const decision = await waitGenericDecision(responses, control, mintCorrelationId, waitGate, waitRequest("channel", {
    channel: channel2,
    ...correlationId === undefined ? {} : { correlation_id: correlationId }
  }, {
    ...opts.timeout === undefined ? {} : { timeout: waitDurationSeconds(opts.timeout) },
    ...opts.metadata === undefined ? {} : { metadata: opts.metadata },
    ...opts.tags === undefined ? {} : { tags: opts.tags }
  }));
  if (decision.kind === "timed_out") {
    const seconds = opts.timeout === undefined ? undefined : waitDurationSeconds(opts.timeout);
    maybeWriteResumeConsumed(control, decision);
    return new WaitpointResultImpl(false, undefined, new WaitTimeoutError(`channel ${JSON.stringify(channel2)} wait timed out${formatTimeoutSuffix(seconds)}`, seconds));
  }
  if (decision.kind !== "completed") {
    throw new Error(`unexpected channel wait resume decision kind ${JSON.stringify(decision.kind)}`);
  }
  const envelope = channelWaitpointEnvelope(parseResumeData(decision.dataJson), channel2);
  const data = schema === undefined ? envelope.data : await parsePayloadWithSchema(schema, envelope.data, `channel ${JSON.stringify(channel2)} data`);
  maybeWriteResumeConsumed(control, decision);
  return completedWaitpointResult(data);
}
async function waitAll(responses, control, mintCorrelationId, waitGate, operands) {
  if (operands.length === 0) {
    throw new Error("wait.all requires at least one operand");
  }
  const requests = operands.map(runtimeWaitOperandRequest);
  const decision = await waitGate.run(async () => {
    const correlationId = mintCorrelationId();
    const aggregateCount = requests.length;
    requests.forEach((request, ordinal) => {
      control.write(waitpointRequestedEvent({ ...request, correlationId, ordinal, aggregateCount }));
    });
    return responses.readDecision();
  });
  if (decision.kind === "timed_out") {
    if (operands.length === 1 && (operands[0]?.type === "for" || operands[0]?.type === "until")) {
      maybeWriteResumeConsumed(control, decision);
      return [undefined];
    }
    maybeWriteResumeConsumed(control, decision);
    throw new WaitTimeoutError("wait.all timed out");
  }
  if (operands.length === 1 && decision.kind === "completed") {
    const operand = operands[0];
    if (operand === undefined) {
      throw new Error("wait.all operand is missing");
    }
    const result = await decodeWaitAllOperand(operand, parseResumeData(decision.dataJson));
    maybeWriteResumeConsumed(control, decision);
    return [result];
  }
  if (decision.kind !== "waitpoints") {
    throw new Error(`unexpected wait.all resume decision kind ${JSON.stringify(decision.kind)}`);
  }
  const envelope = waitAllEnvelope(parseResumeData(decision.dataJson), operands.length);
  const results = [];
  for (let index = 0;index < operands.length; index += 1) {
    const operand = operands[index];
    if (operand === undefined) {
      throw new Error(`wait.all operand at index ${index} is missing`);
    }
    results.push(await decodeWaitAllOperand(operand, envelope[index]));
  }
  maybeWriteResumeConsumed(control, decision);
  return results;
}
function runtimeWaitOperandRequest(operand) {
  switch (operand.type) {
    case "for": {
      const seconds = waitDurationSeconds(operand.input);
      return waitRequest("timer", normalizeWaitDurationInput(operand.input), { timeout: seconds });
    }
    case "until": {
      const until = waitUntilInputDate(operand.input);
      const seconds = Math.max(1, Math.ceil((until.getTime() - Date.now()) / 1000));
      return waitRequest("timer", normalizeWaitUntilInput(operand.input), { timeout: seconds });
    }
    case "waitpoint":
      return waitRequest(operand.options.kind ?? "token", waitpointData(operand.options), {
        ...operand.options.timeout === undefined ? {} : { timeout: waitDurationSeconds(operand.options.timeout) },
        ...operand.options.metadata === undefined ? {} : { metadata: operand.options.metadata },
        ...operand.options.tags === undefined ? {} : { tags: operand.options.tags }
      });
    case "channel": {
      const correlationId = normalizeOptionalCorrelationId(operand.options?.correlationId);
      return waitRequest("channel", {
        channel: operand.channel,
        ...correlationId === undefined ? {} : { correlation_id: correlationId }
      }, {
        ...operand.options?.timeout === undefined ? {} : { timeout: waitDurationSeconds(operand.options.timeout) },
        ...operand.options?.metadata === undefined ? {} : { metadata: operand.options.metadata },
        ...operand.options?.tags === undefined ? {} : { tags: operand.options.tags }
      });
    }
  }
}
async function decodeWaitAllOperand(operand, value) {
  switch (operand.type) {
    case "for":
    case "until":
      return;
    case "waitpoint":
      if (operand.options.schema === undefined) {
        return value;
      }
      return await parsePayloadWithSchema(operand.options.schema, value, "wait.all waitpoint data");
    case "channel": {
      const envelope = channelWaitpointEnvelope(value, operand.channel);
      if (operand.schema === undefined) {
        return envelope.data;
      }
      return await parsePayloadWithSchema(operand.schema, envelope.data, `channel ${JSON.stringify(operand.channel)} data`);
    }
  }
}
function waitAllEnvelope(value, expectedLength) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("wait.all data must be an object");
  }
  const waitpoints = value.waitpoints;
  if (!Array.isArray(waitpoints)) {
    throw new Error("wait.all data.waitpoints must be an array");
  }
  if (waitpoints.length !== expectedLength) {
    throw new Error(`wait.all data.waitpoints length ${waitpoints.length} did not match operand count ${expectedLength}`);
  }
  return waitpoints;
}
function normalizeOptionalCorrelationId(value) {
  if (value === undefined) {
    return;
  }
  const normalized = value.trim();
  return normalized === "" ? undefined : normalized;
}
function waitpointHandle2(operand, factory) {
  let promise;
  const getPromise = () => {
    promise ??= factory();
    return promise;
  };
  const handle = {
    then(onfulfilled, onrejected) {
      return getPromise().then(onfulfilled, onrejected);
    },
    unwrap: async () => (await getPromise()).unwrap()
  };
  Object.defineProperty(handle, runtimeWaitOperand, { value: operand });
  return handle;
}
function completedWaitpointResult(data) {
  return new WaitpointResultImpl(true, data);
}
function channelWaitpointEnvelope(value, expectedChannel) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("channel waitpoint data must be an object");
  }
  const record = value;
  if (record.channel !== expectedChannel) {
    throw new Error(`channel waitpoint channel mismatch: expected ${JSON.stringify(expectedChannel)}`);
  }
  if (typeof record.sequence !== "number" || !Number.isInteger(record.sequence) || record.sequence < 0) {
    throw new Error("channel waitpoint sequence must be a non-negative integer");
  }
  return {
    channel: record.channel,
    sequence: record.sequence,
    data: record.data
  };
}
async function waitGenericDecision(responses, control, mintCorrelationId, waitGate, request) {
  return waitGate.run(async () => {
    const correlationId = mintCorrelationId();
    control.write(waitpointRequestedEvent({ ...request, correlationId }));
    return responses.readDecision();
  });
}
function waitRequest(kind, data, opts) {
  const normalizedKind = normalizeWaitKind(kind);
  const timeout = opts?.timeout;
  if (timeout !== undefined) {
    validateWaitTimeout(timeout);
  }
  const tags = normalizeWaitTags(opts?.tags);
  return {
    kind: normalizedKind,
    paramsJson: JSON.stringify(data),
    ...opts?.metadata === undefined ? {} : { metadataJson: JSON.stringify(normalizeWaitMetadata(opts.metadata)) },
    ...tags === undefined ? {} : { tags },
    ...timeout === undefined ? {} : { timeout }
  };
}
function waitpointRequestedEvent(request) {
  const value = waitpointRequestedValue(request);
  return create(exports_run_pb.RunEventSchema, {
    event: {
      case: "waitpointRequested",
      value
    }
  });
}
function waitpointRequestedValue(request) {
  return create(exports_run_pb.WaitpointRequestedSchema, {
    correlationId: request.correlationId,
    kind: request.kind,
    paramsJson: request.paramsJson,
    ...request.metadataJson === undefined ? {} : { metadataJson: request.metadataJson },
    ...request.tags === undefined ? {} : { tags: request.tags },
    ...request.timeout === undefined ? {} : { timeout: request.timeout },
    ...request.ordinal === undefined ? {} : { ordinal: request.ordinal },
    ...request.aggregateCount === undefined ? {} : { aggregateCount: request.aggregateCount }
  });
}
function maybeWriteResumeConsumed(control, decision) {
  if (!decision.requireConsumedAck) {
    return;
  }
  if (decision.waitpointId.trim() === "") {
    throw new Error("resume decision waitpoint_id is required");
  }
  control.write(create(exports_run_pb.RunEventSchema, {
    event: {
      case: "resumeConsumed",
      value: {
        waitpointId: decision.waitpointId
      }
    }
  }));
}
function formatTimeoutSuffix(timeout) {
  return timeout === undefined ? "" : ` after ${timeout}`;
}
function waitpointData(opts) {
  return opts.params === undefined ? {} : normalizeWaitJson(opts.params, "waitpoint params");
}
function normalizeOptionalIdentifier(value, label) {
  if (value === undefined)
    return;
  const normalized = value.trim();
  if (normalized === "") {
    throw new Error(`${label} must be non-empty`);
  }
  return normalized;
}
function normalizeWaitDurationInput(input) {
  if (typeof input === "string") {
    return { duration: input };
  }
  if (typeof input === "number") {
    return { seconds: input };
  }
  return normalizeWaitJson(input, "wait duration input");
}
function waitDurationSeconds(input) {
  if (typeof input === "number") {
    return positiveDelaySeconds(input);
  }
  if (typeof input === "string") {
    return parseDurationSeconds(input, "wait duration");
  }
  const seconds = input.seconds;
  if (seconds !== undefined) {
    return positiveDelaySeconds(seconds);
  }
  const milliseconds = input.milliseconds;
  if (milliseconds !== undefined) {
    return positiveDelaySeconds(milliseconds / 1000);
  }
  const minutes = input.minutes;
  if (minutes !== undefined) {
    return positiveDelaySeconds(minutes * 60);
  }
  const hours = input.hours;
  if (hours !== undefined) {
    return positiveDelaySeconds(hours * 3600);
  }
  const duration = input.duration;
  if (duration !== undefined) {
    return parseDurationSeconds(duration, "wait duration");
  }
  throw new Error("wait duration requires seconds, milliseconds, minutes, hours, or duration");
}
function parseDurationSeconds(value, label) {
  const match = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(value.trim());
  if (match === null) {
    throw new Error(`${label} must use ms, s, m, or h units`);
  }
  const amount = Number(match[1]);
  const unit = match[2];
  const multiplier = unit === "ms" ? 0.001 : unit === "s" ? 1 : unit === "m" ? 60 : 3600;
  return positiveDelaySeconds(amount * multiplier);
}
function normalizeWaitUntilInput(input) {
  if (typeof input === "string") {
    return { date: input };
  }
  if (input instanceof Date) {
    return { date: input.toISOString() };
  }
  return normalizeWaitJson(input, "wait.until input");
}
function waitUntilInputDate(input) {
  const value = typeof input === "object" && !(input instanceof Date) ? input.date : input;
  if (value === undefined) {
    throw new Error("wait.until requires a date");
  }
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) {
    throw new Error("wait.until date must be a valid timestamp");
  }
  return date;
}
function positiveDelaySeconds(value) {
  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`invalid wait timeout: ${value}`);
  }
  const seconds = Math.ceil(value);
  validateWaitTimeout(seconds);
  return seconds;
}
function normalizeWaitJson(value, label) {
  if (value === null || typeof value === "boolean" || typeof value === "string") {
    return value;
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new Error(`${label} number must be finite`);
    }
    return value;
  }
  if (value instanceof Date) {
    return value.toISOString();
  }
  if (Array.isArray(value)) {
    return value.map((item) => normalizeWaitJson(item, label));
  }
  if (typeof value === "object" && value !== undefined) {
    const entries = [];
    for (const [key, item] of Object.entries(value)) {
      if (item === undefined) {
        continue;
      }
      entries.push([key, normalizeWaitJson(item, label)]);
    }
    return Object.fromEntries(entries);
  }
  throw new Error(`${label} must be JSON-serializable`);
}
function normalizeWaitKind(value) {
  const kind = value.trim();
  if (kind === "") {
    throw new Error("wait kind must be non-empty");
  }
  return kind;
}
function normalizeWaitTags(value) {
  if (value === undefined)
    return;
  const tags = typeof value === "string" ? [value] : [...value];
  if (tags.length > WAIT_TAGS_MAX_COUNT) {
    throw new Error(`wait tags has ${tags.length} entries, exceeds max ${WAIT_TAGS_MAX_COUNT}`);
  }
  return tags.map((tag) => {
    const normalized = normalizeRequiredIdentifier(tag, "wait tag");
    validateUtf8ByteLength("wait tag", normalized, WAIT_TAG_MAX_BYTES);
    return normalized;
  });
}
function parseWaitpointTokenMetadata(value) {
  const parsed = JSON.parse(value);
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error("waitpoint token create response metadata_json must be a JSON object");
  }
  return parsed;
}
function normalizeWaitMetadata(value) {
  const normalized = normalizeWaitJson(value, "wait metadata");
  if (normalized === null || typeof normalized !== "object" || Array.isArray(normalized)) {
    throw new Error("wait metadata must be a JSON object");
  }
  const metadataJson = JSON.stringify(normalized);
  validateUtf8ByteLength("wait metadata_json", metadataJson, WAIT_METADATA_JSON_MAX_BYTES);
  return normalized;
}
function normalizeRequiredIdentifier(value, label) {
  const normalized = normalizeOptionalIdentifier(value, label);
  if (normalized === undefined) {
    throw new Error(`${label} is required`);
  }
  return normalized;
}
function parseResumeData(json) {
  if (json === "") {
    throw new Error("waitpoint data is required");
  }
  try {
    return JSON.parse(json);
  } catch (error) {
    if (error instanceof Error) {
      throw new Error(`waitpoint data must be valid JSON: ${error.message}`);
    }
    throw new Error("waitpoint data must be valid JSON");
  }
}
async function writeChannelOutput(control, channelInput, payload, opts = {}) {
  const channel2 = validateChannelName(channelInput);
  validateUtf8ByteLength("channel", channel2, CHANNEL_NAME_MAX_BYTES);
  const contentType = opts.contentType?.trim();
  if (contentType !== undefined && contentType === "") {
    throw new Error("channel output contentType must be non-empty");
  }
  const payloadJson = JSON.stringify(payload === undefined ? null : payload);
  validateUtf8ByteLength("channel output payload_json", payloadJson, RUNTIME_CONTENT_JSON_MAX_BYTES);
  const objectRefJson = opts.objectRef === undefined ? undefined : JSON.stringify(normalizeWaitJson(opts.objectRef, "channel output objectRef"));
  if (objectRefJson !== undefined) {
    validateUtf8ByteLength("channel output object_ref_json", objectRefJson, RUNTIME_CONTENT_JSON_MAX_BYTES);
  }
  control.write(create(exports_run_pb.RunEventSchema, {
    event: {
      case: "channelOutputAppended",
      value: {
        channel: channel2,
        payloadJson,
        ...contentType === undefined ? {} : { contentType },
        ...objectRefJson === undefined ? {} : { objectRefJson }
      }
    }
  }));
}
async function writeMetadataSet(control, key, value) {
  const normalizedKey = normalizeRequiredIdentifier(key, "metadata key");
  const valueJson = JSON.stringify(normalizeWaitJson(value, "metadata value"));
  validateUtf8ByteLength("metadata value_json", valueJson, RUNTIME_CONTENT_JSON_MAX_BYTES);
  control.write(create(exports_run_pb.RunEventSchema, {
    event: {
      case: "metadataUpdated",
      value: {
        operation: "set",
        key: normalizedKey,
        valueJson
      }
    }
  }));
}
async function writeMetadataPatch(control, patch) {
  const payloadJson = JSON.stringify(normalizeWaitJson(patch, "metadata patch"));
  validateUtf8ByteLength("metadata patch_json", payloadJson, RUNTIME_CONTENT_JSON_MAX_BYTES);
  control.write(create(exports_run_pb.RunEventSchema, {
    event: {
      case: "metadataUpdated",
      value: {
        operation: "patch",
        patchJson: payloadJson
      }
    }
  }));
}
async function writeMetadataIncrement(control, key, amount) {
  const normalizedKey = normalizeRequiredIdentifier(key, "metadata key");
  if (!Number.isFinite(amount)) {
    throw new Error("metadata increment amount must be finite");
  }
  control.write(create(exports_run_pb.RunEventSchema, {
    event: {
      case: "metadataUpdated",
      value: {
        operation: "increment",
        key: normalizedKey,
        amount
      }
    }
  }));
}
function validateWaitTimeout(value) {
  if (!Number.isInteger(value) || !Number.isFinite(value) || value < 1) {
    throw new Error(`invalid wait timeout: ${value}`);
  }
}
function validateUtf8ByteLength(field, value, maxBytes) {
  const bytes = Buffer.byteLength(value, "utf8");
  if (bytes > maxBytes) {
    throw new Error(`${field} is ${bytes} bytes, exceeds max ${maxBytes}`);
  }
}
function writeLog(control, level, values) {
  const entry = formatLogEntry(level, formatMessage(values));
  control.write(create(exports_run_pb.RunEventSchema, {
    event: {
      case: "logEntry",
      value: entry
    }
  }));
}
function stringifyTaskOutput(result) {
  if (result === undefined)
    return;
  return JSON.stringify(result);
}
function writeTaskResult(control, result) {
  control.write(create(exports_run_pb.RunEventSchema, {
    event: {
      case: "taskResult",
      value: create(exports_run_pb.TaskResultSchema, {
        exitCode: result.exitCode,
        ...result.errorMessage === undefined ? {} : { errorMessage: result.errorMessage },
        ...result.errorMessage === undefined ? {} : {
          error: {
            type: "Error",
            code: "task_error",
            message: result.errorMessage,
            retryable: false,
            detailsJson: "{}"
          }
        },
        ...result.outputJson === undefined ? {} : { outputJson: result.outputJson }
      })
    }
  }));
}
function formatLogEntry(level, message) {
  const initial = JSON.stringify({ level, message });
  if (Buffer.byteLength(initial, "utf8") <= LOG_ENTRY_MAX_BYTES) {
    return initial;
  }
  const markerOnly = JSON.stringify({ level, message: TRUNCATED_LOG_ENTRY_MARKER });
  let prefixBudget = Math.max(0, LOG_ENTRY_MAX_BYTES - Buffer.byteLength(markerOnly, "utf8"));
  let truncated = `${truncateUtf8Bytes(message, prefixBudget)}${TRUNCATED_LOG_ENTRY_MARKER}`;
  let entry = JSON.stringify({ level, message: truncated });
  while (Buffer.byteLength(entry, "utf8") > LOG_ENTRY_MAX_BYTES && prefixBudget > 0) {
    prefixBudget -= 1;
    truncated = `${truncateUtf8Bytes(message, prefixBudget)}${TRUNCATED_LOG_ENTRY_MARKER}`;
    entry = JSON.stringify({ level, message: truncated });
  }
  return entry;
}
function truncateUtf8Bytes(value, maxBytes) {
  let used = 0;
  let out = "";
  for (const char of value) {
    const bytes = Buffer.byteLength(char, "utf8");
    if (used + bytes > maxBytes)
      break;
    used += bytes;
    out += char;
  }
  return out;
}
function formatMessage(values) {
  return values.map((value) => typeof value === "string" ? value : inspect(value, { breakLength: Infinity })).join(" ");
}
function parseArgs(argv) {
  const [command, ...rest] = argv;
  if (!command) {
    throw new Error("missing command");
  }
  const options = {};
  for (let index = 0;index < rest.length; index += 2) {
    const key = rest[index];
    const value = rest[index + 1];
    if (!key?.startsWith("--") || value === undefined) {
      throw new Error(`invalid arguments near ${key ?? "<eof>"}`);
    }
    options[key.slice(2)] = value;
  }
  return { command, options };
}
function requireArg(args, key) {
  const value = args.options[key];
  if (!value) {
    throw new Error(`missing required argument --${key}`);
  }
  return value;
}
function drainProcessStream(stream) {
  return new Promise((resolveDrain) => {
    stream.write("", () => resolveDrain());
  });
}
function drainProcessOutputStreams() {
  return Promise.all([
    drainProcessStream(process.stdout),
    drainProcessStream(process.stderr)
  ]).then(() => {
    return;
  });
}
if (__require.main == __require.module) {
  runAdapterCli().then(async (status) => {
    process.exitCode = status;
    await Promise.all([drainProcessStream(process.stdout), drainProcessStream(process.stderr)]);
    process.exit(status);
  }).catch(async (error) => {
    process.exitCode = 1;
    process.stderr.write(`${JSON.stringify({ level: "error", kind: "bad_request", message: String(error) })}
`);
    await drainProcessStream(process.stderr);
    process.exit(1);
  });
}
export {
  runAdapterCli
};
