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
// proto/typescript/src/gen/run_pb.ts
var exports_run_pb = {};
__export(exports_run_pb, {
  file_run: () => file_run,
  WorkspaceSecretPlacementSchema: () => WorkspaceSecretPlacementSchema,
  WorkspaceRetrieveRequestedSchema: () => WorkspaceRetrieveRequestedSchema,
  WorkspaceFileStatRequestedSchema: () => WorkspaceFileStatRequestedSchema,
  WorkspaceFileReadRequestedSchema: () => WorkspaceFileReadRequestedSchema,
  WorkspaceFileListRequestedSchema: () => WorkspaceFileListRequestedSchema,
  WorkspaceExecRequestedSchema: () => WorkspaceExecRequestedSchema,
  WorkspaceDeleteRequestedSchema: () => WorkspaceDeleteRequestedSchema,
  WorkspaceCreateRequestedSchema: () => WorkspaceCreateRequestedSchema,
  WorkspaceAddressSchema: () => WorkspaceAddressSchema,
  TokenCreateRequestedSchema: () => TokenCreateRequestedSchema,
  TaskSucceededSchema: () => TaskSucceededSchema,
  TaskStartSchema: () => TaskStartSchema,
  TaskPayloadInvalidSchema: () => TaskPayloadInvalidSchema,
  TaskOutcomeSchema: () => TaskOutcomeSchema,
  TaskFailedSchema: () => TaskFailedSchema,
  TaskEntrypointSchema: () => TaskEntrypointSchema,
  TaskChildInvokeRequestedSchema: () => TaskChildInvokeRequestedSchema,
  StructuredLogRequestedSchema: () => StructuredLogRequestedSchema,
  SessionStatusRequestedSchema: () => SessionStatusRequestedSchema,
  SessionOutputPageRequestedSchema: () => SessionOutputPageRequestedSchema,
  SessionInputSendRequestedSchema: () => SessionInputSendRequestedSchema,
  SessionCloseRequestedSchema: () => SessionCloseRequestedSchema,
  ScheduleCauseSchema: () => ScheduleCauseSchema,
  RunWaitRequestedSchema: () => RunWaitRequestedSchema,
  RunEventSchema: () => RunEventSchema,
  RunCauseSchema: () => RunCauseSchema,
  ResumeDecisionSchema: () => ResumeDecisionSchema,
  ResumeConsumedSchema: () => ResumeConsumedSchema,
  ResumeAttachSchema: () => ResumeAttachSchema,
  ResumeAckSchema: () => ResumeAckSchema,
  ProgramSupervisorCommandSchema: () => ProgramSupervisorCommandSchema,
  ProgramStartSchema: () => ProgramStartSchema,
  ProgramStartReleaseSchema: () => ProgramStartReleaseSchema,
  ProgramSecretsCompleteSchema: () => ProgramSecretsCompleteSchema,
  ProgramSecretSchema: () => ProgramSecretSchema,
  ProgramRunRequestSchema: () => ProgramRunRequestSchema,
  ProgramQuiescedSchema: () => ProgramQuiescedSchema,
  ProgramProcessStartedSchema: () => ProgramProcessStartedSchema,
  NoPayloadSchema: () => NoPayloadSchema,
  MetadataUpdatedSchema: () => MetadataUpdatedSchema,
  ManualCauseSchema: () => ManualCauseSchema,
  EntrypointReleaseSchema: () => EntrypointReleaseSchema,
  EntrypointReadySchema: () => EntrypointReadySchema,
  EntrypointIdentitySchema: () => EntrypointIdentitySchema,
  ContinuationCauseSchema: () => ContinuationCauseSchema,
  ChildCauseSchema: () => ChildCauseSchema,
  CheckpointPauseRequestSchema: () => CheckpointPauseRequestSchema,
  CheckpointPauseReadySchema: () => CheckpointPauseReadySchema,
  ApiCauseSchema: () => ApiCauseSchema,
  ActorTurnCommitRequestedSchema: () => ActorTurnCommitRequestedSchema,
  ActorTurnCommitPauseRequestSchema: () => ActorTurnCommitPauseRequestSchema,
  ActorTurnCommitPauseReadySchema: () => ActorTurnCommitPauseReadySchema,
  ActorSucceededSchema: () => ActorSucceededSchema,
  ActorStartSchema: () => ActorStartSchema,
  ActorStartRequestedSchema: () => ActorStartRequestedSchema,
  ActorStartCauseSchema: () => ActorStartCauseSchema,
  ActorOutputAppendRequestedSchema: () => ActorOutputAppendRequestedSchema,
  ActorOutcomeSchema: () => ActorOutcomeSchema,
  ActorFailedSchema: () => ActorFailedSchema,
  ActorEntrypointSchema: () => ActorEntrypointSchema
});
var file_run = /* @__PURE__ */ fileDesc("CglydW4ucHJvdG8SDGhlbG1yLnJ1bi52MCLLAgoMUHJvZ3JhbVN0YXJ0Eh4KFmVudHJ5cG9pbnRfZGVjbGFyZWRfaWQYASABKAkSDgoGcnVuX2lkGAIgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAMgASgNEiUKBWNhdXNlGAQgASgLMhYuaGVsbXIucnVuLnYwLlJ1bkNhdXNlEhUKDWRlcGxveW1lbnRfaWQYBSABKAkSGgoSZGVwbG95bWVudF92ZXJzaW9uGAYgASgJEhQKDHdvcmtzcGFjZV9pZBgHIAEoCRIhChliYXNlX3dvcmtzcGFjZV92ZXJzaW9uX2lkGAggASgJEicKBHRhc2sYCSABKAsyFy5oZWxtci5ydW4udjAuVGFza1N0YXJ0SAASKQoFYWN0b3IYCiABKAsyGC5oZWxtci5ydW4udjAuQWN0b3JTdGFydEgAQgwKCmVudHJ5cG9pbnQiXQoJVGFza1N0YXJ0Ei0KCm5vX3BheWxvYWQYASABKAsyFy5oZWxtci5ydW4udjAuTm9QYXlsb2FkSAASFgoMcGF5bG9hZF9qc29uGAIgASgMSABCCQoHcGF5bG9hZCILCglOb1BheWxvYWQidgoKQWN0b3JTdGFydBISCgpzZXNzaW9uX2lkGAEgASgJEhAKA2tleRgCIAEoCUgAiAEBEhwKFHN0YXJ0X2lucHV0X3NlcXVlbmNlGAMgASgDEhwKFGlucHV0X2hpZ2hfd2F0ZXJtYXJrGAQgASgDQgYKBF9rZXkisQIKCFJ1bkNhdXNlEiUKA2FwaRgBIAEoCzIWLmhlbG1yLnJ1bi52MC5BcGlDYXVzZUgAEisKBm1hbnVhbBgCIAEoCzIZLmhlbG1yLnJ1bi52MC5NYW51YWxDYXVzZUgAEikKBWNoaWxkGAMgASgLMhguaGVsbXIucnVuLnYwLkNoaWxkQ2F1c2VIABIvCghzY2hlZHVsZRgEIAEoCzIbLmhlbG1yLnJ1bi52MC5TY2hlZHVsZUNhdXNlSAASNAoLYWN0b3Jfc3RhcnQYBSABKAsyHS5oZWxtci5ydW4udjAuQWN0b3JTdGFydENhdXNlSAASNwoMY29udGludWF0aW9uGAYgASgLMh8uaGVsbXIucnVuLnYwLkNvbnRpbnVhdGlvbkNhdXNlSABCBgoEa2luZCIKCghBcGlDYXVzZSINCgtNYW51YWxDYXVzZSIjCgpDaGlsZENhdXNlEhUKDXBhcmVudF9ydW5faWQYASABKAkiogEKDVNjaGVkdWxlQ2F1c2USEwoLc2NoZWR1bGVfaWQYASABKAkSHAoUc2NoZWR1bGVkX2F0X3VuaXhfbXMYAiABKAMSKgodcHJldmlvdXNfc2NoZWR1bGVkX2F0X3VuaXhfbXMYAyABKANIAIgBARIQCgh0aW1lem9uZRgEIAEoCUIgCh5fcHJldmlvdXNfc2NoZWR1bGVkX2F0X3VuaXhfbXMiEQoPQWN0b3JTdGFydENhdXNlIhMKEUNvbnRpbnVhdGlvbkNhdXNlIqQBChFQcm9ncmFtUnVuUmVxdWVzdBIOCgZydW5faWQYASABKAkSFgoOYXR0ZW1wdF9udW1iZXIYAiABKA0SFAoMcnVuX2xlYXNlX2lkGAMgASgJEhsKE3Byb2dyYW1fc3RhcnRfZnJhbWUYBCABKAwSFAoMc2VjcmV0X2NvdW50GAUgASgNEh4KFnN0YXJ0X2RlYWRsaW5lX3VuaXhfbXMYBiABKAMiSgoNUHJvZ3JhbVNlY3JldBINCgNlbnYYASABKAlIABIOCgRmaWxlGAIgASgJSAASDQoFdmFsdWUYAyABKAxCCwoJcGxhY2VtZW50ImwKFlByb2dyYW1TZWNyZXRzQ29tcGxldGUSDgoGcnVuX2lkGAEgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAIgASgNEhQKDHJ1bl9sZWFzZV9pZBgDIAEoCRIUCgxzZWNyZXRfY291bnQYBCABKA0imgIKGFByb2dyYW1TdXBlcnZpc29yQ29tbWFuZBI2Cg9zZWNyZXRfZGVsaXZlcnkYASABKAsyGy5oZWxtci5ydW4udjAuUHJvZ3JhbVNlY3JldEgAEkAKEHNlY3JldHNfY29tcGxldGUYAiABKAsyJC5oZWxtci5ydW4udjAuUHJvZ3JhbVNlY3JldHNDb21wbGV0ZUgAEjoKDXN0YXJ0X3JlbGVhc2UYAyABKAsyIS5oZWxtci5ydW4udjAuUHJvZ3JhbVN0YXJ0UmVsZWFzZUgAEj0KEmVudHJ5cG9pbnRfcmVsZWFzZRgEIAEoCzIfLmhlbG1yLnJ1bi52MC5FbnRyeXBvaW50UmVsZWFzZUgAQgkKB2NvbW1hbmQiVQoVUHJvZ3JhbVByb2Nlc3NTdGFydGVkEg4KBnJ1bl9pZBgBIAEoCRIWCg5hdHRlbXB0X251bWJlchgCIAEoDRIUCgxydW5fbGVhc2VfaWQYAyABKAkiUwoTUHJvZ3JhbVN0YXJ0UmVsZWFzZRIOCgZydW5faWQYASABKAkSFgoOYXR0ZW1wdF9udW1iZXIYAiABKA0SFAoMcnVuX2xlYXNlX2lkGAMgASgJIo8BChJFbnRyeXBvaW50SWRlbnRpdHkSEwoLZGVjbGFyZWRfaWQYASABKAkSLAoEdGFzaxgCIAEoCzIcLmhlbG1yLnJ1bi52MC5UYXNrRW50cnlwb2ludEgAEi4KBWFjdG9yGAMgASgLMh0uaGVsbXIucnVuLnYwLkFjdG9yRW50cnlwb2ludEgAQgYKBGtpbmQiEAoOVGFza0VudHJ5cG9pbnQiEQoPQWN0b3JFbnRyeXBvaW50Im8KD0VudHJ5cG9pbnRSZWFkeRIOCgZydW5faWQYASABKAkSFgoOYXR0ZW1wdF9udW1iZXIYAiABKA0SNAoKZW50cnlwb2ludBgDIAEoCzIgLmhlbG1yLnJ1bi52MC5FbnRyeXBvaW50SWRlbnRpdHkicQoRRW50cnlwb2ludFJlbGVhc2USDgoGcnVuX2lkGAEgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAIgASgNEjQKCmVudHJ5cG9pbnQYAyABKAsyIC5oZWxtci5ydW4udjAuRW50cnlwb2ludElkZW50aXR5ItkOCghSdW5FdmVudBIWCgxzdGRvdXRfY2h1bmsYASABKAxIABIWCgxzdGRlcnJfY2h1bmsYAiABKAxIABI8ChJydW5fd2FpdF9yZXF1ZXN0ZWQYBSABKAsyHi5oZWxtci5ydW4udjAuUnVuV2FpdFJlcXVlc3RlZEgAEjkKEG1ldGFkYXRhX3VwZGF0ZWQYByABKAsyHS5oZWxtci5ydW4udjAuTWV0YWRhdGFVcGRhdGVkSAASRAoWdG9rZW5fY3JlYXRlX3JlcXVlc3RlZBgIIAEoCzIiLmhlbG1yLnJ1bi52MC5Ub2tlbkNyZWF0ZVJlcXVlc3RlZEgAEjcKD3Jlc3VtZV9jb25zdW1lZBgGIAEoCzIcLmhlbG1yLnJ1bi52MC5SZXN1bWVDb25zdW1lZEgAEkYKF3Byb2dyYW1fcHJvY2Vzc19zdGFydGVkGAsgASgLMiMuaGVsbXIucnVuLnYwLlByb2dyYW1Qcm9jZXNzU3RhcnRlZEgAEjkKEGVudHJ5cG9pbnRfcmVhZHkYDCABKAsyHS5oZWxtci5ydW4udjAuRW50cnlwb2ludFJlYWR5SAASMQoMdGFza19vdXRjb21lGA0gASgLMhkuaGVsbXIucnVuLnYwLlRhc2tPdXRjb21lSAASOQoQcHJvZ3JhbV9xdWllc2NlZBgOIAEoCzIdLmhlbG1yLnJ1bi52MC5Qcm9ncmFtUXVpZXNjZWRIABIzCg1hY3Rvcl9vdXRjb21lGA8gASgLMhouaGVsbXIucnVuLnYwLkFjdG9yT3V0Y29tZUgAEk0KG2FjdG9yX3R1cm5fY29tbWl0X3JlcXVlc3RlZBgQIAEoCzImLmhlbG1yLnJ1bi52MC5BY3RvclR1cm5Db21taXRSZXF1ZXN0ZWRIABJRCh1hY3Rvcl9vdXRwdXRfYXBwZW5kX3JlcXVlc3RlZBgRIAEoCzIoLmhlbG1yLnJ1bi52MC5BY3Rvck91dHB1dEFwcGVuZFJlcXVlc3RlZEgAEk8KHHNlc3Npb25faW5wdXRfc2VuZF9yZXF1ZXN0ZWQYEiABKAsyJy5oZWxtci5ydW4udjAuU2Vzc2lvbklucHV0U2VuZFJlcXVlc3RlZEgAEkgKGHN0cnVjdHVyZWRfbG9nX3JlcXVlc3RlZBgTIAEoCzIkLmhlbG1yLnJ1bi52MC5TdHJ1Y3R1cmVkTG9nUmVxdWVzdGVkSAASTQobdGFza19jaGlsZF9pbnZva2VfcmVxdWVzdGVkGBQgASgLMiYuaGVsbXIucnVuLnYwLlRhc2tDaGlsZEludm9rZVJlcXVlc3RlZEgAEkIKFWFjdG9yX3N0YXJ0X3JlcXVlc3RlZBgVIAEoCzIhLmhlbG1yLnJ1bi52MC5BY3RvclN0YXJ0UmVxdWVzdGVkSAASSAoYc2Vzc2lvbl9zdGF0dXNfcmVxdWVzdGVkGBYgASgLMiQuaGVsbXIucnVuLnYwLlNlc3Npb25TdGF0dXNSZXF1ZXN0ZWRIABJGChdzZXNzaW9uX2Nsb3NlX3JlcXVlc3RlZBgXIAEoCzIjLmhlbG1yLnJ1bi52MC5TZXNzaW9uQ2xvc2VSZXF1ZXN0ZWRIABJRCh1zZXNzaW9uX291dHB1dF9wYWdlX3JlcXVlc3RlZBgYIAEoCzIoLmhlbG1yLnJ1bi52MC5TZXNzaW9uT3V0cHV0UGFnZVJlcXVlc3RlZEgAEkwKGndvcmtzcGFjZV9jcmVhdGVfcmVxdWVzdGVkGBkgASgLMiYuaGVsbXIucnVuLnYwLldvcmtzcGFjZUNyZWF0ZVJlcXVlc3RlZEgAElAKHHdvcmtzcGFjZV9yZXRyaWV2ZV9yZXF1ZXN0ZWQYGiABKAsyKC5oZWxtci5ydW4udjAuV29ya3NwYWNlUmV0cmlldmVSZXF1ZXN0ZWRIABJRCh13b3Jrc3BhY2VfZmlsZV9yZWFkX3JlcXVlc3RlZBgbIAEoCzIoLmhlbG1yLnJ1bi52MC5Xb3Jrc3BhY2VGaWxlUmVhZFJlcXVlc3RlZEgAElEKHXdvcmtzcGFjZV9maWxlX3N0YXRfcmVxdWVzdGVkGBwgASgLMiguaGVsbXIucnVuLnYwLldvcmtzcGFjZUZpbGVTdGF0UmVxdWVzdGVkSAASUQodd29ya3NwYWNlX2ZpbGVfbGlzdF9yZXF1ZXN0ZWQYHSABKAsyKC5oZWxtci5ydW4udjAuV29ya3NwYWNlRmlsZUxpc3RSZXF1ZXN0ZWRIABJIChh3b3Jrc3BhY2VfZXhlY19yZXF1ZXN0ZWQYHiABKAsyJC5oZWxtci5ydW4udjAuV29ya3NwYWNlRXhlY1JlcXVlc3RlZEgAEkwKGndvcmtzcGFjZV9kZWxldGVfcmVxdWVzdGVkGB8gASgLMiYuaGVsbXIucnVuLnYwLldvcmtzcGFjZURlbGV0ZVJlcXVlc3RlZEgAQgcKBWV2ZW50SgQIAxAESgQICRAKSgQIChALIrMBCgtUYXNrT3V0Y29tZRIwCglzdWNjZWVkZWQYASABKAsyGy5oZWxtci5ydW4udjAuVGFza1N1Y2NlZWRlZEgAEioKBmZhaWxlZBgCIAEoCzIYLmhlbG1yLnJ1bi52MC5UYXNrRmFpbGVkSAASOwoPcGF5bG9hZF9pbnZhbGlkGAMgASgLMiAuaGVsbXIucnVuLnYwLlRhc2tQYXlsb2FkSW52YWxpZEgAQgkKB291dGNvbWUiJAoNVGFza1N1Y2NlZWRlZBITCgtvdXRwdXRfanNvbhgBIAEoCSJJCgpUYXNrRmFpbGVkEg8KB21lc3NhZ2UYASABKAkSGQoMZGV0YWlsc19qc29uGAIgASgJSACIAQFCDwoNX2RldGFpbHNfanNvbiJRChJUYXNrUGF5bG9hZEludmFsaWQSDwoHbWVzc2FnZRgBIAEoCRIZCgxkZXRhaWxzX2pzb24YAiABKAlIAIgBAUIPCg1fZGV0YWlsc19qc29uIrsBCgxBY3Rvck91dGNvbWUSJAoXdGVybWluYWxfaW5wdXRfc2VxdWVuY2UYASABKANIAYgBARIxCglzdWNjZWVkZWQYAiABKAsyHC5oZWxtci5ydW4udjAuQWN0b3JTdWNjZWVkZWRIABIrCgZmYWlsZWQYAyABKAsyGS5oZWxtci5ydW4udjAuQWN0b3JGYWlsZWRIAEIJCgdvdXRjb21lQhoKGF90ZXJtaW5hbF9pbnB1dF9zZXF1ZW5jZSIQCg5BY3RvclN1Y2NlZWRlZCJKCgtBY3RvckZhaWxlZBIPCgdtZXNzYWdlGAEgASgJEhkKDGRldGFpbHNfanNvbhgCIAEoCUgAiAEBQg8KDV9kZXRhaWxzX2pzb24iUQoYQWN0b3JUdXJuQ29tbWl0UmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEh0KFXRhcmdldF9pbnB1dF9zZXF1ZW5jZRgCIAEoAyKhAgobQWN0b3JUdXJuQ29tbWl0UGF1c2VSZXF1ZXN0EhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEh0KFXRhcmdldF9pbnB1dF9zZXF1ZW5jZRgCIAEoAxIOCgZydW5faWQYAyABKAkSFgoOYXR0ZW1wdF9udW1iZXIYBCABKA0SFAoMcnVuX2xlYXNlX2lkGAUgASgJEhwKFGV4cGVjdGVkX3RyZWVfZGlnZXN0GAYgASgJEiAKGGV4cGVjdGVkX3RyZWVfc2l6ZV9ieXRlcxgHIAEoAxIhChlleHBlY3RlZF90cmVlX2VudHJ5X2NvdW50GAggASgNEioKImV4cGVjdGVkX2Jhc2Vfd29ya3NwYWNlX3ZlcnNpb25faWQYCSABKAki8wEKGUFjdG9yVHVybkNvbW1pdFBhdXNlUmVhZHkSFgoOY29ycmVsYXRpb25faWQYASABKAkSHQoVdGFyZ2V0X2lucHV0X3NlcXVlbmNlGAIgASgDEg4KBnJ1bl9pZBgDIAEoCRIWCg5hdHRlbXB0X251bWJlchgEIAEoDRIUCgxydW5fbGVhc2VfaWQYBSABKAkSEwoLdHJlZV9kaWdlc3QYBiABKAkSFwoPdHJlZV9zaXplX2J5dGVzGAcgASgDEhgKEHRyZWVfZW50cnlfY291bnQYCCABKA0SGQoRd29ya3NwYWNlX2NoYW5nZWQYCSABKAgijwEKGkFjdG9yT3V0cHV0QXBwZW5kUmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEhEKCWRhdGFfanNvbhgCIAEoCRIUCgxjb250ZW50X3R5cGUYAyABKAkSHAoPaWRlbXBvdGVuY3lfa2V5GAQgASgJSACIAQFCEgoQX2lkZW1wb3RlbmN5X2tleSKMAQoZU2Vzc2lvbklucHV0U2VuZFJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJEhEKCWRhdGFfanNvbhgDIAEoCRIcCg9pZGVtcG90ZW5jeV9rZXkYBCABKAlIAIgBAUISChBfaWRlbXBvdGVuY3lfa2V5IuYBChNBY3RvclN0YXJ0UmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEhMKC2RlY2xhcmVkX2lkGAIgASgJEhQKDHdvcmtzcGFjZV9pZBgDIAEoCRIQCgNrZXkYBSABKAlIAIgBARIXCgppbnB1dF9qc29uGAYgASgJSAGIAQESHAoPaWRlbXBvdGVuY3lfa2V5GAcgASgJSAKIAQESGAoQcnVuX29wdGlvbnNfanNvbhgIIAEoCUIGCgRfa2V5Qg0KC19pbnB1dF9qc29uQhIKEF9pZGVtcG90ZW5jeV9rZXkiRAoWU2Vzc2lvblN0YXR1c1JlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJInUKFVNlc3Npb25DbG9zZVJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJEhwKD2lkZW1wb3RlbmN5X2tleRgDIAEoCUgAiAEBQhIKEF9pZGVtcG90ZW5jeV9rZXkidQoaU2Vzc2lvbk91dHB1dFBhZ2VSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSEgoKc2Vzc2lvbl9pZBgCIAEoCRISCgVhZnRlchgDIAEoA0gAiAEBEg0KBWxpbWl0GAQgASgNQggKBl9hZnRlciIoChBXb3Jrc3BhY2VBZGRyZXNzEhQKDHdvcmtzcGFjZV9pZBgBIAEoCSJUChhXb3Jrc3BhY2VTZWNyZXRQbGFjZW1lbnQSDAoEbmFtZRgBIAEoCRINCgNlbnYYAiABKAlIABIOCgRmaWxlGAMgASgJSABCCwoJcGxhY2VtZW50IswBChhXb3Jrc3BhY2VDcmVhdGVSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSEwoLZGVjbGFyZWRfaWQYAiABKAkSEAoDa2V5GAMgASgJSACIAQESNwoHc2VjcmV0cxgEIAMoCzImLmhlbG1yLnJ1bi52MC5Xb3Jrc3BhY2VTZWNyZXRQbGFjZW1lbnQSHAoPaWRlbXBvdGVuY3lfa2V5GAUgASgJSAGIAQFCBgoEX2tleUISChBfaWRlbXBvdGVuY3lfa2V5ImcKGldvcmtzcGFjZVJldHJpZXZlUmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEjEKCXdvcmtzcGFjZRgCIAEoCzIeLmhlbG1yLnJ1bi52MC5Xb3Jrc3BhY2VBZGRyZXNzInUKGldvcmtzcGFjZUZpbGVSZWFkUmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEjEKCXdvcmtzcGFjZRgCIAEoCzIeLmhlbG1yLnJ1bi52MC5Xb3Jrc3BhY2VBZGRyZXNzEgwKBHBhdGgYAyABKAkidQoaV29ya3NwYWNlRmlsZVN0YXRSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSMQoJd29ya3NwYWNlGAIgASgLMh4uaGVsbXIucnVuLnYwLldvcmtzcGFjZUFkZHJlc3MSDAoEcGF0aBgDIAEoCSKkAQoaV29ya3NwYWNlRmlsZUxpc3RSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSMQoJd29ya3NwYWNlGAIgASgLMh4uaGVsbXIucnVuLnYwLldvcmtzcGFjZUFkZHJlc3MSDAoEcGF0aBgDIAEoCRITCgZjdXJzb3IYBCABKAlIAIgBARINCgVsaW1pdBgFIAEoDUIJCgdfY3Vyc29yIsYCChZXb3Jrc3BhY2VFeGVjUmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEjEKCXdvcmtzcGFjZRgCIAEoCzIeLmhlbG1yLnJ1bi52MC5Xb3Jrc3BhY2VBZGRyZXNzEg8KB2NvbW1hbmQYAyADKAkSEAoDY3dkGAQgASgJSACIAQESOgoDZW52GAUgAygLMi0uaGVsbXIucnVuLnYwLldvcmtzcGFjZUV4ZWNSZXF1ZXN0ZWQuRW52RW50cnkSDQoFc3RkaW4YBiABKAwSFwoKdGltZW91dF9tcxgHIAEoBEgBiAEBEhcKD2lkZW1wb3RlbmN5X2tleRgIIAEoCRoqCghFbnZFbnRyeRILCgNrZXkYASABKAkSDQoFdmFsdWUYAiABKAk6AjgBQgYKBF9jd2RCDQoLX3RpbWVvdXRfbXMilwEKGFdvcmtzcGFjZURlbGV0ZVJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRIxCgl3b3Jrc3BhY2UYAiABKAsyHi5oZWxtci5ydW4udjAuV29ya3NwYWNlQWRkcmVzcxIcCg9pZGVtcG90ZW5jeV9rZXkYAyABKAlIAIgBAUISChBfaWRlbXBvdGVuY3lfa2V5Ik8KD1Byb2dyYW1RdWllc2NlZBIOCgZydW5faWQYASABKAkSFgoOYXR0ZW1wdF9udW1iZXIYAiABKA0SFAoMcnVuX2xlYXNlX2lkGAMgASgJIuYCChBSdW5XYWl0UmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEgwKBGtpbmQYAiABKAkSEwoLcGFyYW1zX2pzb24YAyABKAkSGgoNbWV0YWRhdGFfanNvbhgEIAEoCUgAiAEBEhcKCnRpbWVvdXRfbXMYBSABKARIAYgBARIMCgR0YWdzGAYgAygJEhMKC3J1bl93YWl0X2lkGAcgASgJEhgKEHJlc3VtZV9hdHRhY2hfaWQYCCABKAkSHAoPaWRsZV90aW1lb3V0X21zGAkgASgESAKIAQESLQogYWN0b3Jfc3BlY3VsYXRpdmVfaW5wdXRfc2VxdWVuY2UYCiABKANIA4gBAUIQCg5fbWV0YWRhdGFfanNvbkINCgtfdGltZW91dF9tc0ISChBfaWRsZV90aW1lb3V0X21zQiMKIV9hY3Rvcl9zcGVjdWxhdGl2ZV9pbnB1dF9zZXF1ZW5jZSLEAQoUVG9rZW5DcmVhdGVSZXF1ZXN0ZWQSFwoKdGltZW91dF9tcxgBIAEoBEgAiAEBEhYKDmNvcnJlbGF0aW9uX2lkGAIgASgJEhwKD2lkZW1wb3RlbmN5X2tleRgDIAEoCUgBiAEBEgwKBHRhZ3MYBCADKAkSGgoNbWV0YWRhdGFfanNvbhgFIAEoCUgCiAEBQg0KC190aW1lb3V0X21zQhIKEF9pZGVtcG90ZW5jeV9rZXlCEAoOX21ldGFkYXRhX2pzb24i/wIKGFRhc2tDaGlsZEludm9rZVJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRITCgtkZWNsYXJlZF9pZBgCIAEoCRIOCgZtZXRob2QYAyABKAkSFwoPcGF5bG9hZF9wcmVzZW50GAQgASgIEhkKDHBheWxvYWRfanNvbhgFIAEoCUgAiAEBEhYKDndvcmtzcGFjZV9qc29uGAYgASgJEhQKDG9wdGlvbnNfanNvbhgHIAEoCRIcCg9pZGVtcG90ZW5jeV9rZXkYCCABKAlIAYgBARItCiBhY3Rvcl9zcGVjdWxhdGl2ZV9pbnB1dF9zZXF1ZW5jZRgJIAEoA0gCiAEBEhMKC3J1bl93YWl0X2lkGAogASgJEhgKEHJlc3VtZV9hdHRhY2hfaWQYCyABKAlCDwoNX3BheWxvYWRfanNvbkISChBfaWRlbXBvdGVuY3lfa2V5QiMKIV9hY3Rvcl9zcGVjdWxhdGl2ZV9pbnB1dF9zZXF1ZW5jZSLzAQoWQ2hlY2twb2ludFBhdXNlUmVxdWVzdBITCgtydW5fd2FpdF9pZBgBIAEoCRIVCg1jaGVja3BvaW50X2lkGAIgASgJEhkKEWNhcHR1cmVfd29ya3NwYWNlGAMgASgIEg4KBnJ1bl9pZBgEIAEoCRIWCg5hdHRlbXB0X251bWJlchgFIAEoDRIUCgxydW5fbGVhc2VfaWQYBiABKAkSGAoQcmVzdW1lX2F0dGFjaF9pZBgHIAEoCRIiChpjaGVja3BvaW50X3JlcXVlc3RfdmVyc2lvbhgIIAEoAxIWCg5jb3JyZWxhdGlvbl9pZBgJIAEoCSLWAQoUQ2hlY2twb2ludFBhdXNlUmVhZHkSEwoLcnVuX3dhaXRfaWQYASABKAkSFQoNY2hlY2twb2ludF9pZBgCIAEoCRIOCgZydW5faWQYAyABKAkSFgoOYXR0ZW1wdF9udW1iZXIYBCABKA0SFAoMcnVuX2xlYXNlX2lkGAUgASgJEhgKEHJlc3VtZV9hdHRhY2hfaWQYBiABKAkSIgoaY2hlY2twb2ludF9yZXF1ZXN0X3ZlcnNpb24YByABKAMSFgoOY29ycmVsYXRpb25faWQYCCABKAkiygEKDFJlc3VtZUF0dGFjaBIVCg1jaGVja3BvaW50X2lkGAEgASgJEhMKC3J1bl93YWl0X2lkGAIgASgJEhQKDHJ1bl9sZWFzZV9pZBgDIAEoCRIOCgZydW5faWQYBCABKAkSFgoOYXR0ZW1wdF9udW1iZXIYBSABKA0SGAoQcmVzdW1lX2F0dGFjaF9pZBgGIAEoCRIeChZyZXN1bWVfcmVxdWVzdF92ZXJzaW9uGAcgASgDEhYKDmNvcnJlbGF0aW9uX2lkGAggASgJIvYBCg5SZXN1bWVEZWNpc2lvbhITCgtydW5fd2FpdF9pZBgBIAEoCRIMCgRraW5kGAIgASgJEhEKCWRhdGFfanNvbhgDIAEoCRIcChRyZXF1aXJlX2NvbnN1bWVkX2FjaxgEIAEoCBIVCg1jaGVja3BvaW50X2lkGAUgASgJEhgKEHJlc3VtZV9hdHRhY2hfaWQYBiABKAkSHgoWcmVzdW1lX3JlcXVlc3RfdmVyc2lvbhgHIAEoAxIUCgxydW5fbGVhc2VfaWQYCCABKAkSFgoOY29ycmVsYXRpb25faWQYCSABKAkSEQoJbm9fcmVzdWx0GAogASgIIp8BCglSZXN1bWVBY2sSEwoLcnVuX3dhaXRfaWQYASABKAkSFQoNY2hlY2twb2ludF9pZBgCIAEoCRIYChByZXN1bWVfYXR0YWNoX2lkGAMgASgJEh4KFnJlc3VtZV9yZXF1ZXN0X3ZlcnNpb24YBCABKAMSFAoMcnVuX2xlYXNlX2lkGAUgASgJEhYKDmNvcnJlbGF0aW9uX2lkGAYgASgJIqQBCg5SZXN1bWVDb25zdW1lZBITCgtydW5fd2FpdF9pZBgBIAEoCRIVCg1jaGVja3BvaW50X2lkGAIgASgJEhgKEHJlc3VtZV9hdHRhY2hfaWQYAyABKAkSHgoWcmVzdW1lX3JlcXVlc3RfdmVyc2lvbhgEIAEoAxIUCgxydW5fbGVhc2VfaWQYBSABKAkSFgoOY29ycmVsYXRpb25faWQYBiABKAkixgEKD01ldGFkYXRhVXBkYXRlZBIRCglvcGVyYXRpb24YASABKAkSEAoDa2V5GAIgASgJSACIAQESFwoKdmFsdWVfanNvbhgDIAEoCUgBiAEBEhcKCnBhdGNoX2pzb24YBCABKAlIAogBARITCgZhbW91bnQYBSABKAFIA4gBARIWCg5jb3JyZWxhdGlvbl9pZBgGIAEoCUIGCgRfa2V5Qg0KC192YWx1ZV9qc29uQg0KC19wYXRjaF9qc29uQgkKB19hbW91bnQiaQoWU3RydWN0dXJlZExvZ1JlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRINCgVsZXZlbBgCIAEoCRIPCgdtZXNzYWdlGAMgASgJEhcKD2F0dHJpYnV0ZXNfanNvbhgEIAEoCUI6WjhnaXRodWIuY29tL2hlbG1yZG90ZGV2L2hlbG1yL2ludGVybmFsL3Byb3RvL3J1bi92MDtydW52MGIGcHJvdG8z");
var ProgramStartSchema = /* @__PURE__ */ messageDesc(file_run, 0);
var TaskStartSchema = /* @__PURE__ */ messageDesc(file_run, 1);
var NoPayloadSchema = /* @__PURE__ */ messageDesc(file_run, 2);
var ActorStartSchema = /* @__PURE__ */ messageDesc(file_run, 3);
var RunCauseSchema = /* @__PURE__ */ messageDesc(file_run, 4);
var ApiCauseSchema = /* @__PURE__ */ messageDesc(file_run, 5);
var ManualCauseSchema = /* @__PURE__ */ messageDesc(file_run, 6);
var ChildCauseSchema = /* @__PURE__ */ messageDesc(file_run, 7);
var ScheduleCauseSchema = /* @__PURE__ */ messageDesc(file_run, 8);
var ActorStartCauseSchema = /* @__PURE__ */ messageDesc(file_run, 9);
var ContinuationCauseSchema = /* @__PURE__ */ messageDesc(file_run, 10);
var ProgramRunRequestSchema = /* @__PURE__ */ messageDesc(file_run, 11);
var ProgramSecretSchema = /* @__PURE__ */ messageDesc(file_run, 12);
var ProgramSecretsCompleteSchema = /* @__PURE__ */ messageDesc(file_run, 13);
var ProgramSupervisorCommandSchema = /* @__PURE__ */ messageDesc(file_run, 14);
var ProgramProcessStartedSchema = /* @__PURE__ */ messageDesc(file_run, 15);
var ProgramStartReleaseSchema = /* @__PURE__ */ messageDesc(file_run, 16);
var EntrypointIdentitySchema = /* @__PURE__ */ messageDesc(file_run, 17);
var TaskEntrypointSchema = /* @__PURE__ */ messageDesc(file_run, 18);
var ActorEntrypointSchema = /* @__PURE__ */ messageDesc(file_run, 19);
var EntrypointReadySchema = /* @__PURE__ */ messageDesc(file_run, 20);
var EntrypointReleaseSchema = /* @__PURE__ */ messageDesc(file_run, 21);
var RunEventSchema = /* @__PURE__ */ messageDesc(file_run, 22);
var TaskOutcomeSchema = /* @__PURE__ */ messageDesc(file_run, 23);
var TaskSucceededSchema = /* @__PURE__ */ messageDesc(file_run, 24);
var TaskFailedSchema = /* @__PURE__ */ messageDesc(file_run, 25);
var TaskPayloadInvalidSchema = /* @__PURE__ */ messageDesc(file_run, 26);
var ActorOutcomeSchema = /* @__PURE__ */ messageDesc(file_run, 27);
var ActorSucceededSchema = /* @__PURE__ */ messageDesc(file_run, 28);
var ActorFailedSchema = /* @__PURE__ */ messageDesc(file_run, 29);
var ActorTurnCommitRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 30);
var ActorTurnCommitPauseRequestSchema = /* @__PURE__ */ messageDesc(file_run, 31);
var ActorTurnCommitPauseReadySchema = /* @__PURE__ */ messageDesc(file_run, 32);
var ActorOutputAppendRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 33);
var SessionInputSendRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 34);
var ActorStartRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 35);
var SessionStatusRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 36);
var SessionCloseRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 37);
var SessionOutputPageRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 38);
var WorkspaceAddressSchema = /* @__PURE__ */ messageDesc(file_run, 39);
var WorkspaceSecretPlacementSchema = /* @__PURE__ */ messageDesc(file_run, 40);
var WorkspaceCreateRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 41);
var WorkspaceRetrieveRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 42);
var WorkspaceFileReadRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 43);
var WorkspaceFileStatRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 44);
var WorkspaceFileListRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 45);
var WorkspaceExecRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 46);
var WorkspaceDeleteRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 47);
var ProgramQuiescedSchema = /* @__PURE__ */ messageDesc(file_run, 48);
var RunWaitRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 49);
var TokenCreateRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 50);
var TaskChildInvokeRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 51);
var CheckpointPauseRequestSchema = /* @__PURE__ */ messageDesc(file_run, 52);
var CheckpointPauseReadySchema = /* @__PURE__ */ messageDesc(file_run, 53);
var ResumeAttachSchema = /* @__PURE__ */ messageDesc(file_run, 54);
var ResumeDecisionSchema = /* @__PURE__ */ messageDesc(file_run, 55);
var ResumeAckSchema = /* @__PURE__ */ messageDesc(file_run, 56);
var ResumeConsumedSchema = /* @__PURE__ */ messageDesc(file_run, 57);
var MetadataUpdatedSchema = /* @__PURE__ */ messageDesc(file_run, 58);
var StructuredLogRequestedSchema = /* @__PURE__ */ messageDesc(file_run, 59);
// sdk/typescript/src/config.ts
var arrayPrototype = Array.prototype;
var objectPrototype = Object.prototype;
var startsWith = String.prototype.startsWith.call.bind(String.prototype.startsWith);
var endsWith = String.prototype.endsWith.call.bind(String.prototype.endsWith);
var includes = String.prototype.includes.call.bind(String.prototype.includes);
var split = String.prototype.split.call.bind(String.prototype.split);
var slice = String.prototype.slice.call.bind(String.prototype.slice);
var charCodeAt = String.prototype.charCodeAt.call.bind(String.prototype.charCodeAt);
var regexpTest = RegExp.prototype.test.call.bind(RegExp.prototype.test);
var utf8Encoder = new TextEncoder;
var encodeUTF8 = TextEncoder.prototype.encode.call.bind(TextEncoder.prototype.encode);
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
function isAsciiAlnum(code) {
  return code >= 48 && code <= 57 || code >= 65 && code <= 90 || code >= 97 && code <= 122;
}

// sdk/typescript/src/internal/runtime.ts
var runtimeOperationsSymbol = Symbol.for("helmr.sdk.v0.runtime_operations");
function installRuntimeOperations(operations) {
  const target = globalThis;
  if (target[runtimeOperationsSymbol] !== undefined) {
    throw new Error("Helmr runtime operations are already installed");
  }
  const installed = Object.freeze(operations);
  target[runtimeOperationsSymbol] = installed;
  return () => {
    if (target[runtimeOperationsSymbol] === installed) {
      delete target[runtimeOperationsSymbol];
    }
  };
}
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

// sdk/typescript/src/internal/run-handle.ts
function createRunHandle(id) {
  return Object.freeze({
    id: resourceID(id, "Run ID")
  });
}

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
// sdk/typescript/src/secret.ts
var secretAddressBrand = Symbol.for("helmr.sdk.v0.secret-address");
var secretNamePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/;

class SecretNameAddress {
  name;
  constructor(name) {
    validateSecretName(name);
    this.name = name;
    Object.defineProperty(this, secretAddressBrand, { value: true });
    Object.freeze(this);
  }
}
var secrets = Object.freeze({
  fromName(name) {
    return new SecretNameAddress(name);
  }
});
function inspectSecretAddress(value) {
  if (typeof value !== "object" || value === null || value[secretAddressBrand] !== true) {
    return;
  }
  const name = value.name;
  if (typeof name !== "string") {
    throw new Error("private Secret address is invalid");
  }
  validateSecretName(name);
  return name;
}
function validateSecretName(value) {
  if (!secretNamePattern.test(value)) {
    throw new Error("Secret name is invalid");
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
// sdk/typescript/src/internal/timestamp.ts
var utcRFC3339 = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,9})?Z$/;
function timestampString(value, label) {
  const match = typeof value === "string" ? utcRFC3339.exec(value) : null;
  if (match === null || !validDateTime(match)) {
    throw new Error(`${label} must be a UTC RFC 3339 timestamp`);
  }
  return value;
}
function validDateTime(match) {
  const year = Number(match[1]);
  const month = Number(match[2]);
  const day = Number(match[3]);
  const hour = Number(match[4]);
  const minute = Number(match[5]);
  const second = Number(match[6]);
  if (month < 1 || month > 12 || hour > 23 || minute > 59 || second > 59) {
    return false;
  }
  const leap = year % 4 === 0 && (year % 100 !== 0 || year % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  return day >= 1 && day <= days[month - 1];
}

// sdk/typescript/src/workspace.ts
var sandboxDefinitionBrand = Symbol.for("helmr.sdk.v0.sandbox");
var workspaceAddressBrand = Symbol.for("helmr.sdk.v0.workspace-address");
var workspaces = Object.freeze({
  ref: createWorkspaceRef
});
function inspectWorkspaceAddress(value) {
  if (typeof value !== "object" || value === null || value[workspaceAddressBrand] !== true) {
    return;
  }
  const address = value;
  if (address.id === undefined) {
    throw new Error("private Workspace address is invalid");
  }
  return createWorkspaceAddress(address.id);
}
function workspaceRefID(value) {
  const address = inspectWorkspaceAddress(value);
  if (address === undefined || typeof address.id !== "string") {
    throw new Error("Workspace requires a Workspace ref");
  }
  return address.id;
}
function encodeWorkspaceSecrets(inputs) {
  if (inputs === undefined)
    return Object.freeze([]);
  if (!Array.isArray(inputs)) {
    throw new Error("Workspace secrets must be an array");
  }
  if (inputs.length > 64) {
    throw new Error("at most 64 Workspace Secret placements are allowed");
  }
  return Object.freeze(inputs.map((input) => {
    if (typeof input !== "object" || input === null || Array.isArray(input)) {
      throw new Error("Workspace Secret placement must be an object");
    }
    if (!Object.hasOwn(input, "secret")) {
      throw new Error("Workspace Secret placement.secret is required");
    }
    const name = inspectSecretAddress(input.secret);
    if (name === undefined) {
      throw new Error("Workspace Secret requires secrets.fromName()");
    }
    const hasEnv = Object.hasOwn(input, "env") && input.env !== undefined;
    const hasFile = Object.hasOwn(input, "file") && input.file !== undefined;
    if (hasEnv === hasFile) {
      throw new Error("Workspace Secret requires exactly one of env or file");
    }
    const allowed = new Set(hasEnv ? ["secret", "env"] : ["secret", "file"]);
    const unknown = Object.keys(input).find((key) => !allowed.has(key));
    if (unknown !== undefined) {
      throw new Error(`Workspace Secret placement has unknown member ${JSON.stringify(unknown)}`);
    }
    if (hasEnv) {
      if (typeof input.env !== "string" || input.env.length === 0) {
        throw new Error("Workspace Secret env target is required");
      }
      return Object.freeze({ name, env: input.env });
    }
    if (typeof input.file !== "string" || input.file.length === 0) {
      throw new Error("Workspace Secret file target is required");
    }
    return Object.freeze({ name, file: input.file });
  }));
}
function createWorkspaceRef(id) {
  const workspaceID = resourceID(id, "Workspace ID");
  const files = Object.freeze({
    read(path, options) {
      return currentRuntimeOperations().workspaceFileRead(workspaceID, path, options?.signal);
    },
    stat(path, options) {
      return currentRuntimeOperations().workspaceFileStat(workspaceID, path, options?.signal);
    },
    list(path, query, options) {
      return currentRuntimeOperations().workspaceFileList(workspaceID, path, query, options?.signal);
    }
  });
  const operations = {
    files,
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
function createWorkspaceAddress(id) {
  return brandWorkspaceAddress({ id: resourceID(id, "Workspace ID") });
}
function brandWorkspaceAddress(value) {
  resourceID(value.id, "Workspace ID");
  return freezeWorkspaceAddress(value);
}
function freezeWorkspaceAddress(value) {
  Object.defineProperty(value, workspaceAddressBrand, { value: true });
  return Object.freeze(value);
}
function parseWorkspace(value) {
  const input = workspaceObject(value, "Workspace response");
  const key = input["key"];
  if (key !== undefined && typeof key !== "string") {
    throw new Error("Workspace response.key must be a string");
  }
  const sandboxId = input["sandbox_id"];
  if (typeof sandboxId !== "string") {
    throw new Error("Workspace response.sandbox_id must be a string");
  }
  validateTaskId(sandboxId);
  const status = input["status"];
  if (status !== "available" && status !== "recovery_required" && status !== "deleting") {
    throw new Error("Workspace response.status is invalid");
  }
  if (!Array.isArray(input["secrets"])) {
    throw new Error("Workspace response.secrets must be an array");
  }
  return Object.freeze({
    id: resourceID(input["id"], "Workspace response.id"),
    ...key === undefined ? {} : { key },
    sandboxId,
    deploymentId: resourceID(input["deployment_id"], "Workspace response.deployment_id"),
    status,
    secrets: Object.freeze(input["secrets"].map(parseWorkspaceSecret)),
    lastActivityAt: workspaceTimestamp(input["last_activity_at"], "last_activity_at"),
    createdAt: workspaceTimestamp(input["created_at"], "created_at"),
    updatedAt: workspaceTimestamp(input["updated_at"], "updated_at")
  });
}
function parseWorkspaceSecret(value) {
  const input = workspaceObject(value, "Workspace Secret");
  if (typeof input["name"] !== "string") {
    throw new Error("Workspace Secret.name must be a string");
  }
  const hasEnv = typeof input["env"] === "string";
  const hasFile = typeof input["file"] === "string";
  if (hasEnv === hasFile) {
    throw new Error("Workspace Secret must contain exactly one placement");
  }
  return Object.freeze({
    name: input["name"],
    ...hasEnv ? { env: input["env"] } : { file: input["file"] }
  });
}
function parseWorkspaceFileEntry(value) {
  const input = workspaceObject(value, "Workspace file entry");
  if (typeof input["path"] !== "string" || typeof input["mode"] !== "number" || !Number.isInteger(input["mode"])) {
    throw new Error("Workspace file entry identity is invalid");
  }
  switch (input["kind"]) {
    case "file":
      if (typeof input["size_bytes"] !== "number" || !Number.isSafeInteger(input["size_bytes"])) {
        throw new Error("Workspace file entry.size_bytes must be an integer");
      }
      return Object.freeze({
        path: input["path"],
        kind: "file",
        mode: input["mode"],
        sizeBytes: input["size_bytes"]
      });
    case "directory":
      return Object.freeze({
        path: input["path"],
        kind: "directory",
        mode: input["mode"]
      });
    case "symlink":
      if (typeof input["link_target"] !== "string") {
        throw new Error("Workspace symlink entry.link_target must be a string");
      }
      return Object.freeze({
        path: input["path"],
        kind: "symlink",
        mode: input["mode"],
        linkTarget: input["link_target"]
      });
    default:
      throw new Error("Workspace file entry.kind is invalid");
  }
}
function parseWorkspaceFileContent(value) {
  const response = workspaceObject(value, "Workspace file response");
  return decodeWorkspaceBase64(response["data_base64"], "Workspace file response.data_base64");
}
function parseWorkspaceFilePage(value) {
  const response = workspaceObject(value, "Workspace file list response");
  if (!Array.isArray(response["items"])) {
    throw new Error("Workspace file list response.items must be an array");
  }
  const nextCursor = response["next_cursor"];
  if (nextCursor !== undefined && typeof nextCursor !== "string") {
    throw new Error("Workspace file list response.next_cursor must be a string");
  }
  return Object.freeze({
    items: Object.freeze(response["items"].map(parseWorkspaceFileEntry)),
    ...nextCursor === undefined ? {} : { nextCursor }
  });
}
function parseWorkspaceExecResult(value) {
  const response = workspaceObject(value, "Workspace exec response");
  const exitCode = response["exit_code"];
  if (!Number.isSafeInteger(exitCode)) {
    throw new Error("Workspace exec response.exit_code must be an integer");
  }
  return Object.freeze({
    exitCode,
    stdout: decodeWorkspaceBase64(response["stdout_base64"], "Workspace exec response.stdout_base64"),
    stderr: decodeWorkspaceBase64(response["stderr_base64"], "Workspace exec response.stderr_base64")
  });
}
function parseWorkspaceDeleteReceipt(value) {
  const response = workspaceObject(value, "Workspace delete response");
  return Object.freeze({
    workspaceId: resourceID(response["workspace_id"], "Workspace delete response.workspace_id")
  });
}
function workspaceObject(value, label) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value;
}
function workspaceTimestamp(value, field) {
  return timestampString(value, `Workspace response.${field}`);
}
function decodeWorkspaceBase64(value, label) {
  if (typeof value !== "string") {
    throw new Error(`${label} must be canonical padded base64`);
  }
  let binary;
  try {
    binary = atob(value);
  } catch {
    throw new Error(`${label} must be canonical padded base64`);
  }
  if (btoa(binary) !== value) {
    throw new Error(`${label} must be canonical padded base64`);
  }
  const output = new Uint8Array(binary.length);
  for (let index = 0;index < binary.length; index++) {
    output[index] = binary.charCodeAt(index);
  }
  return output;
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
// sdk/typescript/src/internal/session.ts
function parseSession(value) {
  const input = objectValue(value, "Session response");
  const status = sessionStatus(input["status"]);
  const failure = input["failure"] === undefined ? undefined : parseSessionFailure(input["failure"]);
  const terminalFailure = status === "cancelled" || status === "failed";
  if (terminalFailure !== (failure !== undefined)) {
    throw new Error("Session response has an inconsistent failure projection");
  }
  if (failure !== undefined && status === "cancelled" !== (failure.code === "cancelled")) {
    throw new Error("Session response failure code is inconsistent with status");
  }
  const actorId = requiredString(input, "actor_id", "Session response");
  validateTaskId(actorId);
  return Object.freeze({
    id: resourceID(input["id"], "Session response.id"),
    actorId,
    deploymentId: resourceID(input["deployment_id"], "Session response.deployment_id"),
    ...input["key"] === undefined ? {} : { key: requiredString(input, "key", "Session response") },
    status,
    createdAt: timestampString(input["created_at"], "Session response.created_at"),
    updatedAt: timestampString(input["updated_at"], "Session response.updated_at"),
    ...input["current_run_id"] === undefined ? {} : { currentRunId: resourceID(input["current_run_id"], "Session response.current_run_id") },
    ...failure === undefined ? {} : { failure }
  });
}
function parseSessionInputRecord(value) {
  const input = objectValue(value, "Session input response");
  if (!Object.hasOwn(input, "data")) {
    throw new Error("Session input response.data is required");
  }
  const data = input["data"];
  canonicalizeJsonValue(data);
  const source2 = objectValue(input["source"], "Session input response.source");
  const type = source2["type"];
  if (type !== "external" && type !== "run") {
    throw new Error("Session input response.source.type is invalid");
  }
  const parsedSource = type === "external" ? Object.freeze({ type }) : Object.freeze({
    type,
    runId: resourceID(source2["run_id"], "Session input response.source.run_id")
  });
  return Object.freeze({
    id: resourceID(input["id"], "Session input response.id"),
    sequence: safeSequence(input["sequence"], "Session input response.sequence"),
    data,
    source: parsedSource,
    createdAt: timestampString(input["created_at"], "Session input response.created_at")
  });
}
function parseSessionFailure(value) {
  const input = objectValue(value, "Session failure");
  const code = requiredString(input, "code", "Session failure");
  if (code !== "cancelled" && code !== "no_progress" && code !== "run_failed" && code !== "run_expired" && code !== "platform_failure") {
    throw new Error("Session failure.code is invalid");
  }
  const details = objectValue(input["details"], "Session failure.details");
  const runId = details["run_id"] === undefined ? undefined : resourceID(details["run_id"], "Session failure.details.run_id");
  return Object.freeze({
    code,
    message: requiredString(input, "message", "Session failure"),
    details: Object.freeze(runId === undefined ? {} : { runId })
  });
}
function sessionStatus(value) {
  if (value !== "open" && value !== "closed" && value !== "cancelled" && value !== "failed") {
    throw new Error("Session response.status is invalid");
  }
  return value;
}
function requiredString(value, field, label) {
  const result = value[field];
  if (typeof result !== "string" || result.length === 0) {
    throw new Error(`${label}.${field} must be a non-empty string`);
  }
  return result;
}
function objectValue(value, label) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${label} must be an object`);
  }
  return value;
}
function safeSequence(value, label) {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${label} must be a non-negative safe integer`);
  }
  return value;
}
// sdk/typescript/src/internal/strings.ts
var goSpaceEdges = /^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/gu;
function trimGoSpace(value) {
  return value.replace(goSpaceEdges, "");
}
// runtime/typescript/src/program.ts
import { createWriteStream, promises as fs } from "node:fs";
import { randomBytes } from "node:crypto";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
var MAX_PROGRAM_FRAME_BYTES = 256 * 1024 * 1024;
var MAX_TASK_OUTPUT_BYTES = 16 * 1024 * 1024;
var MAX_TASK_ERROR_BYTES = 16 * 1024;
var MAX_RUN_LOG_MESSAGE_BYTES = 4 * 1024;
var MAX_RUN_LOG_ATTRIBUTES_BYTES = 16 * 1024;
var MAX_TASK_ERROR_MESSAGE_BYTES = 1024;
var MAX_ACTOR_INPUT_BYTES = 1 * 1024 * 1024;
function newUUIDv7() {
  const bytes = randomBytes(16);
  let timestamp = Date.now();
  for (let index = 5;index >= 0; index -= 1) {
    bytes[index] = timestamp & 255;
    timestamp = Math.floor(timestamp / 256);
  }
  bytes[6] = bytes[6] & 15 | 112;
  bytes[8] = bytes[8] & 63 | 128;
  const hex = bytes.toString("hex");
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

class FrameReader {
  #iterator;
  #chunk = new Uint8Array;
  #offset = 0;
  constructor(input) {
    this.#iterator = input[Symbol.asyncIterator]();
  }
  async read(maxBytes = MAX_PROGRAM_FRAME_BYTES) {
    const header = await this.#readExact(4);
    const size = new DataView(header.buffer, header.byteOffset, header.byteLength).getUint32(0);
    if (size > maxBytes) {
      throw new Error(`runtime frame length ${size} exceeds max ${maxBytes}`);
    }
    return this.#readExact(size);
  }
  async#readExact(size) {
    const result = new Uint8Array(size);
    let written = 0;
    while (written < size) {
      if (this.#offset === this.#chunk.byteLength) {
        const next = await this.#iterator.next();
        if (next.done) {
          throw new Error(`runtime frame ended after ${written} of ${size} bytes`);
        }
        this.#chunk = typeof next.value === "string" ? new TextEncoder().encode(next.value) : next.value;
        this.#offset = 0;
        if (this.#chunk.byteLength === 0)
          continue;
      }
      const count = Math.min(size - written, this.#chunk.byteLength - this.#offset);
      result.set(this.#chunk.subarray(this.#offset, this.#offset + count), written);
      this.#offset += count;
      written += count;
    }
    return result;
  }
}

class ResumeDecisionRouter {
  #reader;
  #pending = new Map;
  #reading = false;
  constructor(reader) {
    this.#reader = reader;
  }
  register(correlationId) {
    if (this.#pending.has(correlationId)) {
      return Promise.reject(new Error("duplicate runtime correlation id"));
    }
    const result = new Promise((resolve, reject) => {
      this.#pending.set(correlationId, { resolve, reject });
    });
    this.#pump();
    return result;
  }
  cancel(correlationId) {
    this.#pending.delete(correlationId);
  }
  abandonPending() {
    this.#pending.clear();
  }
  #pump() {
    if (this.#reading)
      return;
    this.#reading = true;
    (async () => {
      try {
        while (this.#pending.size > 0) {
          const decision = fromBinary(exports_run_pb.ResumeDecisionSchema, await this.#reader.read());
          const pending = this.#pending.get(decision.correlationId);
          if (pending === undefined) {
            throw new Error("resume decision did not match a pending runtime operation");
          }
          this.#pending.delete(decision.correlationId);
          pending.resolve(decision);
        }
      } catch (error) {
        const failure = error instanceof Error ? error : new Error(String(error));
        for (const pending of this.#pending.values())
          pending.reject(failure);
        this.#pending.clear();
      } finally {
        this.#reading = false;
        if (this.#pending.size > 0)
          this.#pump();
      }
    })();
  }
}

class RuntimeProtocolError extends Error {
  constructor(message, options) {
    super(message, options);
    this.name = "RuntimeProtocolError";
  }
}

class ActorCancellationError extends Error {
  code;
  constructor(reasonCode) {
    super(`Actor execution was cancelled: ${reasonCode}`);
    this.name = "AbortError";
    this.code = reasonCode;
  }
}

class RunOperationState {
  controller = new AbortController;
  #active = 0;
  #drainable = new Set;
  #protocolFault;
  track(operation) {
    this.#active++;
    const result = (async () => {
      try {
        return await operation();
      } catch (error) {
        if (error instanceof RuntimeProtocolError && this.#protocolFault === undefined) {
          this.#protocolFault = error;
        }
        throw error;
      } finally {
        this.#active--;
      }
    })();
    result.catch(() => {});
    return result;
  }
  trackDrainable(operation) {
    const result = this.track(operation);
    this.#drainable.add(result);
    result.finally(() => {
      this.#drainable.delete(result);
    }).catch(() => {});
    return result;
  }
  async drainForCompletion() {
    while (this.#drainable.size !== 0) {
      await Promise.allSettled([...this.#drainable]);
    }
  }
  cancel(reasonCode) {
    const error = new ActorCancellationError(reasonCode);
    if (!this.controller.signal.aborted)
      this.controller.abort(error);
    return this.controller.signal.reason;
  }
  assertCanComplete() {
    if (this.#protocolFault !== undefined)
      throw this.#protocolFault;
    if (this.controller.signal.aborted) {
      throw this.controller.signal.reason;
    }
    if (this.#active !== 0) {
      throw new RuntimeProtocolError("Run handler returned with runtime operations still pending");
    }
  }
}

class ConsumingWaitGate {
  #pending = false;
  acquire(error = () => new Error("only one consuming Wait may be pending")) {
    if (this.#pending)
      throw error();
    this.#pending = true;
    let released = false;
    return () => {
      if (released)
        return;
      released = true;
      this.#pending = false;
    };
  }
}
async function requestRuntimeDecision(io, decisions, correlationId, event) {
  const pending = decisions.register(correlationId);
  try {
    await writeRunEvent(io, event);
  } catch (error) {
    decisions.cancel(correlationId);
    throw new RuntimeProtocolError("failed to write runtime operation request", {
      cause: error
    });
  }
  try {
    return await pending;
  } catch (error) {
    throw new RuntimeProtocolError("failed to read runtime operation decision", {
      cause: error
    });
  }
}
function requireWaitDecision(decision, correlationId, runWaitId, resumeAttachId, operation) {
  if (decision.correlationId !== correlationId || decision.runWaitId !== runWaitId || decision.resumeAttachId !== resumeAttachId || decision.kind !== "completed" && decision.kind !== "failed" && decision.kind !== "cancelled") {
    throw new RuntimeProtocolError(`${operation} decision did not match the pending Wait`);
  }
}
async function writeRuntimeProtocolEvent(io, event) {
  try {
    await writeRunEvent(io, event);
  } catch (error) {
    throw new RuntimeProtocolError("failed to write runtime protocol event", {
      cause: error
    });
  }
}
function parseRuntimeProtocolValue(label, parse) {
  try {
    return parse();
  } catch (error) {
    if (error instanceof RuntimeProtocolError)
      throw error;
    throw new RuntimeProtocolError(`${label} was invalid`, { cause: error });
  }
}
async function runProgram(locatorURL, io = defaultProgramIO()) {
  const reader = new FrameReader(io.input);
  const start = fromBinary(exports_run_pb.ProgramStartSchema, await reader.read());
  validateProgramStart(start);
  const index = await loadProgramIndex(locatorURL, io);
  const kind = start.entrypoint.case;
  if (kind !== "task" && kind !== "actor") {
    throw new Error("Program-start entrypoint is required");
  }
  const located = index.declarations.filter((declaration2) => declaration2.kind === kind && declaration2.declaredId === start.entrypointDeclaredId && declaration2.locator !== undefined);
  if (located.length !== 1) {
    throw new Error(`Program declaration ${kind}:${JSON.stringify(start.entrypointDeclaredId)} was not found exactly once`);
  }
  const declaration = located[0];
  const locator = declaration.locator;
  const moduleURL = resolveModuleURL(locatorURL, locator.modulePath);
  const imported = io.importModule === undefined ? await import(moduleURL.href) : await io.importModule(moduleURL);
  const definition = inspectDefinition(imported[locator.exportName]);
  if (definition === undefined || definition.kind !== declaration.kind || definition.id !== declaration.declaredId || definition.kind !== "task" && definition.kind !== "actor") {
    throw new Error(`Program export ${JSON.stringify(locator.exportName)} does not match ${kind}:${JSON.stringify(start.entrypointDeclaredId)}`);
  }
  validateEntrypointContract(start, definition);
  const identity = entrypointIdentity(kind, start.entrypointDeclaredId);
  await writeRunEvent(io, {
    case: "entrypointReady",
    value: create(exports_run_pb.EntrypointReadySchema, {
      runId: start.runId,
      attemptNumber: start.attemptNumber,
      entrypoint: identity
    })
  });
  const release = fromBinary(exports_run_pb.EntrypointReleaseSchema, await reader.read());
  validateEntrypointRelease(release, start, kind);
  const decisions = new ResumeDecisionRouter(reader);
  if (definition.kind === "task") {
    await runTask(start, definition, io, decisions);
    return;
  }
  await runActor(start, definition, io, decisions);
}
async function loadProgramIndex(url, io) {
  const raw = io.readLocator === undefined ? await fs.readFile(url, "utf8") : await io.readLocator(url);
  const value = JSON.parse(raw);
  if (typeof value !== "object" || value === null) {
    throw new Error("Program index must be an object");
  }
  const record = value;
  if (record["architecture"] !== "x86_64" || record["runtimeContract"] !== "helmr.runtime.v0" || typeof record["configResultDigest"] !== "string" || !Array.isArray(record["queues"]) || !Array.isArray(record["declarations"]) || record["declarations"].length === 0) {
    throw new Error("Program index has an invalid v0 shape");
  }
  const declarations = record["declarations"].map((entry, index) => parseProgramIndexDeclaration(entry, index));
  return { declarations };
}
function parseProgramIndexDeclaration(value, index) {
  if (typeof value !== "object" || value === null) {
    throw new Error(`Program index declaration ${index} must be an object`);
  }
  const record = value;
  if (record["kind"] !== "task" && record["kind"] !== "actor" && record["kind"] !== "sandbox" || typeof record["declaredId"] !== "string" || record["declaredId"] === "" || typeof record["manifest"] !== "object" || record["manifest"] === null) {
    throw new Error(`Program index declaration ${index} is invalid`);
  }
  if (record["kind"] === "sandbox") {
    if (record["locator"] !== undefined) {
      throw new Error(`Program index Sandbox declaration ${index} has a locator`);
    }
    return {
      kind: "sandbox",
      declaredId: record["declaredId"]
    };
  }
  const locator = record["locator"];
  if (typeof locator !== "object" || locator === null) {
    throw new Error(`Program index declaration ${index} has no locator`);
  }
  const located = locator;
  if (typeof located["exportName"] !== "string" || located["exportName"] === "" || typeof located["modulePath"] !== "string" || located["slot"] !== "handler") {
    throw new Error(`Program index declaration ${index} locator is invalid`);
  }
  return {
    kind: record["kind"],
    declaredId: record["declaredId"],
    locator: {
      exportName: located["exportName"],
      modulePath: validateModulePath(located["modulePath"]),
      slot: "handler"
    }
  };
}
function validateModulePath(value) {
  const components = value.split("/");
  const prefix = components.slice(0, -3);
  if (components.length < 3 || components.at(-3) !== ".helmr" || components.at(-2) !== "modules" || !/^[0-9a-f]{64}\.mjs$/.test(components.at(-1) ?? "") || prefix.some((component) => component === "" || component === "." || component === ".." || component === ".helmr" || component.includes("\\") || /[\u0000-\u001f\u007f]/.test(component))) {
    throw new Error("declaration modulePath is not a generated Program module");
  }
  return value;
}
function resolveModuleURL(locatorURL, modulePath) {
  const root = path.dirname(path.dirname(fileURLToPath(locatorURL)));
  const resolved = path.resolve(root, modulePath);
  const relative = path.relative(root, resolved);
  if (relative === "" || relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)) {
    throw new Error("declaration modulePath escapes the Program root");
  }
  return pathToFileURL(resolved);
}
function validateProgramStart(start) {
  if (start.runId === "" || start.attemptNumber === 0 || start.entrypointDeclaredId === "" || start.deploymentId === "" || start.deploymentVersion === "" || start.workspaceId === "" || start.baseWorkspaceVersionId === "" || start.cause === undefined || start.cause.kind.case === undefined) {
    throw new Error("Program-start frame is missing required logical fields");
  }
}
function validateEntrypointContract(start, definition) {
  if (definition.kind === "actor") {
    if (start.entrypoint.case !== "actor" || start.entrypoint.value.startInputSequence < 0n || start.entrypoint.value.inputHighWatermark < start.entrypoint.value.startInputSequence) {
      throw new Error("Program-start Actor cursor authority is invalid");
    }
    return;
  }
  const payload = start.entrypoint.case === "task" ? start.entrypoint.value.payload.case : undefined;
  if (definition.hasPayload && payload !== "payloadJson" || !definition.hasPayload && payload !== "noPayload") {
    throw new Error(`Program-start payload presence does not match task ${JSON.stringify(definition.id)}`);
  }
}
function entrypointIdentity(kind, declaredId) {
  return create(exports_run_pb.EntrypointIdentitySchema, {
    declaredId,
    kind: kind === "task" ? {
      case: "task",
      value: create(exports_run_pb.TaskEntrypointSchema)
    } : {
      case: "actor",
      value: create(exports_run_pb.ActorEntrypointSchema)
    }
  });
}
function validateEntrypointRelease(release, start, kind) {
  if (release.runId !== start.runId || release.attemptNumber !== start.attemptNumber || release.entrypoint?.declaredId !== start.entrypointDeclaredId || release.entrypoint.kind.case !== kind) {
    throw new Error("entrypoint release does not match Program-start identity");
  }
}
async function runTask(start, definition, io, decisions) {
  let payload;
  if (definition.hasPayload) {
    let failureDetails;
    try {
      if (start.entrypoint.case !== "task" || start.entrypoint.value.payload.case !== "payloadJson") {
        throw new Error("task payload is missing");
      }
      payload = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(start.entrypoint.value.payload.value));
      const parsed = await definition.payloadSchema["~standard"].validate(payload);
      if ("issues" in parsed && parsed.issues !== undefined) {
        failureDetails = validationDetails(parsed.issues);
      } else {
        payload = parsed.value;
      }
    } catch (error) {
      failureDetails = {
        message: boundedUtf8(errorMessage(error), 2048)
      };
    }
    if (failureDetails !== undefined) {
      await writeTaskFailure(io, "payload_invalid", "task payload failed validation", failureDetails);
      return;
    }
  }
  const context = taskContext(start);
  const runOperations = new RunOperationState;
  const uninstallRuntime = installRuntimeOperations(programRuntimeOperations(start, io, decisions, new ConsumingWaitGate, runOperations));
  let normalized;
  try {
    let output;
    if (definition.hasPayload) {
      output = await definition.handler(payload, context);
    } else {
      output = await definition.handler(context);
    }
    await runOperations.drainForCompletion();
    runOperations.assertCanComplete();
    normalized = canonicalizeJsonValue(output);
    if (normalized.byteLength > MAX_TASK_OUTPUT_BYTES) {
      throw new Error(`task output exceeds ${MAX_TASK_OUTPUT_BYTES} bytes`);
    }
  } catch (error) {
    if (error instanceof RuntimeProtocolError)
      throw error;
    await runOperations.drainForCompletion();
    runOperations.assertCanComplete();
    await writeTaskFailure(io, "failed", errorMessage(error));
    return;
  } finally {
    uninstallRuntime();
  }
  await writeRunEvent(io, {
    case: "taskOutcome",
    value: create(exports_run_pb.TaskOutcomeSchema, {
      outcome: {
        case: "succeeded",
        value: create(exports_run_pb.TaskSucceededSchema, {
          outputJson: new TextDecoder().decode(normalized)
        })
      }
    })
  });
}
function programRuntimeOperations(start, io, decisions, waitGate, runOperations, actorCursor) {
  const performTaskStart = async (target, payload, options) => {
    if (options.signal?.aborted)
      throw abortSignalReason(options.signal);
    const idempotencyKey = options.idempotencyKey === "" ? undefined : options.idempotencyKey;
    if (idempotencyKey === undefined && process.env["NODE_ENV"] !== "production") {
      process.emitWarning(`Task "${target.declaredId}" was started without an idempotencyKey; retrying the parent Run may create another child Run.`, { code: "HELMR_KEYLESS_CHILD_TASK_START" });
    }
    const correlationId = newUUIDv7();
    const payloadJson = target.payloadPresent ? new TextDecoder().decode(canonicalizeJsonValue(payload)) : undefined;
    const workspaceJson = new TextDecoder().decode(canonicalizeJsonValue({ id: workspaceRefID(options.workspace) }));
    const requestOptions = {
      ...options.queue === undefined ? {} : { queue: options.queue },
      ...options.concurrencyKey === undefined ? {} : { concurrency_key: options.concurrencyKey },
      ...options.priority === undefined ? {} : { priority: options.priority },
      ...options.ttl === undefined ? {} : { ttl: options.ttl },
      ...options.retry === undefined ? {} : { retry: taskRetryRequest(options.retry) },
      ...options.metadata === undefined ? {} : { metadata: options.metadata },
      ...options.tags === undefined ? {} : { tags: [...options.tags] }
    };
    const optionsJson = new TextDecoder().decode(canonicalizeJsonValue(requestOptions));
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "taskChildInvokeRequested",
        value: create(exports_run_pb.TaskChildInvokeRequestedSchema, {
          correlationId,
          declaredId: target.declaredId,
          method: "start",
          payloadPresent: target.payloadPresent,
          ...payloadJson === undefined ? {} : { payloadJson },
          workspaceJson,
          optionsJson,
          ...idempotencyKey === undefined ? {} : { idempotencyKey }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Task child start");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Task child start", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Task child start result", () => {
        const value = JSON.parse(decision.dataJson);
        if (typeof value !== "object" || value === null || Array.isArray(value)) {
          throw new Error("result must be an object");
        }
        const keys = Object.keys(value);
        if (keys.length !== 1 || keys[0] !== "run_id") {
          throw new Error("result fields are invalid");
        }
        const id = resourceID(value["run_id"], "Task child start result.run_id");
        return createRunHandle(id);
      });
    });
    return await abortableRuntimeOperation(operation, options.signal);
  };
  const performTaskCall = (target, payload, options) => {
    if (options.signal?.aborted) {
      return Promise.reject(abortSignalReason(options.signal));
    }
    const operation = runOperations.track(async () => {
      const releaseWait = waitGate.acquire();
      try {
        const correlationId = newUUIDv7();
        const runWaitId = newUUIDv7();
        const resumeAttachId = newUUIDv7();
        const payloadJson = target.payloadPresent ? new TextDecoder().decode(canonicalizeJsonValue(payload)) : undefined;
        const workspaceJson = new TextDecoder().decode(canonicalizeJsonValue({ id: workspaceRefID(options.workspace) }));
        const requestOptions = {
          ...options.queue === undefined ? {} : { queue: options.queue },
          ...options.concurrencyKey === undefined ? {} : { concurrency_key: options.concurrencyKey },
          ...options.priority === undefined ? {} : { priority: options.priority },
          ...options.ttl === undefined ? {} : { ttl: options.ttl },
          ...options.retry === undefined ? {} : { retry: taskRetryRequest(options.retry) },
          ...options.metadata === undefined ? {} : { metadata: options.metadata },
          ...options.tags === undefined ? {} : { tags: [...options.tags] }
        };
        const decision = await requestRuntimeDecision(io, decisions, correlationId, {
          case: "taskChildInvokeRequested",
          value: create(exports_run_pb.TaskChildInvokeRequestedSchema, {
            correlationId,
            runWaitId,
            resumeAttachId,
            declaredId: target.declaredId,
            method: "call",
            payloadPresent: target.payloadPresent,
            ...payloadJson === undefined ? {} : { payloadJson },
            workspaceJson,
            optionsJson: new TextDecoder().decode(canonicalizeJsonValue(requestOptions)),
            idempotencyKey: options.idempotencyKey,
            ...actorCursor === undefined ? {} : { actorSpeculativeInputSequence: actorCursor.value }
          })
        });
        requireWaitDecision(decision, correlationId, runWaitId, resumeAttachId, "Task child call");
        if (decision.requireConsumedAck) {
          await writeRuntimeProtocolEvent(io, {
            case: "resumeConsumed",
            value: create(exports_run_pb.ResumeConsumedSchema, {
              runWaitId: decision.runWaitId,
              checkpointId: decision.checkpointId,
              resumeAttachId: decision.resumeAttachId,
              resumeRequestVersion: decision.resumeRequestVersion,
              runLeaseId: decision.runLeaseId,
              correlationId: decision.correlationId
            })
          });
        }
        if (decision.kind === "cancelled" && actorCursor !== undefined) {
          const failure = parseRuntimeProtocolValue("Actor child Task cancellation decision", () => resumeFailure(decision.dataJson));
          throw runOperations.cancel(failure.reasonCode);
        }
        if (decision.kind !== "completed") {
          throw decision.kind === "failed" ? runtimeOperationFailure("Task child call", decision.dataJson) : new RuntimeProtocolError(`Task child call was cancelled: ${resumeFailure(decision.dataJson).reasonCode}`);
        }
        return parseRuntimeProtocolValue("Task child call result", () => parseTaskResult(decision.dataJson));
      } finally {
        releaseWait();
      }
    });
    return abortableRuntimeOperation(operation, options.signal);
  };
  const performWait = async (params, timeoutMs) => {
    const releaseWait = waitGate.acquire();
    const correlationId = newUUIDv7();
    const runWaitId = newUUIDv7();
    const resumeAttachId = newUUIDv7();
    try {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(exports_run_pb.RunWaitRequestedSchema, {
          correlationId,
          runWaitId,
          resumeAttachId,
          kind: "timer",
          paramsJson: new TextDecoder().decode(canonicalizeJsonValue(params)),
          timeoutMs: BigInt(timeoutMs),
          ...actorCursor === undefined ? {} : { actorSpeculativeInputSequence: actorCursor.value }
        })
      });
      requireWaitDecision(decision, correlationId, runWaitId, resumeAttachId, "timer resume");
      if (decision.requireConsumedAck) {
        await writeRuntimeProtocolEvent(io, {
          case: "resumeConsumed",
          value: create(exports_run_pb.ResumeConsumedSchema, {
            runWaitId: decision.runWaitId,
            checkpointId: decision.checkpointId,
            resumeAttachId: decision.resumeAttachId,
            resumeRequestVersion: decision.resumeRequestVersion,
            runLeaseId: decision.runLeaseId,
            correlationId: decision.correlationId
          })
        });
      }
      if (decision.kind === "cancelled" && actorCursor !== undefined) {
        const failure = parseRuntimeProtocolValue("Actor timer cancellation decision", () => resumeFailure(decision.dataJson));
        throw runOperations.cancel(failure.reasonCode);
      }
      if (decision.kind !== "completed") {
        const failure = parseRuntimeProtocolValue("timer Wait failure decision", () => resumeFailure(decision.dataJson));
        throw new RuntimeProtocolError(`timer Wait ${decision.kind}: ${failure.reasonCode}`);
      }
    } finally {
      releaseWait();
    }
  };
  const wait = (params, timeoutMs) => runOperations.track(() => performWait(params, timeoutMs));
  const performActorInputSend = async (sessionId, input, request, signal) => {
    if (signal?.aborted) {
      throw abortSignalReason(signal);
    }
    const idempotencyKey = normalizeActorInputIdempotencyKey(request?.idempotencyKey);
    const normalized = canonicalizeJsonValue(input);
    if (normalized.byteLength > MAX_ACTOR_INPUT_BYTES) {
      throw actorInputSendError("actor_input_too_large", `Actor input exceeds ${MAX_ACTOR_INPUT_BYTES} bytes`);
    }
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "sessionInputSendRequested",
        value: create(exports_run_pb.SessionInputSendRequestedSchema, {
          correlationId,
          sessionId,
          dataJson: new TextDecoder().decode(normalized),
          ...idempotencyKey === undefined ? {} : { idempotencyKey }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Actor input send");
      if (decision.kind === "failed") {
        throw parseRuntimeProtocolValue("Actor input send failure", () => parseActorInputSendFailure(decision.dataJson));
      }
      return parseRuntimeProtocolValue("Actor input send result", () => parseActorInputSendResult(decision.dataJson));
    });
    return await abortableRuntimeOperation(operation, signal);
  };
  const performActorStart = async (declaredId, options) => {
    if (options.signal?.aborted)
      throw abortSignalReason(options.signal);
    const correlationId = newUUIDv7();
    const run = options.run;
    const runOptions = {
      ...run?.queue === undefined ? {} : { queue: run.queue },
      ...run?.concurrencyKey === undefined ? {} : { concurrency_key: run.concurrencyKey },
      ...run?.priority === undefined ? {} : { priority: run.priority },
      ...run?.ttl === undefined ? {} : { ttl: run.ttl },
      ...run?.retry === undefined ? {} : { retry: taskRetryRequest(run.retry) },
      ...run?.metadata === undefined ? {} : { metadata: run.metadata },
      ...run?.tags === undefined ? {} : { tags: [...run.tags] }
    };
    const inputPresent = Object.hasOwn(options, "input");
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "actorStartRequested",
        value: create(exports_run_pb.ActorStartRequestedSchema, {
          correlationId,
          declaredId,
          workspaceId: workspaceRefID(options.workspace),
          ...options.key === undefined ? {} : { key: options.key },
          ...inputPresent ? {
            inputJson: new TextDecoder().decode(canonicalizeJsonValue(options.input))
          } : {},
          ...options.idempotencyKey === undefined ? {} : { idempotencyKey: options.idempotencyKey },
          runOptionsJson: new TextDecoder().decode(canonicalizeJsonValue(runOptions))
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Actor start");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Actor start", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Actor start result", () => {
        const value = parseObjectJSON(decision.dataJson, "Actor start result");
        requireExactKeys(value, ["run_id", "session_id"], "Actor start result");
        const sessionId = resourceID(stringField(value, "session_id", "Actor start result"), "Actor start result.session_id");
        const runId = resourceID(stringField(value, "run_id", "Actor start result"), "Actor start result.run_id");
        return Object.freeze({ sessionId, runId });
      });
    });
    return abortableRuntimeOperation(operation, options.signal);
  };
  const performSessionStatus = async (sessionId, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "sessionStatusRequested",
        value: create(exports_run_pb.SessionStatusRequestedSchema, {
          correlationId,
          sessionId
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Session retrieve");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Session retrieve", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Session retrieve result", () => parseRuntimeSession(decision.dataJson));
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performSessionClose = async (sessionId, request, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "sessionCloseRequested",
        value: create(exports_run_pb.SessionCloseRequestedSchema, {
          correlationId,
          sessionId,
          ...request?.idempotencyKey === undefined ? {} : { idempotencyKey: request.idempotencyKey }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Actor close");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Actor close", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Actor close result", () => {
        const value = parseObjectJSON(decision.dataJson, "Actor close result");
        requireExactKeys(value, ["accepted_at", "session_id"], "Actor close result");
        const sessionId2 = resourceID(stringField(value, "session_id", "Actor close result"), "Actor close result.session_id");
        const acceptedAt = stringField(value, "accepted_at", "Actor close result");
        return Object.freeze({
          sessionId: sessionId2,
          acceptedAt: timestampString(acceptedAt, "Session close result.accepted_at")
        });
      });
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performSessionOutputPage = async (sessionId, query, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    if (query?.after !== undefined && (!Number.isSafeInteger(query.after) || query.after < 0)) {
      throw new Error("Session output after must be a non-negative safe integer");
    }
    if (query?.limit !== undefined && (!Number.isInteger(query.limit) || query.limit < 1 || query.limit > 100)) {
      throw new Error("Session output limit must be an integer in [1,100]");
    }
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "sessionOutputPageRequested",
        value: create(exports_run_pb.SessionOutputPageRequestedSchema, {
          correlationId,
          sessionId,
          ...query?.after === undefined ? {} : { after: BigInt(query.after) },
          limit: query?.limit ?? 50
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Actor output page");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Actor output page", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Actor output page result", () => {
        const value = parseObjectJSON(decision.dataJson, "Actor output page result");
        requireExactKeys(value, ["has_more", "next_after", "records"], "Actor output page result");
        const records = value["records"];
        if (!Array.isArray(records)) {
          throw new Error("Actor output page result.records must be an array");
        }
        const hasMore = value["has_more"];
        if (typeof hasMore !== "boolean") {
          throw new Error("Actor output page result.has_more must be a boolean");
        }
        const nextAfter = safeJSONSequence(value["next_after"], "Actor output page result.next_after");
        return Object.freeze({
          records: Object.freeze(records.map((record) => parseSessionOutputRecord2(JSON.stringify(record)))),
          nextAfter,
          hasMore
        });
      });
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const workspaceAddress = (workspaceId) => create(exports_run_pb.WorkspaceAddressSchema, { workspaceId });
  const performWorkspaceCreate = async (declaredId, request = {}, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceCreateRequested",
        value: create(exports_run_pb.WorkspaceCreateRequestedSchema, {
          correlationId,
          declaredId,
          ...request.key === undefined ? {} : { key: request.key },
          secrets: encodeWorkspaceSecrets(request.secrets).map((secret) => create(exports_run_pb.WorkspaceSecretPlacementSchema, {
            name: secret.name,
            placement: "env" in secret ? { case: "env", value: secret.env } : { case: "file", value: secret.file }
          })) ?? [],
          ...request.idempotencyKey === undefined ? {} : { idempotencyKey: request.idempotencyKey }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace create");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace create", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Workspace create result", () => {
        const value = parseObjectJSON(decision.dataJson, "Workspace create result");
        requireExactKeys(value, ["workspace_id"], "Workspace create result");
        const workspaceId = resourceID(stringField(value, "workspace_id", "Workspace create result"), "Workspace create result.workspace_id");
        return Object.freeze({ workspaceId });
      });
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceRetrieve = async (workspaceId, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceRetrieveRequested",
        value: create(exports_run_pb.WorkspaceRetrieveRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId)
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace retrieve");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace retrieve", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Workspace retrieve result", () => parseWorkspace(JSON.parse(decision.dataJson)));
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceFileRead = async (workspaceId, filePath, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceFileReadRequested",
        value: create(exports_run_pb.WorkspaceFileReadRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
          path: filePath
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace file read");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace file read", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Workspace file read result", () => parseWorkspaceFileContent(JSON.parse(decision.dataJson)));
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceFileStat = async (workspaceId, filePath, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceFileStatRequested",
        value: create(exports_run_pb.WorkspaceFileStatRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
          path: filePath
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace file stat");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace file stat", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Workspace file stat result", () => parseWorkspaceFileEntry(JSON.parse(decision.dataJson)));
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceFileList = async (workspaceId, filePath, query = {}, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const limit = query.limit ?? 50;
    if (!Number.isInteger(limit) || limit < 1 || limit > 100) {
      throw new Error("Workspace file list limit must be an integer between 1 and 100");
    }
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceFileListRequested",
        value: create(exports_run_pb.WorkspaceFileListRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
          path: filePath,
          ...query.cursor === undefined ? {} : { cursor: query.cursor },
          limit
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace file list");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace file list", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Workspace file list result", () => parseWorkspaceFilePage(JSON.parse(decision.dataJson)));
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceExec = async (workspaceId, request, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const timeoutMs = request.timeout === undefined ? undefined : durationMilliseconds(request.timeout, "Workspace exec timeout");
    if (timeoutMs !== undefined && timeoutMs > 900000) {
      throw new Error("Workspace exec timeout must not exceed 15m");
    }
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceExecRequested",
        value: create(exports_run_pb.WorkspaceExecRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
          command: [...request.command],
          ...request.cwd === undefined ? {} : { cwd: request.cwd },
          env: request.env === undefined ? {} : { ...request.env },
          stdin: request.stdin === undefined ? new Uint8Array : new Uint8Array(request.stdin),
          ...timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) },
          idempotencyKey: request.idempotencyKey
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace exec");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace exec", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Workspace exec result", () => parseWorkspaceExecResult(JSON.parse(decision.dataJson)));
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceDelete = async (workspaceId, request = {}, signal) => {
    if (signal?.aborted)
      throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceDeleteRequested",
        value: create(exports_run_pb.WorkspaceDeleteRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
          ...request.idempotencyKey === undefined ? {} : { idempotencyKey: request.idempotencyKey }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace delete");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace delete", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Workspace delete result", () => parseWorkspaceDeleteReceipt(JSON.parse(decision.dataJson)));
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performTokenCreate = async (request) => {
    const correlationId = newUUIDv7();
    const timeoutMs = request.timeout === undefined ? undefined : durationMilliseconds(request.timeout, "Token timeout");
    const metadataJson = request.metadata === undefined ? undefined : new TextDecoder().decode(canonicalizeJsonValue(request.metadata));
    const idempotencyKey = normalizeTokenIdempotencyKey(request.idempotencyKey);
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "tokenCreateRequested",
        value: create(exports_run_pb.TokenCreateRequestedSchema, {
          correlationId,
          ...timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) },
          ...idempotencyKey === undefined ? {} : { idempotencyKey },
          tags: request.tags === undefined ? [] : [...request.tags],
          ...metadataJson === undefined ? {} : { metadataJson }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Token create");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Token create", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Token create result", () => parseTokenCreateResult(decision.dataJson));
    });
    return await operation;
  };
  const performTokenWait = async (tokenId, options) => {
    const releaseWait = waitGate.acquire();
    const correlationId = newUUIDv7();
    const runWaitId = newUUIDv7();
    const resumeAttachId = newUUIDv7();
    const timeoutMs = options.timeout === undefined ? undefined : durationMilliseconds(options.timeout, "Token Wait timeout");
    const idleTimeoutMs = options.idleTimeout === undefined ? undefined : tokenWaitIdleTimeoutMilliseconds(options.idleTimeout);
    try {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(exports_run_pb.RunWaitRequestedSchema, {
          correlationId,
          runWaitId,
          resumeAttachId,
          kind: "token",
          paramsJson: JSON.stringify({ token_id: tokenId }),
          ...options.metadata === undefined ? {} : { metadataJson: new TextDecoder().decode(canonicalizeJsonValue(options.metadata)) },
          ...timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) },
          ...idleTimeoutMs === undefined ? {} : { idleTimeoutMs: BigInt(idleTimeoutMs) },
          tags: options.tags === undefined ? [] : [...options.tags],
          ...actorCursor === undefined ? {} : { actorSpeculativeInputSequence: actorCursor.value }
        })
      });
      requireWaitDecision(decision, correlationId, runWaitId, resumeAttachId, "Token resume");
      if (decision.requireConsumedAck) {
        await writeRuntimeProtocolEvent(io, {
          case: "resumeConsumed",
          value: create(exports_run_pb.ResumeConsumedSchema, {
            runWaitId: decision.runWaitId,
            checkpointId: decision.checkpointId,
            resumeAttachId: decision.resumeAttachId,
            resumeRequestVersion: decision.resumeRequestVersion,
            runLeaseId: decision.runLeaseId,
            correlationId: decision.correlationId
          })
        });
      }
      if (decision.kind === "cancelled" && actorCursor !== undefined) {
        const failure = parseRuntimeProtocolValue("Actor Token cancellation decision", () => resumeFailure(decision.dataJson));
        throw runOperations.cancel(failure.reasonCode);
      }
      if (decision.kind !== "completed") {
        if (decision.kind !== "failed" && decision.kind !== "cancelled") {
          throw new RuntimeProtocolError("Token resume decision kind was invalid");
        }
        throw tokenWaitFailure(decision.kind, decision.dataJson);
      }
      return parseRuntimeProtocolValue("Token completion result", () => JSON.parse(decision.dataJson));
    } finally {
      releaseWait();
    }
  };
  const performMetadataMutation = async (request) => {
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "metadataUpdated",
        value: create(exports_run_pb.MetadataUpdatedSchema, {
          correlationId,
          operation: request.operation,
          ...request.operation === "set" ? {
            key: normalizeMetadataKey(request.key),
            valueJson: new TextDecoder().decode(canonicalizeJsonValue(request.value))
          } : request.operation === "patch" ? {
            patchJson: new TextDecoder().decode(canonicalizeMetadataPatch(request.values))
          } : {
            key: normalizeMetadataKey(request.key),
            amount: finiteMetadataIncrement(request.amount)
          }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Metadata mutation");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Metadata mutation", decision.dataJson);
      }
    });
    await operation;
  };
  const performStructuredLog = async (level, message, attributes) => {
    if (level !== "debug" && level !== "info" && level !== "warn" && level !== "error") {
      throw new Error("logger level must be debug, info, warn, or error");
    }
    if (typeof message !== "string") {
      throw new Error("logger message must be a string");
    }
    if (new TextEncoder().encode(message).byteLength > MAX_RUN_LOG_MESSAGE_BYTES) {
      throw new Error(`logger message must be at most ${MAX_RUN_LOG_MESSAGE_BYTES} UTF-8 bytes`);
    }
    const attributesJson = canonicalizeLogAttributes(attributes);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "structuredLogRequested",
        value: create(exports_run_pb.StructuredLogRequestedSchema, {
          correlationId,
          level,
          message,
          attributesJson: new TextDecoder().decode(attributesJson)
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Structured log");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Structured log", decision.dataJson);
      }
    });
    await operation;
  };
  return {
    taskStart: performTaskStart,
    taskCall: performTaskCall,
    waitFor(duration) {
      return wait({ duration }, durationMilliseconds(duration));
    },
    waitUntil(date) {
      if (!(date instanceof Date) || Number.isNaN(date.getTime())) {
        return Promise.reject(new Error("timers.waitUntil() requires a valid Date"));
      }
      const remainingMs = date.getTime() - Date.now();
      if (remainingMs <= 0)
        return Promise.resolve();
      return wait({ date: date.toISOString() }, boundedTimerMilliseconds(Math.ceil(remainingMs)));
    },
    actorInputSend(target, input, request, signal) {
      return performActorInputSend(target, input, request, signal);
    },
    actorStart(declaredId, options) {
      return performActorStart(declaredId, options);
    },
    sessionRetrieve(sessionId, signal) {
      return performSessionStatus(sessionId, signal);
    },
    sessionClose(sessionId, request, signal) {
      return performSessionClose(sessionId, request, signal);
    },
    sessionOutputPage(sessionId, query, signal) {
      return performSessionOutputPage(sessionId, query, signal);
    },
    workspaceCreate(declaredId, request, signal) {
      return performWorkspaceCreate(declaredId, request, signal);
    },
    workspaceRetrieve(address, signal) {
      return performWorkspaceRetrieve(address, signal);
    },
    workspaceFileRead(address, path2, signal) {
      return performWorkspaceFileRead(address, path2, signal);
    },
    workspaceFileStat(address, path2, signal) {
      return performWorkspaceFileStat(address, path2, signal);
    },
    workspaceFileList(address, path2, query, signal) {
      return performWorkspaceFileList(address, path2, query, signal);
    },
    workspaceExec(address, request, signal) {
      return performWorkspaceExec(address, request, signal);
    },
    workspaceDelete(address, request, signal) {
      return performWorkspaceDelete(address, request, signal);
    },
    tokenCreate(options) {
      return performTokenCreate(options);
    },
    tokenWait(tokenId, options) {
      return runOperations.track(() => performTokenWait(tokenId, options));
    },
    metadataSet(key, value) {
      return performMetadataMutation({ operation: "set", key, value });
    },
    metadataPatch(values) {
      return performMetadataMutation({ operation: "patch", values });
    },
    metadataIncrement(key, amount) {
      return performMetadataMutation({ operation: "increment", key, amount });
    },
    structuredLog(level, message, attributes) {
      return performStructuredLog(level, message, attributes);
    }
  };
}
function normalizeMetadataKey(value) {
  if (typeof value !== "string" || value === "") {
    throw new Error("metadata key must be a nonempty string");
  }
  if (new TextEncoder().encode(value).byteLength > 512) {
    throw new Error("metadata key must be at most 512 UTF-8 bytes");
  }
  return value;
}
function canonicalizeMetadataPatch(values) {
  if (values === null || typeof values !== "object" || Array.isArray(values)) {
    throw new Error("metadata.patch() requires an object");
  }
  for (const key of Object.keys(values))
    normalizeMetadataKey(key);
  return canonicalizeJsonValue(values);
}
function finiteMetadataIncrement(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    throw new Error("metadata.increment() amount must be finite");
  }
  return value;
}
function canonicalizeLogAttributes(attributes) {
  if (attributes === null || typeof attributes !== "object" || Array.isArray(attributes)) {
    throw new Error("logger attributes must be an object");
  }
  const normalized = canonicalizeJsonValue(attributes);
  if (normalized.byteLength > MAX_RUN_LOG_ATTRIBUTES_BYTES) {
    throw new Error(`logger attributes must be at most ${MAX_RUN_LOG_ATTRIBUTES_BYTES} canonical JSON bytes`);
  }
  return normalized;
}
function normalizeTokenIdempotencyKey(value) {
  if (value === undefined)
    return;
  const normalized = trimGoSpace(value);
  if (new TextEncoder().encode(normalized).byteLength > 512) {
    throw new Error("Token idempotency key must be at most 512 UTF-8 bytes");
  }
  return normalized === "" ? undefined : normalized;
}
function taskRetryRequest(retry) {
  if (retry.enabled === false)
    return { enabled: false };
  return {
    ...retry.enabled === undefined ? {} : { enabled: retry.enabled },
    max_attempts: retry.maxAttempts,
    ...retry.backoff === undefined ? {} : {
      backoff: {
        ...retry.backoff.minDelay === undefined ? {} : { min_delay: retry.backoff.minDelay },
        ...retry.backoff.maxDelay === undefined ? {} : { max_delay: retry.backoff.maxDelay },
        ...retry.backoff.factor === undefined ? {} : { factor: retry.backoff.factor },
        ...retry.backoff.jitter === undefined ? {} : { jitter: retry.backoff.jitter }
      }
    }
  };
}
function parseTokenCreateResult(dataJson) {
  const value = parseObjectJSON(dataJson, "Token create result");
  const metadata = objectField(value, "metadata", "Token create result");
  const tags = value["tags"];
  if (!Array.isArray(tags) || tags.some((tag) => typeof tag !== "string")) {
    throw new Error("Token create result.tags must be an array of strings");
  }
  if (value["status"] !== "pending") {
    throw new Error("Token create result.status must be pending");
  }
  return Object.freeze({
    id: resourceID(stringField(value, "id", "Token create result"), "Token create result.id"),
    callbackUrl: stringField(value, "callback_url", "Token create result"),
    publicAccessToken: stringField(value, "public_access_token", "Token create result"),
    timeoutAt: timestampString(value["timeout_at"], "Token create result.timeout_at"),
    status: "pending",
    metadata,
    tags: Object.freeze([...tags]),
    createdAt: timestampString(value["created_at"], "Token create result.created_at"),
    updatedAt: timestampString(value["updated_at"], "Token create result.updated_at")
  });
}
function runtimeOperationFailure(operation, dataJson) {
  const value = parseObjectJSON(dataJson, `${operation} failure`);
  const code = stringField(value, "code", `${operation} failure`);
  const message = stringField(value, "message", `${operation} failure`);
  const retryable = value["retryable"];
  if (typeof retryable !== "boolean") {
    throw new Error(`${operation} failure.retryable must be a boolean`);
  }
  const error = new Error(message);
  error.name = "HelmrError";
  error.code = code;
  return error;
}
function parseTaskResult(dataJson) {
  const value = parseObjectJSON(dataJson, "Task child call result");
  const ok = value["ok"];
  if (ok === true) {
    requireExactKeys(value, ["ok", "output", "run"], "Task child call success");
    return Object.freeze({
      ok: true,
      output: jsonValueField(value, "output", "Task child call success"),
      run: parseTaskResultRun(value)
    });
  }
  if (ok !== false) {
    throw new Error("Task child call result.ok must be a boolean");
  }
  requireExactKeys(value, ["failure", "ok", "run"], "Task child call failure");
  const rawFailure = objectField(value, "failure", "Task child call failure");
  requireExactKeys(rawFailure, ["code", "details", "message"], "Task child call failure.failure");
  const details = objectField(rawFailure, "details", "Task child call failure.failure");
  const failure = Object.freeze({
    code: stringField(rawFailure, "code", "Task child call failure.failure"),
    message: stringField(rawFailure, "message", "Task child call failure.failure"),
    details: Object.freeze({ ...details })
  });
  return Object.freeze({
    ok: false,
    failure,
    run: parseTaskResultRun(value)
  });
}
function parseTaskResultRun(value) {
  const run = objectField(value, "run", "Task child call result");
  requireExactKeys(run, ["id"], "Task child call result.run");
  const id = resourceID(stringField(run, "id", "Task child call result.run"), "Task child call result.run.id");
  return createRunHandle(id);
}
function tokenWaitFailure(kind, dataJson) {
  const failure = parseRuntimeProtocolValue("Token Wait failure", () => resumeFailure(dataJson));
  const code = failure.reasonCode;
  const error = new Error(code === "wait_timeout" ? "Token wait timed out" : code === "token_expired" ? "Token expired" : code === "token_cancelled" ? "Token was cancelled" : `Token Wait ${kind}: ${code}`);
  error.name = code === "wait_timeout" ? "WaitTimeoutError" : "HelmrError";
  error.code = code;
  return error;
}
function normalizeActorInputIdempotencyKey(value) {
  if (value === undefined)
    return;
  const normalized = trimGoSpace(value);
  if (new TextEncoder().encode(normalized).byteLength > 512) {
    throw actorInputSendError("invalid_idempotency_key", "Actor input idempotency key must be at most 512 UTF-8 bytes");
  }
  return normalized === "" ? undefined : normalized;
}
function requireRuntimeOperationDecision(decision, correlationId, operation) {
  if (decision.correlationId !== correlationId || decision.kind !== "completed" && decision.kind !== "failed" || decision.runWaitId !== "" || decision.requireConsumedAck || decision.checkpointId !== "" || decision.resumeAttachId !== "" || decision.resumeRequestVersion !== 0n || decision.runLeaseId !== "" || decision.noResult) {
    throw new RuntimeProtocolError(`${operation} decision did not match the pending operation`);
  }
}
function parseActorInputSendResult(dataJson) {
  const value = parseObjectJSON(dataJson, "Actor input send result");
  requireExactKeys(value, ["created_at", "data", "id", "sequence", "source"], "Actor input send result");
  const record = parseSessionInputRecord(value);
  if (record.sequence === 0) {
    throw new Error("Actor input send result.sequence must be positive");
  }
  return record;
}
function parseActorInputSendFailure(dataJson) {
  const value = parseObjectJSON(dataJson, "Actor input send failure");
  requireExactKeys(value, ["code", "message", "retryable"], "Actor input send failure");
  if (typeof value["code"] !== "string" || value["code"].trim() === "" || typeof value["message"] !== "string" || value["message"].trim() === "" || typeof value["retryable"] !== "boolean") {
    throw new Error("Actor input send failure must contain code, message, and retryable");
  }
  return actorInputSendError(value["code"], value["message"]);
}
function actorInputSendError(code, message) {
  const error = new Error(message);
  error.name = "HelmrError";
  error.code = code;
  return error;
}
async function abortableRuntimeOperation(operation, signal) {
  if (signal === undefined)
    return operation;
  if (signal.aborted)
    throw abortSignalReason(signal);
  let rejectAbort;
  const aborted = new Promise((_resolve, reject) => {
    rejectAbort = reject;
  });
  const onAbort = () => rejectAbort?.(abortSignalReason(signal));
  signal.addEventListener("abort", onAbort, { once: true });
  try {
    return await Promise.race([operation, aborted]);
  } finally {
    signal.removeEventListener("abort", onAbort);
  }
}
function abortSignalReason(signal) {
  return signal.reason === undefined ? new DOMException("The operation was aborted", "AbortError") : signal.reason;
}
function resumeFailure(dataJson) {
  let value;
  try {
    value = JSON.parse(dataJson);
  } catch {
    throw new Error("terminal Wait failure data must be valid JSON");
  }
  if (value === null || typeof value !== "object" || Array.isArray(value) || typeof value.reason_code !== "string" || value.reason_code.trim() === "") {
    throw new Error("terminal Wait failure data must contain reason_code");
  }
  return { reasonCode: value.reason_code };
}
function durationMilliseconds(duration, label = "timer duration") {
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(duration);
  if (match === null) {
    throw new Error(`${label} must be a positive integer followed by ms, s, m, h, or d`);
  }
  const amount = BigInt(match[1]);
  const unit = match[2];
  const multiplierMs = unit === "ms" ? 1n : unit === "s" ? 1000n : unit === "m" ? 60000n : unit === "h" ? 3600000n : 86400000n;
  const milliseconds = amount * multiplierMs;
  const maxMilliseconds = 365n * 24n * 60n * 60n * 1000n;
  if (milliseconds > maxMilliseconds) {
    throw new Error(`${label} must be between 1ms and 365d`);
  }
  return boundedTimerMilliseconds(Number(milliseconds));
}
function boundedTimerMilliseconds(milliseconds) {
  const maxMilliseconds = 31536000000;
  if (!Number.isSafeInteger(milliseconds) || milliseconds < 1 || milliseconds > maxMilliseconds) {
    throw new Error("timer duration must be between 1ms and 365d");
  }
  return milliseconds;
}
function tokenWaitIdleTimeoutMilliseconds(duration) {
  const milliseconds = durationMilliseconds(duration, "Token Wait idle timeout");
  if (milliseconds > 3600000) {
    throw new Error("Token Wait idle timeout must be between 1ms and 1h");
  }
  return milliseconds;
}
async function runActor(start, definition, io, decisions) {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required");
  }
  const cursor = { value: start.entrypoint.value.startInputSequence };
  const waitGate = new ConsumingWaitGate;
  const actorOperations = new RunOperationState;
  const uninstallRuntime = installRuntimeOperations(programRuntimeOperations(start, io, decisions, waitGate, actorOperations, cursor));
  try {
    await definition.handler(actorSelf(start, io, decisions, cursor, waitGate, actorOperations), actorContext(start, actorOperations.controller.signal));
    try {
      await actorOperations.drainForCompletion();
      actorOperations.assertCanComplete();
    } catch (error) {
      decisions.abandonPending();
      throw error;
    }
  } catch (error) {
    if (error instanceof RuntimeProtocolError || error instanceof ActorCancellationError) {
      throw error;
    }
    await actorOperations.drainForCompletion();
    actorOperations.assertCanComplete();
    await writeActorFailure(io, cursor.value, errorMessage(error));
    return;
  } finally {
    uninstallRuntime();
  }
  await writeRunEvent(io, {
    case: "actorOutcome",
    value: create(exports_run_pb.ActorOutcomeSchema, {
      terminalInputSequence: cursor.value,
      outcome: {
        case: "succeeded",
        value: create(exports_run_pb.ActorSucceededSchema)
      }
    })
  });
}
function actorSelf(start, io, decisions, cursor, waitGate, actorOperations) {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required");
  }
  const actorStart = start.entrypoint.value;
  let committedBoundary = cursor.value;
  const commitPriorTurn = async () => {
    if (cursor.value === committedBoundary)
      return;
    const correlationId = newUUIDv7();
    const decision = await requestRuntimeDecision(io, decisions, correlationId, {
      case: "actorTurnCommitRequested",
      value: create(exports_run_pb.ActorTurnCommitRequestedSchema, {
        correlationId,
        targetInputSequence: cursor.value
      })
    });
    requireActorDecision(decision, correlationId, "committed", "Actor turn commit");
    committedBoundary = cursor.value;
  };
  const performReceive = async (options, releaseWait) => {
    try {
      await commitPriorTurn();
      const correlationId = newUUIDv7();
      const runWaitId = newUUIDv7();
      const resumeAttachId = newUUIDv7();
      const timeoutMs = options?.timeout === undefined ? undefined : durationMilliseconds(options.timeout);
      const idleTimeoutMs = options?.idleTimeout === undefined ? undefined : durationMilliseconds(options.idleTimeout);
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(exports_run_pb.RunWaitRequestedSchema, {
          correlationId,
          runWaitId,
          resumeAttachId,
          kind: "actor_input",
          paramsJson: JSON.stringify({
            session_id: actorStart.sessionId,
            after_input_sequence: safeActorSequence(cursor.value)
          }),
          ...options?.metadata === undefined ? {} : { metadataJson: new TextDecoder().decode(canonicalizeJsonValue(options.metadata)) },
          ...timeoutMs === undefined ? {} : { timeoutMs: BigInt(timeoutMs) },
          ...idleTimeoutMs === undefined ? {} : { idleTimeoutMs: BigInt(idleTimeoutMs) },
          tags: options?.tags === undefined ? [] : [...options.tags],
          actorSpeculativeInputSequence: cursor.value
        })
      });
      requireWaitDecision(decision, correlationId, runWaitId, resumeAttachId, "Actor input resume");
      if (decision.requireConsumedAck) {
        await writeRuntimeProtocolEvent(io, {
          case: "resumeConsumed",
          value: create(exports_run_pb.ResumeConsumedSchema, {
            runWaitId: decision.runWaitId,
            checkpointId: decision.checkpointId,
            resumeAttachId: decision.resumeAttachId,
            resumeRequestVersion: decision.resumeRequestVersion,
            runLeaseId: decision.runLeaseId,
            correlationId: decision.correlationId
          })
        });
      }
      if (decision.kind === "completed") {
        const delivered = parseRuntimeProtocolValue("Actor input delivery", () => parseActorInputDelivery(decision.dataJson));
        if (BigInt(delivered.record.sequence) !== cursor.value + 1n) {
          throw new RuntimeProtocolError("Actor input delivery was not the next contiguous record");
        }
        cursor.value = BigInt(delivered.record.sequence);
        return delivered;
      }
      if (decision.kind === "failed") {
        const failure = parseRuntimeProtocolValue("Actor input Wait failure decision", () => resumeFailure(decision.dataJson));
        if (failure.reasonCode !== "wait_timeout" && failure.reasonCode !== "session_closed") {
          throw new RuntimeProtocolError(`Actor input Wait failed: ${failure.reasonCode}`);
        }
        return Object.freeze({
          ok: false,
          error: actorChannelError(failure.reasonCode)
        });
      }
      if (decision.kind === "cancelled") {
        const failure = parseRuntimeProtocolValue("Actor input cancellation decision", () => resumeFailure(decision.dataJson));
        throw actorOperations.cancel(failure.reasonCode);
      }
      throw new RuntimeProtocolError(`Actor input Wait returned unsupported decision ${decision.kind}`);
    } finally {
      releaseWait();
    }
  };
  const receive = (options) => {
    let releaseWait;
    try {
      releaseWait = waitGate.acquire(concurrentSessionReceiveError);
    } catch (error) {
      return actorReceive(Promise.reject(concurrentSessionReceiveError()));
    }
    return actorReceive(actorOperations.track(() => performReceive(options, releaseWait)));
  };
  const performAppend = async (value, options) => {
    const normalized = canonicalizeJsonValue(value);
    const correlationId = newUUIDv7();
    const decision = await requestRuntimeDecision(io, decisions, correlationId, {
      case: "actorOutputAppendRequested",
      value: create(exports_run_pb.ActorOutputAppendRequestedSchema, {
        correlationId,
        dataJson: new TextDecoder().decode(normalized),
        contentType: options?.contentType ?? "application/json",
        ...options?.idempotencyKey === undefined ? {} : { idempotencyKey: options.idempotencyKey }
      })
    });
    requireRuntimeOperationDecision(decision, correlationId, "Actor output append");
    if (decision.kind === "failed") {
      throw runtimeOperationFailure("Actor output append", decision.dataJson);
    }
    return parseRuntimeProtocolValue("Actor output append result", () => parseSessionOutputRecord2(decision.dataJson));
  };
  const append = (value, options) => actorOperations.track(() => performAppend(value, options));
  const performPipe = async (source2, options) => {
    for await (const value of source2)
      await performAppend(value, options);
  };
  const pipe = (source2, options) => actorOperations.track(() => performPipe(source2, options));
  const writer = (options) => {
    let closed = false;
    return Object.freeze({
      write(value) {
        if (closed)
          return Promise.reject(new Error("Actor output writer is closed"));
        return append(value, options);
      },
      async close() {
        closed = true;
      }
    });
  };
  return Object.freeze({
    id: actorStart.sessionId,
    ...actorStart.key === undefined ? {} : { key: actorStart.key },
    input: Object.freeze({ receive }),
    output: Object.freeze({
      append,
      pipe,
      writer
    })
  });
}
function actorReceive(result) {
  return Object.freeze({
    then: result.then.bind(result),
    async unwrap() {
      const resolved = await result;
      if (resolved.ok)
        return resolved.value;
      throw resolved.error;
    }
  });
}
function requireActorDecision(decision, correlationId, kind, operation) {
  if (decision.correlationId !== correlationId || decision.kind !== kind) {
    throw new RuntimeProtocolError(`${operation} decision did not match the pending operation`);
  }
}
function safeActorSequence(value) {
  if (value < 0n || value > BigInt(Number.MAX_SAFE_INTEGER)) {
    throw new Error("Actor input sequence exceeds the JavaScript safe-integer range");
  }
  return Number(value);
}
function parseActorInputDelivery(dataJson) {
  const value = parseObjectJSON(dataJson, "Actor input delivery");
  requireExactKeys(value, ["record", "value"], "Actor input delivery");
  const record = objectField(value, "record", "Actor input delivery");
  requireExactKeys(record, ["created_at", "id", "sequence", "source"], "Actor input record");
  const source2 = objectField(record, "source", "Actor input record");
  const sourceType = stringField(source2, "type", "Actor input source");
  let parsedSource;
  if (sourceType === "external") {
    requireExactKeys(source2, ["type"], "Actor input source");
    parsedSource = Object.freeze({ type: "external" });
  } else if (sourceType === "run") {
    requireExactKeys(source2, ["run_id", "type"], "Actor input source");
    parsedSource = Object.freeze({
      type: "run",
      runId: resourceID(stringField(source2, "run_id", "Actor input source"), "Actor input source.run_id")
    });
  } else {
    throw new Error("Actor input source type is invalid");
  }
  const sequence = safeJSONSequence(record["sequence"], "Actor input record.sequence");
  return Object.freeze({
    ok: true,
    value: jsonValueField(value, "value", "Actor input delivery"),
    record: Object.freeze({
      id: resourceID(stringField(record, "id", "Actor input record"), "Actor input record.id"),
      sequence,
      createdAt: timestampString(record["created_at"], "Session input record.created_at"),
      source: parsedSource
    })
  });
}
function parseSessionOutputRecord2(dataJson) {
  const value = parseObjectJSON(dataJson, "Actor output append result");
  requireExactKeys(value, ["content_type", "created_at", "data", "id", "provenance", "sequence"], "Actor output append result");
  const provenance = objectField(value, "provenance", "Actor output append result");
  requireExactKeys(provenance, ["attempt_number", "deployment_id", "run_id"], "Actor output provenance");
  return Object.freeze({
    id: resourceID(stringField(value, "id", "Actor output append result"), "Actor output append result.id"),
    sequence: safeJSONSequence(value["sequence"], "Actor output sequence"),
    data: jsonValueField(value, "data", "Actor output append result"),
    contentType: stringField(value, "content_type", "Actor output append result"),
    createdAt: timestampString(value["created_at"], "Session output record.created_at"),
    provenance: Object.freeze({
      runId: resourceID(stringField(provenance, "run_id", "Actor output provenance"), "Actor output provenance.run_id"),
      attemptNumber: safeJSONSequence(provenance["attempt_number"], "Actor output attempt number"),
      deploymentId: resourceID(stringField(provenance, "deployment_id", "Actor output provenance"), "Actor output provenance.deployment_id")
    })
  });
}
function parseRuntimeSession(dataJson) {
  const value = parseObjectJSON(dataJson, "Session retrieve result");
  const required = [
    "actor_id",
    "created_at",
    "deployment_id",
    "id",
    "status",
    "updated_at"
  ];
  const optional = ["current_run_id", "failure", "key"];
  const allowed = new Set([...required, ...optional]);
  if (required.some((key) => !Object.hasOwn(value, key)) || Object.keys(value).some((key) => !allowed.has(key))) {
    throw new Error("Session retrieve result has unknown or missing fields");
  }
  return parseSession(value);
}
function parseObjectJSON(value, label) {
  let parsed;
  try {
    parsed = JSON.parse(value);
  } catch {
    throw new Error(`${label} must be valid JSON`);
  }
  if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
    throw new Error(`${label} must be an object`);
  }
  return parsed;
}
function objectField(value, field, label) {
  const nested = value[field];
  if (nested === null || typeof nested !== "object" || Array.isArray(nested)) {
    throw new Error(`${label}.${field} must be an object`);
  }
  return nested;
}
function requireExactKeys(value, expected, label) {
  const actual = Object.keys(value).sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    throw new Error(`${label} has unknown or missing fields`);
  }
}
function stringField(value, field, label) {
  const result = value[field];
  if (typeof result !== "string" || result.trim() === "") {
    throw new Error(`${label}.${field} must be a non-empty string`);
  }
  return result;
}
function jsonValueField(value, field, label) {
  const result = value[field];
  try {
    canonicalizeJsonValue(result);
  } catch (error) {
    throw new Error(`${label}.${field} must be a JSON value`, { cause: error });
  }
  return result;
}
function safeJSONSequence(value, label) {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${label} must be a non-negative safe integer`);
  }
  return value;
}
function actorChannelError(code) {
  const error = new Error(code === "wait_timeout" ? "Actor input receive timed out" : "Actor is closed");
  error.name = code === "wait_timeout" ? "WaitTimeoutError" : "SessionClosedError";
  error.code = code;
  return error;
}
function concurrentSessionReceiveError() {
  const error = new Error("only one Actor input receive may be unresolved");
  error.name = "ConcurrentSessionReceiveError";
  return error;
}
function actorContext(start, signal) {
  if (start.entrypoint.case !== "actor") {
    throw new Error("Actor Program-start entrypoint is required");
  }
  return Object.freeze({
    ...executionContext(start, signal),
    actor: Object.freeze({
      id: start.entrypointDeclaredId
    })
  });
}
async function writeActorFailure(io, terminalInputSequence, message) {
  const normalizedMessage = canonicalFailureMessage(message, "actor failed");
  await writeRunEvent(io, {
    case: "actorOutcome",
    value: create(exports_run_pb.ActorOutcomeSchema, {
      terminalInputSequence,
      outcome: {
        case: "failed",
        value: create(exports_run_pb.ActorFailedSchema, {
          message: normalizedMessage
        })
      }
    })
  });
}
function taskContext(start, signal = new AbortController().signal) {
  return Object.freeze({
    ...executionContext(start, signal),
    task: Object.freeze({ id: start.entrypointDeclaredId })
  });
}
function executionContext(start, signal) {
  return Object.freeze({
    signal,
    run: Object.freeze({
      id: start.runId,
      attemptNumber: start.attemptNumber,
      cause: runCause(start.cause)
    }),
    deployment: Object.freeze({
      id: start.deploymentId,
      version: start.deploymentVersion
    }),
    workspace: createWorkspaceRef(start.workspaceId)
  });
}
function runCause(cause) {
  switch (cause.kind.case) {
    case "api":
      return { type: "api" };
    case "manual":
      return { type: "manual" };
    case "child":
      return {
        type: "child",
        parentRunId: cause.kind.value.parentRunId
      };
    case "schedule":
      return {
        type: "schedule",
        scheduleId: cause.kind.value.scheduleId,
        scheduledAt: new Date(Number(cause.kind.value.scheduledAtUnixMs)).toISOString(),
        ...cause.kind.value.previousScheduledAtUnixMs === undefined ? {} : {
          lastScheduledAt: new Date(Number(cause.kind.value.previousScheduledAtUnixMs)).toISOString()
        },
        timezone: cause.kind.value.timezone
      };
    case "actorStart":
      return { type: "actor_start" };
    case "continuation":
      return { type: "continuation" };
    default:
      throw new Error("Program-start cause is required");
  }
}
async function writeTaskFailure(io, kind, message, details) {
  const normalizedMessage = canonicalFailureMessage(message, "task failed");
  let detailsJson;
  if (details !== undefined) {
    detailsJson = new TextDecoder().decode(canonicalizeJsonValue(details));
    const errorBytes = canonicalizeJsonValue({
      message: normalizedMessage,
      details
    }).byteLength;
    if (errorBytes > MAX_TASK_ERROR_BYTES)
      detailsJson = undefined;
  }
  await writeRunEvent(io, {
    case: "taskOutcome",
    value: create(exports_run_pb.TaskOutcomeSchema, {
      outcome: kind === "failed" ? {
        case: "failed",
        value: create(exports_run_pb.TaskFailedSchema, {
          message: normalizedMessage,
          ...detailsJson === undefined ? {} : { detailsJson }
        })
      } : {
        case: "payloadInvalid",
        value: create(exports_run_pb.TaskPayloadInvalidSchema, {
          message: normalizedMessage,
          ...detailsJson === undefined ? {} : { detailsJson }
        })
      }
    })
  });
}
function validationDetails(issues) {
  return {
    issues: issues.slice(0, 5).map((issue) => ({
      message: boundedUtf8(issue.message, 1024),
      ...issue.path === undefined ? {} : {
        path: issue.path.slice(0, 16).map((part) => boundedUtf8(String(typeof part === "object" && part !== null && "key" in part ? part.key : part), 256))
      }
    })),
    truncated: issues.length > 5
  };
}
function boundedUtf8(value, maxBytes) {
  const encoder = new TextEncoder;
  if (encoder.encode(value).byteLength <= maxBytes)
    return value;
  const suffix = "…";
  const suffixBytes = encoder.encode(suffix).byteLength;
  let result = "";
  let size = 0;
  for (const character of value) {
    const characterBytes = encoder.encode(character).byteLength;
    if (size + characterBytes + suffixBytes > maxBytes)
      break;
    result += character;
    size += characterBytes;
  }
  return result + suffix;
}
function canonicalFailureMessage(message, fallback) {
  const canonical = trimGoSpace(message);
  return boundedUtf8(canonical === "" ? fallback : canonical, MAX_TASK_ERROR_MESSAGE_BYTES);
}
async function writeRunEvent(io, event) {
  const body = toBinary(exports_run_pb.RunEventSchema, create(exports_run_pb.RunEventSchema, { event }));
  await io.write(frame(body));
}
function frame(body) {
  if (body.byteLength > MAX_PROGRAM_FRAME_BYTES) {
    throw new Error(`runtime frame length ${body.byteLength} exceeds max ${MAX_PROGRAM_FRAME_BYTES}`);
  }
  const result = new Uint8Array(4 + body.byteLength);
  new DataView(result.buffer).setUint32(0, body.byteLength);
  result.set(body, 4);
  return result;
}
function defaultProgramIO() {
  const output = createWriteStream("/dev/null", {
    fd: 3,
    autoClose: false
  });
  return {
    input: process.stdin,
    write: (value) => new Promise((resolve, reject) => {
      output.write(value, (error) => {
        if (error === null || error === undefined)
          resolve();
        else
          reject(error);
      });
    })
  };
}
function errorMessage(error) {
  return error instanceof Error ? error.message : String(error);
}
export {
  runProgram
};
