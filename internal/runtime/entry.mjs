var __defProp = Object.defineProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/is-message.js
function isMessage(arg, schema) {
  const isMessage2 = arg !== null && typeof arg == "object" && "$typeName" in arg && typeof arg.$typeName == "string";
  if (!isMessage2) {
    return false;
  }
  if (schema === void 0) {
    return true;
  }
  return schema.typeName === arg.$typeName;
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/descriptors.js
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/wire/varint.js
function varint64read() {
  const buf = this.buf;
  let pos = this.pos;
  let lo = 0;
  let hi = 0;
  for (let shift = 0; shift < 28; shift += 7) {
    const b = buf[pos++];
    lo |= (b & 127) << shift;
    if ((b & 128) == 0) {
      this.pos = pos;
      this.assertBounds();
      this.varint64Lo = lo;
      this.varint64Hi = hi;
      return;
    }
  }
  const middleByte = buf[pos++];
  lo |= (middleByte & 15) << 28;
  hi = (middleByte & 112) >> 4;
  if ((middleByte & 128) == 0) {
    this.pos = pos;
    this.assertBounds();
    this.varint64Lo = lo;
    this.varint64Hi = hi;
    return;
  }
  for (let shift = 3; shift <= 31; shift += 7) {
    const b = buf[pos++];
    hi |= (b & 127) << shift;
    if ((b & 128) == 0) {
      this.pos = pos;
      this.assertBounds();
      this.varint64Lo = lo;
      this.varint64Hi = hi;
      return;
    }
  }
  throw new Error("invalid varint");
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
  if (value >>> 0 < 128) {
    bytes.push(value);
    return;
  }
  if (value >= 0) {
    while (value > 127) {
      bytes.push(value & 127 | 128);
      value = value >>> 7;
    }
    bytes.push(value);
  } else {
    for (let i = 0; i < 9; i++) {
      bytes.push(value & 127 | 128);
      value = value >> 7;
    }
    bytes.push(1);
  }
}
function varint32read() {
  let b = this.buf[this.pos++];
  if ((b & 128) === 0) {
    this.assertBounds();
    return b;
  }
  let result = b & 127;
  b = this.buf[this.pos++];
  result |= (b & 127) << 7;
  if ((b & 128) === 0) {
    this.assertBounds();
    return result;
  }
  b = this.buf[this.pos++];
  result |= (b & 127) << 14;
  if ((b & 128) === 0) {
    this.assertBounds();
    return result;
  }
  b = this.buf[this.pos++];
  result |= (b & 127) << 21;
  if ((b & 128) === 0) {
    this.assertBounds();
    return result;
  }
  b = this.buf[this.pos++];
  result |= (b & 15) << 28;
  for (let readBytes = 5; (b & 128) !== 0 && readBytes < 10; readBytes++)
    b = this.buf[this.pos++];
  if ((b & 128) !== 0)
    throw new Error("invalid varint");
  this.assertBounds();
  return result >>> 0;
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/proto-int64.js
var protoInt64 = /* @__PURE__ */ makeInt64Support();
function makeInt64Support() {
  const dv = new DataView(new ArrayBuffer(8));
  const ok = typeof BigInt === "function" && typeof dv.getBigInt64 === "function" && typeof dv.getBigUint64 === "function" && typeof dv.setBigInt64 === "function" && typeof dv.setBigUint64 === "function" && (!!globalThis.Deno || !!globalThis.Bun || typeof process != "object" || typeof process.env != "object" || process.env.BUF_BIGINT_DISABLE !== "1");
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/scalar.js
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/unsafe.js
function unsafeIsSetExplicit(target, localName) {
  return Object.prototype.hasOwnProperty.call(target, localName) && target[localName] !== void 0;
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/guard.js
function isObject(arg) {
  return arg !== null && typeof arg == "object" && !Array.isArray(arg);
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/wkt/wrappers.js
function isWrapperDesc(messageDesc2) {
  const f = messageDesc2.fields[0];
  return isWrapperTypeName(messageDesc2.typeName) && f !== void 0 && f.fieldKind == "scalar" && f.name == "value" && f.number == 1;
}
var wrapperTypeNames = /* @__PURE__ */ new Set([
  "google.protobuf.DoubleValue",
  "google.protobuf.FloatValue",
  "google.protobuf.Int64Value",
  "google.protobuf.UInt64Value",
  "google.protobuf.Int32Value",
  "google.protobuf.UInt32Value",
  "google.protobuf.BoolValue",
  "google.protobuf.StringValue",
  "google.protobuf.BytesValue"
]);
function isWrapperTypeName(name) {
  return wrapperTypeNames.has(name);
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/create.js
var EDITION_PROTO3 = 999;
var EDITION_PROTO2 = 998;
var IMPLICIT = 2;
function create(schema, init) {
  if (isMessage(init, schema)) {
    return init;
  }
  return compiledCreate(schema)(init);
}
var compiledCreates = /* @__PURE__ */ new WeakMap();
function compiledCreate(desc) {
  let compiled = compiledCreates.get(desc);
  if (compiled === void 0) {
    compiled = compileCreate(desc);
    compiledCreates.set(desc, compiled);
  }
  return compiled;
}
var INIT_SINGULAR = 0;
var INIT_LIST = 1;
var INIT_MAP = 2;
var INIT_ONEOF = 3;
function compileCreate(desc) {
  const typeName = desc.typeName;
  const { properties, prototype } = compileInitMessage(desc);
  return (init) => {
    let message;
    if (prototype !== void 0) {
      message = Object.create(prototype);
      message.$typeName = typeName;
    } else {
      message = { $typeName: typeName };
    }
    for (let i = 0; i < properties.length; i++) {
      const property = properties[i];
      const name = property.name;
      const initValue = init === null || init === void 0 ? void 0 : init[name];
      switch (property.kind) {
        case INIT_SINGULAR:
          if (initValue != null) {
            message[name] = property.convert !== void 0 ? property.convert(initValue) : initValue;
          } else if (property.constant !== void 0) {
            message[name] = property.constant;
          }
          break;
        case INIT_LIST:
          message[name] = property.convert !== void 0 && Array.isArray(initValue) ? initValue.map(property.convert) : initValue !== null && initValue !== void 0 ? initValue : [];
          break;
        case INIT_MAP:
          if (property.convert === void 0 || !isObject(initValue)) {
            message[name] = initValue !== null && initValue !== void 0 ? initValue : {};
          } else {
            const converted = {};
            const keys = Object.keys(initValue);
            for (let k = 0; k < keys.length; k++) {
              converted[keys[k]] = property.convert(initValue[keys[k]]);
            }
            message[name] = converted;
          }
          break;
        case INIT_ONEOF: {
          const oneofValue = initValue;
          if ((oneofValue === null || oneofValue === void 0 ? void 0 : oneofValue.case) != null) {
            const convert = property.convert.get(oneofValue.case);
            if (convert !== void 0) {
              message[name] = {
                case: oneofValue.case,
                value: convert(oneofValue.value)
              };
              break;
            }
          }
          message[name] = { case: void 0 };
          break;
        }
      }
    }
    return message;
  };
}
function compileInitMessage(desc) {
  var _a, _b;
  const properties = [];
  const prototype = {};
  const usePrototype = needsPrototypeChain(desc);
  for (const member of desc.members) {
    const name = member.localName;
    if (member.kind == "oneof") {
      properties.push({
        name,
        kind: INIT_ONEOF,
        constant: void 0,
        convert: compileConvertOneof(member)
      });
      continue;
    }
    switch (member.fieldKind) {
      case "message": {
        properties.push({
          name,
          kind: INIT_SINGULAR,
          constant: void 0,
          convert: compileConvertMessage(member)
        });
        break;
      }
      case "list": {
        properties.push({
          name,
          kind: INIT_LIST,
          constant: void 0,
          convert: member.listKind == "message" ? (_a = compileConvertMessage(member)) !== null && _a !== void 0 ? _a : ((value) => value) : member.scalar == ScalarType.BYTES ? toU8Arr : void 0
        });
        break;
      }
      case "map": {
        properties.push({
          name,
          kind: INIT_MAP,
          constant: void 0,
          convert: member.mapKind == "message" ? (_b = compileConvertMessage(member)) !== null && _b !== void 0 ? _b : ((value) => value) : member.scalar == ScalarType.BYTES ? toU8Arr : void 0
        });
        break;
      }
      default: {
        const zeroValue = createZeroValue(member);
        properties.push({
          name,
          kind: INIT_SINGULAR,
          constant: member.presence == IMPLICIT ? zeroValue : void 0,
          convert: member.fieldKind == "scalar" && member.scalar == ScalarType.BYTES ? toU8Arr : void 0
        });
        if (usePrototype) {
          prototype[name] = zeroValue;
        }
        break;
      }
    }
  }
  return {
    properties,
    prototype: usePrototype ? prototype : void 0
  };
}
function compileConvertOneof(oneof) {
  const converters = /* @__PURE__ */ new Map();
  for (const field of oneof.fields) {
    let convert;
    if (field.fieldKind == "message") {
      convert = compileConvertMessage(field);
    } else if (field.fieldKind == "scalar" && field.scalar == ScalarType.BYTES) {
      convert = toU8Arr;
    }
    converters.set(field.localName, convert !== null && convert !== void 0 ? convert : ((value) => value));
  }
  return converters;
}
function compileConvertMessage(field) {
  if (field.fieldKind == "message" && !field.oneof && isWrapperDesc(field.message)) {
    return field.message.fields[0].scalar == ScalarType.BYTES ? toU8Arr : void 0;
  }
  if (field.message.typeName == "google.protobuf.Struct" && field.parent.typeName !== "google.protobuf.Value") {
    return void 0;
  }
  const messageDesc2 = field.message;
  let compiled;
  return (value) => {
    if (!isObject(value) || isMessage(value, messageDesc2)) {
      return value;
    }
    compiled !== null && compiled !== void 0 ? compiled : compiled = compiledCreate(messageDesc2);
    return compiled(value);
  };
}
function toU8Arr(value) {
  return Array.isArray(value) ? new Uint8Array(value) : value;
}
function needsPrototypeChain(desc) {
  switch (desc.file.edition) {
    case EDITION_PROTO3:
      return false;
    case EDITION_PROTO2:
      return true;
    default:
      return desc.fields.some((f) => f.presence != IMPLICIT && f.fieldKind != "message" && !f.oneof);
  }
}
function createZeroValue(field) {
  const defaultValue = field.getDefaultValue();
  if (defaultValue !== void 0) {
    return field.fieldKind == "scalar" && field.longAsString ? defaultValue.toString() : defaultValue;
  }
  return field.fieldKind == "scalar" ? scalarZeroValue(field.scalar, field.longAsString) : field.enum.values[0].number;
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/error.js
var FieldError = class extends Error {
  constructor(fieldOrOneof, message, name = "FieldValueInvalidError") {
    super(message);
    this.name = name;
    this.field = () => fieldOrOneof;
  }
};

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/wire/text-encoding.js
var te;
function configureTextEncoding(textEncoding) {
  var _a;
  te = Object.assign(Object.assign({}, textEncoding), { encodeUtf8Into: (_a = textEncoding.encodeUtf8Into) !== null && _a !== void 0 ? _a : emulateEncodeInto(textEncoding.encodeUtf8.bind(textEncoding)) });
}
function getTextEncoding() {
  if (!te) {
    const globals = globalThis;
    if (!globals.TextEncoder || !globals.TextDecoder) {
      throw new Error("encoding API missing: install TextEncoder and TextDecoder on globalThis");
    }
    const textEncoder2 = new globals.TextEncoder();
    const textDecoder = new globals.TextDecoder();
    let textDecoderStrict;
    const config = {
      encodeUtf8(text) {
        return textEncoder2.encode(text);
      },
      decodeUtf8(bytes, strict) {
        if (strict) {
          if (!textDecoderStrict) {
            textDecoderStrict = new globals.TextDecoder("utf-8", {
              fatal: true
            });
          }
          return textDecoderStrict.decode(bytes);
        }
        return textDecoder.decode(bytes);
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
    if (textEncoder2.encodeInto) {
      config.encodeUtf8Into = textEncoder2.encodeInto.bind(textEncoder2);
    }
    const nativeStringIsWellFormed = String.prototype.isWellFormed;
    if (nativeStringIsWellFormed) {
      config.checkUtf8 = (text) => {
        return nativeStringIsWellFormed.call(text);
      };
    }
    configureTextEncoding(config);
  }
  return te;
}
function emulateEncodeInto(encodeUtf8) {
  return (text, dest) => {
    const bytes = encodeUtf8(text);
    dest.set(bytes);
    return { written: bytes.byteLength };
  };
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/wire/binary-encoding.js
var WireType;
(function(WireType2) {
  WireType2[WireType2["Varint"] = 0] = "Varint";
  WireType2[WireType2["Bit64"] = 1] = "Bit64";
  WireType2[WireType2["LengthDelimited"] = 2] = "LengthDelimited";
  WireType2[WireType2["StartGroup"] = 3] = "StartGroup";
  WireType2[WireType2["EndGroup"] = 4] = "EndGroup";
  WireType2[WireType2["Bit32"] = 5] = "Bit32";
})(WireType || (WireType = {}));
var FLOAT32_MAX = 34028234663852886e22;
var FLOAT32_MIN = -34028234663852886e22;
var UINT32_MAX = 4294967295;
var INT32_MAX = 2147483647;
var INT32_MIN = -2147483648;
var BinaryWriter = class {
  constructor(encodeUtf8) {
    this.stackPos = [];
    this.encodeUtf8Into = encodeUtf8 ? emulateEncodeInto(encodeUtf8) : getTextEncoding().encodeUtf8Into;
    this.buffer = EMPTY_BUFFER;
    this.viewCache = EMPTY_VIEW;
    this.pos = 0;
  }
  ensureCapacity(size) {
    const required = this.pos + size;
    if (required > this.buffer.length) {
      let newLen = this.buffer.length || INITIAL_SIZE;
      while (newLen < required)
        newLen *= 2;
      const newBuf = new Uint8Array(newLen);
      if (this.pos > 0)
        newBuf.set(this.buffer);
      this.buffer = newBuf;
    }
  }
  /**
   * The DataView over `buffer`, rebuilt only if the buffer has grown since it
   * was last used.
   */
  view() {
    const bytes = this.buffer;
    const view = this.viewCache;
    if (view.byteLength === bytes.byteLength)
      return view;
    const newView = new DataView(bytes.buffer);
    this.viewCache = newView;
    return newView;
  }
  /**
   * Return all bytes written and reset this writer.
   */
  finish() {
    const result = this.buffer.slice(0, this.pos);
    this.pos = 0;
    this.stackPos = [];
    return result;
  }
  /**
   * Start a new fork for length-delimited data like a message
   * or a packed repeated field.
   *
   * Must be joined later with `join()`.
   */
  fork() {
    this.stackPos.push(this.pos);
    this.ensureCapacity(DEFAULT_LEN_PREFIX_SIZE);
    this.buffer[this.pos++] = 0;
    return this;
  }
  /**
   * Join the last fork. Write its length and bytes, then
   * return to the previous state.
   */
  join() {
    const forkPos = this.stackPos.pop();
    if (forkPos === void 0)
      throw new Error("invalid state, fork stack empty");
    const len = this.pos - forkPos - DEFAULT_LEN_PREFIX_SIZE;
    const lenPrefixSize = varint32Size(len);
    if (lenPrefixSize > DEFAULT_LEN_PREFIX_SIZE) {
      this.ensureCapacity(lenPrefixSize - DEFAULT_LEN_PREFIX_SIZE);
      this.buffer.copyWithin(forkPos + lenPrefixSize, forkPos + DEFAULT_LEN_PREFIX_SIZE, this.pos);
    }
    this.pos = forkPos;
    this.uint32(len);
    this.pos += len;
    return this;
  }
  /**
   * Writes a tag (field number and wire type).
   *
   * Equivalent to `uint32( (fieldNo << 3 | type) >>> 0 )`.
   *
   * Generated code should compute the tag ahead of time and call `uint32()`.
   */
  tag(fieldNo, type) {
    return this.uint32((fieldNo << 3 | type) >>> 0);
  }
  /**
   * Write a chunk of raw bytes.
   */
  raw(chunk) {
    this.ensureCapacity(chunk.length);
    this.buffer.set(chunk, this.pos);
    this.pos += chunk.length;
    return this;
  }
  /**
   * Write a `uint32` value, an unsigned 32 bit varint.
   */
  uint32(value) {
    assertUInt32(value);
    this.ensureCapacity(5);
    if (value < 128) {
      this.buffer[this.pos++] = value;
      return this;
    }
    while (value > 127) {
      this.buffer[this.pos++] = value & 127 | 128;
      value >>>= 7;
    }
    this.buffer[this.pos++] = value;
    return this;
  }
  /**
   * Write a `int32` value, a signed 32 bit varint.
   */
  int32(value) {
    assertInt32(value);
    if (value >= 0) {
      return this.uint32(value);
    }
    this.ensureCapacity(10);
    for (let i = 0; i < 9; i++) {
      this.buffer[this.pos++] = value & 127 | 128;
      value >>= 7;
    }
    this.buffer[this.pos++] = 1;
    return this;
  }
  /**
   * Write a `bool` value, a varint.
   */
  bool(value) {
    this.ensureCapacity(1);
    this.buffer[this.pos++] = value ? 1 : 0;
    return this;
  }
  /**
   * Write a `bytes` value, length-delimited arbitrary data.
   */
  bytes(value) {
    this.uint32(value.byteLength);
    return this.raw(value);
  }
  /**
   * Write a `string` value, length-delimited data converted to UTF-8 text.
   */
  string(value) {
    if (typeof value !== "string") {
      value = String(value);
    }
    const len = value.length;
    if (len <= ASCII_MAX_LENGTH) {
      this.ensureCapacity(len + 1);
      const ascii = this.buffer;
      let pos = this.pos;
      ascii[pos++] = len;
      let i = 0;
      for (; i < len; i++) {
        const code = value.charCodeAt(i);
        if (code > 127)
          break;
        ascii[pos++] = code;
      }
      if (i == len) {
        this.pos = pos;
        return this;
      }
    }
    this.ensureCapacity(len * 3 + 5);
    const lenPrefixSizeGuess = varint32Size(len);
    const buf = this.buffer;
    const start = this.pos;
    const { written } = this.encodeUtf8Into(value, buf.subarray(start + lenPrefixSizeGuess));
    const lenPrefixSize = varint32Size(written);
    if (lenPrefixSize != lenPrefixSizeGuess) {
      buf.copyWithin(start + lenPrefixSize, start + lenPrefixSizeGuess, start + lenPrefixSizeGuess + written);
    }
    this.uint32(written);
    this.pos += written;
    return this;
  }
  /**
   * Write a `float` value, 32-bit floating point number.
   */
  float(value) {
    assertFloat32(value);
    this.ensureCapacity(4);
    this.view().setFloat32(this.pos, value, true);
    this.pos += 4;
    return this;
  }
  /**
   * Write a `double` value, a 64-bit floating point number.
   */
  double(value) {
    this.ensureCapacity(8);
    this.view().setFloat64(this.pos, value, true);
    this.pos += 8;
    return this;
  }
  /**
   * Write a `fixed32` value, an unsigned, fixed-length 32-bit integer.
   */
  fixed32(value) {
    assertUInt32(value);
    this.ensureCapacity(4);
    this.view().setUint32(this.pos, value, true);
    this.pos += 4;
    return this;
  }
  /**
   * Write a `sfixed32` value, a signed, fixed-length 32-bit integer.
   */
  sfixed32(value) {
    assertInt32(value);
    this.ensureCapacity(4);
    this.view().setInt32(this.pos, value, true);
    this.pos += 4;
    return this;
  }
  /**
   * Write a `sint32` value, a signed, zigzag-encoded 32-bit varint.
   */
  sint32(value) {
    assertInt32(value);
    return this.uint32((value << 1 ^ value >> 31) >>> 0);
  }
  /**
   * Write a `sfixed64` value, a signed, fixed-length 64-bit integer.
   */
  sfixed64(value) {
    const tc = protoInt64.enc(value);
    this.ensureCapacity(8);
    const view = this.view();
    view.setInt32(this.pos, tc.lo, true);
    view.setInt32(this.pos + 4, tc.hi, true);
    this.pos += 8;
    return this;
  }
  /**
   * Write a `fixed64` value, an unsigned, fixed-length 64 bit integer.
   */
  fixed64(value) {
    const tc = protoInt64.uEnc(value);
    this.ensureCapacity(8);
    const view = this.view();
    view.setInt32(this.pos, tc.lo, true);
    view.setInt32(this.pos + 4, tc.hi, true);
    this.pos += 8;
    return this;
  }
  /**
   * Write a `int64` value, a signed 64-bit varint.
   */
  int64(value) {
    const tc = protoInt64.enc(value);
    return this.writeVarint64(tc.lo, tc.hi);
  }
  /**
   * Write a `sint64` value, a signed, zig-zag-encoded 64-bit varint.
   */
  sint64(value) {
    const tc = protoInt64.enc(value), sign = tc.hi >> 31, lo = tc.lo << 1 ^ sign, hi = (tc.hi << 1 | tc.lo >>> 31) ^ sign;
    return this.writeVarint64(lo, hi);
  }
  /**
   * Write a `uint64` value, an unsigned 64-bit varint.
   */
  uint64(value) {
    const tc = protoInt64.uEnc(value);
    return this.writeVarint64(tc.lo, tc.hi);
  }
  /**
   * Write a 64-bit varint directly into the buffer. Accepts the value as
   * split low/high 32-bit words.
   *
   * Ported from varint64write() to avoid the intermediate number[] buffer.
   * See https://github.com/protocolbuffers/protobuf/blob/8a71927d74a4ce34efe2d8769fda198f52d20d12/js/experimental/runtime/kernel/writer.js#L344
   */
  writeVarint64(lo, hi) {
    this.ensureCapacity(10);
    const buf = this.buffer;
    let pos = this.pos;
    for (let i = 0; i < 28; i = i + 7) {
      const shift = lo >>> i;
      const hasNext = !(shift >>> 7 == 0 && hi == 0);
      buf[pos++] = (hasNext ? shift | 128 : shift) & 255;
      if (!hasNext) {
        this.pos = pos;
        return this;
      }
    }
    const splitBits = lo >>> 28 & 15 | (hi & 7) << 4;
    const hasMoreBits = !(hi >> 3 == 0);
    buf[pos++] = (hasMoreBits ? splitBits | 128 : splitBits) & 255;
    if (!hasMoreBits) {
      this.pos = pos;
      return this;
    }
    for (let i = 3; i < 31; i = i + 7) {
      const shift = hi >>> i;
      const hasNext = !(shift >>> 7 == 0);
      buf[pos++] = (hasNext ? shift | 128 : shift) & 255;
      if (!hasNext) {
        this.pos = pos;
        return this;
      }
    }
    buf[pos++] = hi >>> 31 & 1;
    this.pos = pos;
    return this;
  }
};
var INITIAL_SIZE = 128;
var DEFAULT_LEN_PREFIX_SIZE = 1;
var EMPTY_BUFFER = new Uint8Array(0);
var EMPTY_VIEW = new DataView(EMPTY_BUFFER.buffer);
var ASCII_MAX_LENGTH = 32;
function varint32Size(value) {
  if (value < 128)
    return 1;
  if (value < 16384)
    return 2;
  if (value < 2097152)
    return 3;
  if (value < 268435456)
    return 4;
  return 5;
}
var BinaryReader = class {
  constructor(buf, decodeUtf8 = getTextEncoding().decodeUtf8) {
    this.decodeUtf8 = decodeUtf8;
    this.varint64Lo = 0;
    this.varint64Hi = 0;
    this.varint64 = varint64read;
    this.uint32 = varint32read;
    this.buf = buf;
    this.len = buf.length;
    this.pos = 0;
    this.view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  }
  /**
   * Reads a tag - field number and wire type. Tags are uint32 varints; values
   * that do not fit in uint32 are rejected.
   */
  tag() {
    const start = this.pos;
    const tag = this.uint32();
    const bytesRead = this.pos - start;
    if (bytesRead > 5 || bytesRead == 5 && this.buf[this.pos - 1] > 15) {
      throw new Error("illegal tag: varint overflows uint32");
    }
    const fieldNo = tag >>> 3;
    const wireType = tag & 7;
    if (fieldNo <= 0 || wireType > 5) {
      throw new Error("illegal tag: field no " + fieldNo + " wire type " + wireType);
    }
    return [fieldNo, wireType];
  }
  /**
   * Skip one element and return the skipped data.
   *
   * When skipping StartGroup, provide the tags field number to check for
   * matching field number in the EndGroup tag. Recursion into nested groups
   * is guarded by the `recursionLimit` argument: When the limit is reached,
   * this method throws.
   */
  skip(wireType, fieldNo, recursionLimit = 100) {
    let start = this.pos;
    switch (wireType) {
      case WireType.Varint:
        while (this.buf[this.pos++] & 128) {
        }
        break;
      // @ts-ignore TS7029: Fallthrough case in switch -- ignore instead of expect-error for compiler settings without noFallthroughCasesInSwitch: true
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
        if (recursionLimit <= 0) {
          throw new Error("maximum recursion depth reached");
        }
        for (; ; ) {
          const [fn, wt] = this.tag();
          if (wt === WireType.EndGroup) {
            if (fieldNo !== void 0 && fn !== fieldNo) {
              throw new Error("invalid end group tag");
            }
            break;
          }
          this.skip(wt, fn, recursionLimit - 1);
        }
        break;
      default:
        throw new Error("cant skip wire type " + wireType);
    }
    this.assertBounds();
    return this.buf.subarray(start, this.pos);
  }
  /**
   * Throws error if position in byte array is out of range.
   */
  assertBounds() {
    if (this.pos > this.len)
      throw new RangeError("premature EOF");
  }
  /**
   * Read a `int32` field, a signed 32 bit varint.
   */
  int32() {
    return this.uint32() | 0;
  }
  /**
   * Read a `sint32` field, a signed, zigzag-encoded 32-bit varint.
   */
  sint32() {
    let zze = this.uint32();
    return zze >>> 1 ^ -(zze & 1);
  }
  /**
   * Read a `int64` field, a signed 64-bit varint.
   */
  int64() {
    this.varint64();
    return protoInt64.dec(this.varint64Lo, this.varint64Hi);
  }
  /**
   * Read a `uint64` field, an unsigned 64-bit varint.
   */
  uint64() {
    this.varint64();
    return protoInt64.uDec(this.varint64Lo, this.varint64Hi);
  }
  /**
   * Read a `sint64` field, a signed, zig-zag-encoded 64-bit varint.
   */
  sint64() {
    this.varint64();
    let lo = this.varint64Lo;
    let hi = this.varint64Hi;
    let s = -(lo & 1);
    lo = (lo >>> 1 | (hi & 1) << 31) ^ s;
    hi = hi >>> 1 ^ s;
    return protoInt64.dec(lo, hi);
  }
  /**
   * Read a `bool` field, a variant.
   */
  bool() {
    const b = this.buf[this.pos];
    if (b < 128) {
      this.pos++;
      return b !== 0;
    }
    this.varint64();
    return this.varint64Lo !== 0 || this.varint64Hi !== 0;
  }
  /**
   * Read a `fixed32` field, an unsigned, fixed-length 32-bit integer.
   */
  fixed32() {
    return this.view.getUint32((this.pos += 4) - 4, true);
  }
  /**
   * Read a `sfixed32` field, a signed, fixed-length 32-bit integer.
   */
  sfixed32() {
    return this.view.getInt32((this.pos += 4) - 4, true);
  }
  /**
   * Read a `fixed64` field, an unsigned, fixed-length 64 bit integer.
   */
  fixed64() {
    return protoInt64.uDec(this.sfixed32(), this.sfixed32());
  }
  /**
   * Read a `fixed64` field, a signed, fixed-length 64-bit integer.
   */
  sfixed64() {
    return protoInt64.dec(this.sfixed32(), this.sfixed32());
  }
  /**
   * Read a `float` field, 32-bit floating point number.
   */
  float() {
    return this.view.getFloat32((this.pos += 4) - 4, true);
  }
  /**
   * Read a `double` field, a 64-bit floating point number.
   */
  double() {
    return this.view.getFloat64((this.pos += 8) - 8, true);
  }
  /**
   * Read a `bytes` field, length-delimited arbitrary data.
   */
  bytes() {
    let len = this.uint32(), start = this.pos;
    this.pos += len;
    this.assertBounds();
    return this.buf.subarray(start, start + len);
  }
  /**
   * Read a `string` field, length-delimited data converted to UTF-8 text. If
   * `strict` is true, throw on invalid UTF-8 instead of substituting U+FFFD.
   */
  string(strict) {
    const bytes = this.bytes();
    const len = bytes.length;
    if (len <= ASCII_MAX_LENGTH) {
      const codes = new Array(len);
      for (let i = 0; i < len; i++) {
        const byte = bytes[i];
        if (byte > 127) {
          return this.decodeUtf8(bytes, strict);
        }
        codes[i] = byte;
      }
      return String.fromCharCode.apply(String, codes);
    }
    return this.decodeUtf8(bytes, strict);
  }
};
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/message.js
var NULL_VALUE = 0;
function localMessageMapper(field) {
  if (usesJsonRepresentation(field)) {
    return {
      toMessage: (local) => wktStructToReflect(local),
      toLocal: (message) => wktStructToLocal(message)
    };
  }
  if (field.fieldKind == "message" && !field.oneof && isWrapperDesc(field.message)) {
    const wrapperDesc = field.message;
    const valueLocalName = wrapperDesc.fields[0].localName;
    return {
      toMessage: (local) => {
        const message = create(wrapperDesc);
        if (local !== void 0) {
          message[valueLocalName] = local;
        }
        return message;
      },
      toLocal: (message) => message[valueLocalName]
    };
  }
  const childDesc = field.message;
  return {
    toMessage: (local) => local === void 0 ? create(childDesc) : local,
    toLocal: (message) => message
  };
}
function usesJsonRepresentation(field) {
  return field.message.typeName == "google.protobuf.Struct" && field.parent.typeName != "google.protobuf.Value";
}
function wktStructToReflect(json) {
  const struct = {
    $typeName: "google.protobuf.Struct",
    fields: {}
  };
  if (isObject(json)) {
    for (const k of Object.keys(json)) {
      struct.fields[k] = wktValueToReflect(json[k]);
    }
  }
  return struct;
}
function wktStructToLocal(val) {
  const json = {};
  for (const k of Object.keys(val.fields)) {
    json[k] = wktValueToLocal(val.fields[k]);
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
    case void 0:
      return null;
    default:
      return val.kind.value;
  }
}
function wktValueToReflect(json) {
  const value = {
    $typeName: "google.protobuf.Value",
    kind: { case: void 0 }
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
        value.kind = { case: "nullValue", value: NULL_VALUE };
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/wire/base64-encoding.js
var nativeSetFromBase64 = Uint8Array.prototype.setFromBase64;
function base64Decode(base64Str) {
  const len = base64Str.length;
  let size = len - (len + 3 >> 2);
  if ((len & 3) == 0 && base64Str[len - 1] == "=") {
    size -= base64Str[len - 2] == "=" ? 2 : 1;
  }
  const bytes = new Uint8Array(size);
  let written = -1;
  if (nativeSetFromBase64) {
    try {
      const result = nativeSetFromBase64.call(bytes, base64Str);
      if (result.read == len) {
        written = result.written;
      }
    } catch (_a) {
    }
  }
  if (written < 0) {
    written = setFromBase64(bytes, base64Str);
  }
  return written == size ? bytes : bytes.subarray(0, written);
}
function setFromBase64(bytes, base64Str) {
  const table = getDecodeTable();
  let bytePos = 0, groupPos = 0, b, p = 0;
  for (let i = 0; i < base64Str.length; i++) {
    b = table[base64Str.charCodeAt(i)];
    if (b === void 0) {
      switch (base64Str[i]) {
        // @ts-ignore TS7029: Fallthrough case in switch -- ignore instead of expect-error for compiler settings without noFallthroughCasesInSwitch: true
        case "=":
          groupPos = 0;
        // reset state when padding found
        case "\n":
        case "\r":
        case "	":
        case " ":
          continue;
        // skip white-space, and padding
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
  return bytePos;
}
var nativeToBase64 = Uint8Array.prototype.toBase64;
var encodeTableStd;
var encodeTableUrl;
var decodeTable;
function getEncodeTable(encoding) {
  if (!encodeTableStd) {
    encodeTableStd = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/".split("");
    encodeTableUrl = encodeTableStd.slice(0, -2).concat("-", "_");
  }
  return encoding == "url" ? (
    // biome-ignore lint/style/noNonNullAssertion: TS fails to narrow down
    encodeTableUrl
  ) : encodeTableStd;
}
function getDecodeTable() {
  if (!decodeTable) {
    decodeTable = [];
    const encodeTable = getEncodeTable("std");
    for (let i = 0; i < encodeTable.length; i++)
      decodeTable[encodeTable[i].charCodeAt(0)] = i;
    decodeTable["-".charCodeAt(0)] = encodeTable.indexOf("+");
    decodeTable["_".charCodeAt(0)] = encodeTable.indexOf("/");
  }
  return decodeTable;
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/names.js
function protoCamelCase(snakeCase) {
  let capNext = false;
  const b = [];
  for (let i = 0; i < snakeCase.length; i++) {
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
var reservedObjectProperties = /* @__PURE__ */ new Set([
  // names reserved by JavaScript
  "constructor",
  "toString",
  "toJSON",
  "valueOf"
]);
function safeObjectProperty(name) {
  return reservedObjectProperties.has(name) ? name + "$" : name;
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/codegenv2/restore-json-names.js
function restoreJsonNames(message) {
  for (const f of message.field) {
    if (!unsafeIsSetExplicit(f, "jsonName")) {
      f.jsonName = protoCamelCase(f.name);
    }
  }
  message.nestedType.forEach(restoreJsonNames);
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/wire/text-format.js
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/reflect/nested-types.js
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/registry.js
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
        if (registry.getFile(protoFileName) != void 0) {
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
    const seen = /* @__PURE__ */ new Set();
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
  const types = /* @__PURE__ */ new Map();
  const extendees = /* @__PURE__ */ new Map();
  const files = /* @__PURE__ */ new Map();
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
          extendees.set(
            desc.extendee.typeName,
            // biome-ignore lint/suspicious/noAssignInExpressions: no
            numberToExt = /* @__PURE__ */ new Map()
          );
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
      return (t === null || t === void 0 ? void 0 : t.kind) == "message" ? t : void 0;
    },
    getEnum(typeName) {
      const t = types.get(typeName);
      return (t === null || t === void 0 ? void 0 : t.kind) == "enum" ? t : void 0;
    },
    getExtension(typeName) {
      const t = types.get(typeName);
      return (t === null || t === void 0 ? void 0 : t.kind) == "extension" ? t : void 0;
    },
    getExtensionFor(extendee, no) {
      var _a;
      return (_a = extendees.get(extendee.typeName)) === null || _a === void 0 ? void 0 : _a.get(no);
    },
    getService(typeName) {
      const t = types.get(typeName);
      return (t === null || t === void 0 ? void 0 : t.kind) == "service" ? t : void 0;
    }
  };
}
var EDITION_PROTO22 = 998;
var EDITION_PROTO32 = 999;
var EDITION_UNSTABLE = 9999;
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
var IMPLICIT2 = 2;
var LEGACY_REQUIRED = 3;
var PACKED = 1;
var DELIMITED = 2;
var OPEN = 1;
var VERIFY = 2;
var maximumEdition = 1001;
var featureDefaults = {
  // EDITION_PROTO2
  998: {
    fieldPresence: 1,
    // EXPLICIT,
    enumType: 2,
    // CLOSED,
    repeatedFieldEncoding: 2,
    // EXPANDED,
    utf8Validation: 3,
    // NONE,
    messageEncoding: 1,
    // LENGTH_PREFIXED,
    jsonFormat: 2,
    // LEGACY_BEST_EFFORT,
    enforceNamingStyle: 2,
    // STYLE_LEGACY,
    defaultSymbolVisibility: 1
    // EXPORT_ALL,
  },
  // EDITION_PROTO3
  999: {
    fieldPresence: 2,
    // IMPLICIT,
    enumType: 1,
    // OPEN,
    repeatedFieldEncoding: 1,
    // PACKED,
    utf8Validation: 2,
    // VERIFY,
    messageEncoding: 1,
    // LENGTH_PREFIXED,
    jsonFormat: 1,
    // ALLOW,
    enforceNamingStyle: 2,
    // STYLE_LEGACY,
    defaultSymbolVisibility: 1
    // EXPORT_ALL,
  },
  // EDITION_2023
  1e3: {
    fieldPresence: 1,
    // EXPLICIT,
    enumType: 1,
    // OPEN,
    repeatedFieldEncoding: 1,
    // PACKED,
    utf8Validation: 2,
    // VERIFY,
    messageEncoding: 1,
    // LENGTH_PREFIXED,
    jsonFormat: 1,
    // ALLOW,
    enforceNamingStyle: 2,
    // STYLE_LEGACY,
    defaultSymbolVisibility: 1
    // EXPORT_ALL,
  },
  // EDITION_2024
  1001: {
    fieldPresence: 1,
    // EXPLICIT,
    enumType: 1,
    // OPEN,
    repeatedFieldEncoding: 1,
    // PACKED,
    utf8Validation: 2,
    // VERIFY,
    messageEncoding: 1,
    // LENGTH_PREFIXED,
    jsonFormat: 1,
    // ALLOW,
    enforceNamingStyle: 1,
    // STYLE2024,
    defaultSymbolVisibility: 2
    // EXPORT_TOP_LEVEL,
  }
};
function addFile(proto, reg) {
  var _a, _b;
  const file = {
    kind: "file",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === void 0 ? void 0 : _a.deprecated) !== null && _b !== void 0 ? _b : false,
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
  const mapEntriesStore = /* @__PURE__ */ new Map();
  const mapEntries = {
    get(typeName) {
      return mapEntriesStore.get(typeName);
    },
    add(desc) {
      var _a2;
      assert(((_a2 = desc.proto.options) === null || _a2 === void 0 ? void 0 : _a2.mapEntry) === true);
      mapEntriesStore.set(desc.typeName, desc);
    }
  };
  for (const enumProto of proto.enumType) {
    addEnum(enumProto, file, void 0, reg);
  }
  for (const messageProto of proto.messageType) {
    addMessage(messageProto, file, void 0, reg, mapEntries);
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
  const oneofsSeen = /* @__PURE__ */ new Set();
  for (const proto of message.proto.field) {
    const oneof = findOneof(proto, allOneofs);
    const field = newField(proto, message, reg, oneof, mapEntries);
    message.fields.push(field);
    message.field[field.localName] = field;
    if (oneof === void 0) {
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
    deprecated: (_b = (_a = proto.options) === null || _a === void 0 ? void 0 : _a.deprecated) !== null && _b !== void 0 ? _b : false,
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
    desc.values.push(
      // biome-ignore lint/suspicious/noAssignInExpressions: no
      desc.value[p.number] = {
        kind: "enum_value",
        proto: p,
        deprecated: (_d = (_c = p.options) === null || _c === void 0 ? void 0 : _c.deprecated) !== null && _d !== void 0 ? _d : false,
        parent: desc,
        name,
        localName: safeObjectProperty(sharedPrefix == void 0 ? name : name.substring(sharedPrefix.length)),
        number: p.number,
        toString() {
          return `enum value ${desc.typeName}.${name}`;
        }
      }
    );
  }
  ((_e = parent === null || parent === void 0 ? void 0 : parent.nestedEnums) !== null && _e !== void 0 ? _e : file.enums).push(desc);
}
function addMessage(proto, file, parent, reg, mapEntries) {
  var _a, _b, _c, _d;
  const desc = {
    kind: "message",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === void 0 ? void 0 : _a.deprecated) !== null && _b !== void 0 ? _b : false,
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
  if (((_c = proto.options) === null || _c === void 0 ? void 0 : _c.mapEntry) === true) {
    mapEntries.add(desc);
  } else {
    ((_d = parent === null || parent === void 0 ? void 0 : parent.nestedMessages) !== null && _d !== void 0 ? _d : file.messages).push(desc);
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
    deprecated: (_b = (_a = proto.options) === null || _a === void 0 ? void 0 : _a.deprecated) !== null && _b !== void 0 ? _b : false,
    file,
    name: proto.name,
    typeName: makeTypeName(proto, void 0, file),
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
    deprecated: (_b = (_a = proto.options) === null || _a === void 0 ? void 0 : _a.deprecated) !== null && _b !== void 0 ? _b : false,
    parent,
    name,
    localName: safeObjectProperty(name.length ? safeObjectProperty(name[0].toLowerCase() + name.substring(1)) : name),
    methodKind,
    input,
    output,
    idempotency: (_d = (_c = proto.options) === null || _c === void 0 ? void 0 : _c.idempotencyLevel) !== null && _d !== void 0 ? _d : IDEMPOTENCY_UNKNOWN,
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
  const isExtension = mapEntries === void 0;
  const field = {
    kind: "field",
    proto,
    deprecated: (_b = (_a = proto.options) === null || _a === void 0 ? void 0 : _a.deprecated) !== null && _b !== void 0 ? _b : false,
    name: proto.name,
    number: proto.number,
    scalar: void 0,
    message: void 0,
    enum: void 0,
    presence: getFieldPresence(proto, oneof, isExtension, parentOrFile),
    utf8Validation: isUtf8Validated(proto, parentOrFile),
    listKind: void 0,
    mapKind: void 0,
    mapKey: void 0,
    delimitedEncoding: void 0,
    packed: void 0,
    longAsString: false,
    getDefaultValue: void 0
  };
  let toStr;
  if (isExtension) {
    const file = parentOrFile.kind == "file" ? parentOrFile : parentOrFile.file;
    const parent = parentOrFile.kind == "file" ? void 0 : parentOrFile;
    const typeName = makeTypeName(proto, parent, file);
    field.kind = "extension";
    field.file = file;
    field.parent = parent;
    field.oneof = void 0;
    field.typeName = typeName;
    field.jsonName = `[${typeName}]`;
    toStr = () => `extension ${typeName}`;
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
    toStr = () => `field ${parent.typeName}.${proto.name}`;
  }
  Object.defineProperty(field, "toString", {
    value: toStr,
    writable: true,
    enumerable: true,
    configurable: true
  });
  const label = proto.label;
  const type = proto.type;
  const jstype = (_c = proto.options) === null || _c === void 0 ? void 0 : _c.jstype;
  if (label === LABEL_REPEATED) {
    const mapEntry = type == TYPE_MESSAGE ? mapEntries === null || mapEntries === void 0 ? void 0 : mapEntries.get(trimLeadingDot(proto.typeName)) : void 0;
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
      field.getDefaultValue = () => void 0;
      break;
    case TYPE_ENUM: {
      const enumeration = reg.getEnum(trimLeadingDot(proto.typeName));
      assert(enumeration !== void 0, `invalid FieldDescriptorProto: type_name ${proto.typeName} not found`);
      field.fieldKind = "enum";
      field.enum = reg.getEnum(trimLeadingDot(proto.typeName));
      field.getDefaultValue = () => {
        return unsafeIsSetExplicit(proto, "defaultValue") ? parseTextFormatEnumValue(enumeration, proto.defaultValue) : void 0;
      };
      break;
    }
    default: {
      field.fieldKind = "scalar";
      field.scalar = type;
      field.longAsString = jstype == JS_STRING;
      field.getDefaultValue = () => {
        return unsafeIsSetExplicit(proto, "defaultValue") ? parseTextFormatScalarValue(type, proto.defaultValue) : void 0;
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
      if (proto.edition === EDITION_UNSTABLE) {
        return maximumEdition;
      }
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
      return void 0;
    }
    const shortName = value.name.substring(prefix.length);
    if (shortName.length == 0) {
      return void 0;
    }
    if (/^\d/.test(shortName)) {
      return void 0;
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
    return void 0;
  }
  if (proto.proto3Optional) {
    return void 0;
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
    return IMPLICIT2;
  }
  if (!!oneof || proto.proto3Optional) {
    return EXPLICIT;
  }
  if (isExtension) {
    return EXPLICIT;
  }
  const resolved = resolveFeature("fieldPresence", { proto, parent });
  if (resolved == IMPLICIT2 && (proto.type == TYPE_MESSAGE || proto.type == TYPE_GROUP)) {
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
    parent: (_a = desc.parent) !== null && _a !== void 0 ? _a : desc.file
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
function isUtf8Validated(proto, parent) {
  return VERIFY == resolveFeature("utf8Validation", {
    proto,
    parent
  });
}
function resolveFeature(name, ref) {
  var _a, _b;
  const featureSet = (_a = ref.proto.options) === null || _a === void 0 ? void 0 : _a.features;
  if (featureSet) {
    const val = featureSet[name];
    if (val != 0) {
      return val;
    }
  }
  if ("kind" in ref) {
    if (ref.kind == "message") {
      return resolveFeature(name, (_b = ref.parent) !== null && _b !== void 0 ? _b : ref.file);
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/codegenv2/boot.js
function boot(boot2) {
  const root = bootFileDescriptorProto(boot2);
  root.messageType.forEach(restoreJsonNames);
  const reg = createFileRegistry(root, () => void 0);
  return reg.getFile(root.name);
}
function bootFileDescriptorProto(init) {
  const proto = /* @__PURE__ */ Object.create({
    syntax: "",
    edition: 0
  });
  return Object.assign(proto, Object.assign(Object.assign({ $typeName: "google.protobuf.FileDescriptorProto", dependency: [], publicDependency: [], weakDependency: [], optionDependency: [], service: [], extension: [] }, init), { messageType: init.messageType.map(bootDescriptorProto), enumType: init.enumType.map(bootEnumDescriptorProto) }));
}
function bootDescriptorProto(init) {
  var _a, _b, _c, _d, _e, _f, _g, _h;
  const proto = /* @__PURE__ */ Object.create({
    visibility: 0
  });
  return Object.assign(proto, {
    $typeName: "google.protobuf.DescriptorProto",
    name: init.name,
    field: (_b = (_a = init.field) === null || _a === void 0 ? void 0 : _a.map(bootFieldDescriptorProto)) !== null && _b !== void 0 ? _b : [],
    extension: [],
    nestedType: (_d = (_c = init.nestedType) === null || _c === void 0 ? void 0 : _c.map(bootDescriptorProto)) !== null && _d !== void 0 ? _d : [],
    enumType: (_f = (_e = init.enumType) === null || _e === void 0 ? void 0 : _e.map(bootEnumDescriptorProto)) !== null && _f !== void 0 ? _f : [],
    extensionRange: (_h = (_g = init.extensionRange) === null || _g === void 0 ? void 0 : _g.map((e) => Object.assign({ $typeName: "google.protobuf.DescriptorProto.ExtensionRange" }, e))) !== null && _h !== void 0 ? _h : [],
    oneofDecl: [],
    reservedRange: [],
    reservedName: []
  });
}
function bootFieldDescriptorProto(init) {
  const proto = /* @__PURE__ */ Object.create({
    label: 1,
    typeName: "",
    extendee: "",
    defaultValue: "",
    oneofIndex: 0,
    jsonName: "",
    proto3Optional: false
  });
  return Object.assign(proto, Object.assign(Object.assign({ $typeName: "google.protobuf.FieldDescriptorProto" }, init), { options: init.options ? bootFieldOptions(init.options) : void 0 }));
}
function bootFieldOptions(init) {
  var _a, _b, _c;
  const proto = /* @__PURE__ */ Object.create({
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
  return Object.assign(proto, Object.assign(Object.assign({ $typeName: "google.protobuf.FieldOptions" }, init), { targets: (_a = init.targets) !== null && _a !== void 0 ? _a : [], editionDefaults: (_c = (_b = init.editionDefaults) === null || _b === void 0 ? void 0 : _b.map((e) => Object.assign({ $typeName: "google.protobuf.FieldOptions.EditionDefault" }, e))) !== null && _c !== void 0 ? _c : [], uninterpretedOption: [] }));
}
function bootEnumDescriptorProto(init) {
  const proto = /* @__PURE__ */ Object.create({
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/codegenv2/message.js
function messageDesc(file, path2, ...paths) {
  return paths.reduce((acc, cur) => acc.nestedMessages[cur], file.messages[path2]);
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/wkt/gen/google/protobuf/descriptor_pb.js
var file_google_protobuf_descriptor = /* @__PURE__ */ boot({ "name": "google/protobuf/descriptor.proto", "package": "google.protobuf", "messageType": [{ "name": "FileDescriptorSet", "field": [{ "name": "file", "number": 1, "type": 11, "label": 3, "typeName": ".google.protobuf.FileDescriptorProto" }], "extensionRange": [{ "start": 536e6, "end": 536000001 }] }, { "name": "FileDescriptorProto", "field": [{ "name": "name", "number": 1, "type": 9, "label": 1 }, { "name": "package", "number": 2, "type": 9, "label": 1 }, { "name": "dependency", "number": 3, "type": 9, "label": 3 }, { "name": "public_dependency", "number": 10, "type": 5, "label": 3 }, { "name": "weak_dependency", "number": 11, "type": 5, "label": 3 }, { "name": "option_dependency", "number": 15, "type": 9, "label": 3 }, { "name": "message_type", "number": 4, "type": 11, "label": 3, "typeName": ".google.protobuf.DescriptorProto" }, { "name": "enum_type", "number": 5, "type": 11, "label": 3, "typeName": ".google.protobuf.EnumDescriptorProto" }, { "name": "service", "number": 6, "type": 11, "label": 3, "typeName": ".google.protobuf.ServiceDescriptorProto" }, { "name": "extension", "number": 7, "type": 11, "label": 3, "typeName": ".google.protobuf.FieldDescriptorProto" }, { "name": "options", "number": 8, "type": 11, "label": 1, "typeName": ".google.protobuf.FileOptions" }, { "name": "source_code_info", "number": 9, "type": 11, "label": 1, "typeName": ".google.protobuf.SourceCodeInfo" }, { "name": "syntax", "number": 12, "type": 9, "label": 1 }, { "name": "edition", "number": 14, "type": 14, "label": 1, "typeName": ".google.protobuf.Edition" }] }, { "name": "DescriptorProto", "field": [{ "name": "name", "number": 1, "type": 9, "label": 1 }, { "name": "field", "number": 2, "type": 11, "label": 3, "typeName": ".google.protobuf.FieldDescriptorProto" }, { "name": "extension", "number": 6, "type": 11, "label": 3, "typeName": ".google.protobuf.FieldDescriptorProto" }, { "name": "nested_type", "number": 3, "type": 11, "label": 3, "typeName": ".google.protobuf.DescriptorProto" }, { "name": "enum_type", "number": 4, "type": 11, "label": 3, "typeName": ".google.protobuf.EnumDescriptorProto" }, { "name": "extension_range", "number": 5, "type": 11, "label": 3, "typeName": ".google.protobuf.DescriptorProto.ExtensionRange" }, { "name": "oneof_decl", "number": 8, "type": 11, "label": 3, "typeName": ".google.protobuf.OneofDescriptorProto" }, { "name": "options", "number": 7, "type": 11, "label": 1, "typeName": ".google.protobuf.MessageOptions" }, { "name": "reserved_range", "number": 9, "type": 11, "label": 3, "typeName": ".google.protobuf.DescriptorProto.ReservedRange" }, { "name": "reserved_name", "number": 10, "type": 9, "label": 3 }, { "name": "visibility", "number": 11, "type": 14, "label": 1, "typeName": ".google.protobuf.SymbolVisibility" }], "nestedType": [{ "name": "ExtensionRange", "field": [{ "name": "start", "number": 1, "type": 5, "label": 1 }, { "name": "end", "number": 2, "type": 5, "label": 1 }, { "name": "options", "number": 3, "type": 11, "label": 1, "typeName": ".google.protobuf.ExtensionRangeOptions" }] }, { "name": "ReservedRange", "field": [{ "name": "start", "number": 1, "type": 5, "label": 1 }, { "name": "end", "number": 2, "type": 5, "label": 1 }] }] }, { "name": "ExtensionRangeOptions", "field": [{ "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }, { "name": "declaration", "number": 2, "type": 11, "label": 3, "typeName": ".google.protobuf.ExtensionRangeOptions.Declaration", "options": { "retention": 2 } }, { "name": "features", "number": 50, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "verification", "number": 3, "type": 14, "label": 1, "typeName": ".google.protobuf.ExtensionRangeOptions.VerificationState", "defaultValue": "UNVERIFIED", "options": { "retention": 2 } }], "nestedType": [{ "name": "Declaration", "field": [{ "name": "number", "number": 1, "type": 5, "label": 1 }, { "name": "full_name", "number": 2, "type": 9, "label": 1 }, { "name": "type", "number": 3, "type": 9, "label": 1 }, { "name": "reserved", "number": 5, "type": 8, "label": 1 }, { "name": "repeated", "number": 6, "type": 8, "label": 1 }] }], "enumType": [{ "name": "VerificationState", "value": [{ "name": "DECLARATION", "number": 0 }, { "name": "UNVERIFIED", "number": 1 }] }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "FieldDescriptorProto", "field": [{ "name": "name", "number": 1, "type": 9, "label": 1 }, { "name": "number", "number": 3, "type": 5, "label": 1 }, { "name": "label", "number": 4, "type": 14, "label": 1, "typeName": ".google.protobuf.FieldDescriptorProto.Label" }, { "name": "type", "number": 5, "type": 14, "label": 1, "typeName": ".google.protobuf.FieldDescriptorProto.Type" }, { "name": "type_name", "number": 6, "type": 9, "label": 1 }, { "name": "extendee", "number": 2, "type": 9, "label": 1 }, { "name": "default_value", "number": 7, "type": 9, "label": 1 }, { "name": "oneof_index", "number": 9, "type": 5, "label": 1 }, { "name": "json_name", "number": 10, "type": 9, "label": 1 }, { "name": "options", "number": 8, "type": 11, "label": 1, "typeName": ".google.protobuf.FieldOptions" }, { "name": "proto3_optional", "number": 17, "type": 8, "label": 1 }], "enumType": [{ "name": "Type", "value": [{ "name": "TYPE_DOUBLE", "number": 1 }, { "name": "TYPE_FLOAT", "number": 2 }, { "name": "TYPE_INT64", "number": 3 }, { "name": "TYPE_UINT64", "number": 4 }, { "name": "TYPE_INT32", "number": 5 }, { "name": "TYPE_FIXED64", "number": 6 }, { "name": "TYPE_FIXED32", "number": 7 }, { "name": "TYPE_BOOL", "number": 8 }, { "name": "TYPE_STRING", "number": 9 }, { "name": "TYPE_GROUP", "number": 10 }, { "name": "TYPE_MESSAGE", "number": 11 }, { "name": "TYPE_BYTES", "number": 12 }, { "name": "TYPE_UINT32", "number": 13 }, { "name": "TYPE_ENUM", "number": 14 }, { "name": "TYPE_SFIXED32", "number": 15 }, { "name": "TYPE_SFIXED64", "number": 16 }, { "name": "TYPE_SINT32", "number": 17 }, { "name": "TYPE_SINT64", "number": 18 }] }, { "name": "Label", "value": [{ "name": "LABEL_OPTIONAL", "number": 1 }, { "name": "LABEL_REPEATED", "number": 3 }, { "name": "LABEL_REQUIRED", "number": 2 }] }] }, { "name": "OneofDescriptorProto", "field": [{ "name": "name", "number": 1, "type": 9, "label": 1 }, { "name": "options", "number": 2, "type": 11, "label": 1, "typeName": ".google.protobuf.OneofOptions" }] }, { "name": "EnumDescriptorProto", "field": [{ "name": "name", "number": 1, "type": 9, "label": 1 }, { "name": "value", "number": 2, "type": 11, "label": 3, "typeName": ".google.protobuf.EnumValueDescriptorProto" }, { "name": "options", "number": 3, "type": 11, "label": 1, "typeName": ".google.protobuf.EnumOptions" }, { "name": "reserved_range", "number": 4, "type": 11, "label": 3, "typeName": ".google.protobuf.EnumDescriptorProto.EnumReservedRange" }, { "name": "reserved_name", "number": 5, "type": 9, "label": 3 }, { "name": "visibility", "number": 6, "type": 14, "label": 1, "typeName": ".google.protobuf.SymbolVisibility" }], "nestedType": [{ "name": "EnumReservedRange", "field": [{ "name": "start", "number": 1, "type": 5, "label": 1 }, { "name": "end", "number": 2, "type": 5, "label": 1 }] }] }, { "name": "EnumValueDescriptorProto", "field": [{ "name": "name", "number": 1, "type": 9, "label": 1 }, { "name": "number", "number": 2, "type": 5, "label": 1 }, { "name": "options", "number": 3, "type": 11, "label": 1, "typeName": ".google.protobuf.EnumValueOptions" }] }, { "name": "ServiceDescriptorProto", "field": [{ "name": "name", "number": 1, "type": 9, "label": 1 }, { "name": "method", "number": 2, "type": 11, "label": 3, "typeName": ".google.protobuf.MethodDescriptorProto" }, { "name": "options", "number": 3, "type": 11, "label": 1, "typeName": ".google.protobuf.ServiceOptions" }] }, { "name": "MethodDescriptorProto", "field": [{ "name": "name", "number": 1, "type": 9, "label": 1 }, { "name": "input_type", "number": 2, "type": 9, "label": 1 }, { "name": "output_type", "number": 3, "type": 9, "label": 1 }, { "name": "options", "number": 4, "type": 11, "label": 1, "typeName": ".google.protobuf.MethodOptions" }, { "name": "client_streaming", "number": 5, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "server_streaming", "number": 6, "type": 8, "label": 1, "defaultValue": "false" }] }, { "name": "FileOptions", "field": [{ "name": "java_package", "number": 1, "type": 9, "label": 1 }, { "name": "java_outer_classname", "number": 8, "type": 9, "label": 1 }, { "name": "java_multiple_files", "number": 10, "type": 8, "label": 1, "defaultValue": "false", "options": {} }, { "name": "java_generate_equals_and_hash", "number": 20, "type": 8, "label": 1, "options": { "deprecated": true } }, { "name": "java_string_check_utf8", "number": 27, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "optimize_for", "number": 9, "type": 14, "label": 1, "typeName": ".google.protobuf.FileOptions.OptimizeMode", "defaultValue": "SPEED" }, { "name": "go_package", "number": 11, "type": 9, "label": 1 }, { "name": "cc_generic_services", "number": 16, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "java_generic_services", "number": 17, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "py_generic_services", "number": 18, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "deprecated", "number": 23, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "cc_enable_arenas", "number": 31, "type": 8, "label": 1, "defaultValue": "true" }, { "name": "objc_class_prefix", "number": 36, "type": 9, "label": 1 }, { "name": "csharp_namespace", "number": 37, "type": 9, "label": 1 }, { "name": "swift_prefix", "number": 39, "type": 9, "label": 1 }, { "name": "php_class_prefix", "number": 40, "type": 9, "label": 1 }, { "name": "php_namespace", "number": 41, "type": 9, "label": 1 }, { "name": "php_metadata_namespace", "number": 44, "type": 9, "label": 1 }, { "name": "ruby_package", "number": 45, "type": 9, "label": 1 }, { "name": "features", "number": 50, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }], "enumType": [{ "name": "OptimizeMode", "value": [{ "name": "SPEED", "number": 1 }, { "name": "CODE_SIZE", "number": 2 }, { "name": "LITE_RUNTIME", "number": 3 }] }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "MessageOptions", "field": [{ "name": "message_set_wire_format", "number": 1, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "no_standard_descriptor_accessor", "number": 2, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "deprecated", "number": 3, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "map_entry", "number": 7, "type": 8, "label": 1 }, { "name": "deprecated_legacy_json_field_conflicts", "number": 11, "type": 8, "label": 1, "options": { "deprecated": true } }, { "name": "features", "number": 12, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "FieldOptions", "field": [{ "name": "ctype", "number": 1, "type": 14, "label": 1, "typeName": ".google.protobuf.FieldOptions.CType", "defaultValue": "STRING" }, { "name": "packed", "number": 2, "type": 8, "label": 1 }, { "name": "jstype", "number": 6, "type": 14, "label": 1, "typeName": ".google.protobuf.FieldOptions.JSType", "defaultValue": "JS_NORMAL" }, { "name": "lazy", "number": 5, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "unverified_lazy", "number": 15, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "deprecated", "number": 3, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "weak", "number": 10, "type": 8, "label": 1, "defaultValue": "false", "options": { "deprecated": true } }, { "name": "debug_redact", "number": 16, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "retention", "number": 17, "type": 14, "label": 1, "typeName": ".google.protobuf.FieldOptions.OptionRetention" }, { "name": "targets", "number": 19, "type": 14, "label": 3, "typeName": ".google.protobuf.FieldOptions.OptionTargetType" }, { "name": "edition_defaults", "number": 20, "type": 11, "label": 3, "typeName": ".google.protobuf.FieldOptions.EditionDefault" }, { "name": "features", "number": 21, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "feature_support", "number": 22, "type": 11, "label": 1, "typeName": ".google.protobuf.FieldOptions.FeatureSupport" }, { "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }], "nestedType": [{ "name": "EditionDefault", "field": [{ "name": "edition", "number": 3, "type": 14, "label": 1, "typeName": ".google.protobuf.Edition" }, { "name": "value", "number": 2, "type": 9, "label": 1 }] }, { "name": "FeatureSupport", "field": [{ "name": "edition_introduced", "number": 1, "type": 14, "label": 1, "typeName": ".google.protobuf.Edition" }, { "name": "edition_deprecated", "number": 2, "type": 14, "label": 1, "typeName": ".google.protobuf.Edition" }, { "name": "deprecation_warning", "number": 3, "type": 9, "label": 1 }, { "name": "edition_removed", "number": 4, "type": 14, "label": 1, "typeName": ".google.protobuf.Edition" }, { "name": "removal_error", "number": 5, "type": 9, "label": 1 }] }], "enumType": [{ "name": "CType", "value": [{ "name": "STRING", "number": 0 }, { "name": "CORD", "number": 1 }, { "name": "STRING_PIECE", "number": 2 }] }, { "name": "JSType", "value": [{ "name": "JS_NORMAL", "number": 0 }, { "name": "JS_STRING", "number": 1 }, { "name": "JS_NUMBER", "number": 2 }] }, { "name": "OptionRetention", "value": [{ "name": "RETENTION_UNKNOWN", "number": 0 }, { "name": "RETENTION_RUNTIME", "number": 1 }, { "name": "RETENTION_SOURCE", "number": 2 }] }, { "name": "OptionTargetType", "value": [{ "name": "TARGET_TYPE_UNKNOWN", "number": 0 }, { "name": "TARGET_TYPE_FILE", "number": 1 }, { "name": "TARGET_TYPE_EXTENSION_RANGE", "number": 2 }, { "name": "TARGET_TYPE_MESSAGE", "number": 3 }, { "name": "TARGET_TYPE_FIELD", "number": 4 }, { "name": "TARGET_TYPE_ONEOF", "number": 5 }, { "name": "TARGET_TYPE_ENUM", "number": 6 }, { "name": "TARGET_TYPE_ENUM_ENTRY", "number": 7 }, { "name": "TARGET_TYPE_SERVICE", "number": 8 }, { "name": "TARGET_TYPE_METHOD", "number": 9 }] }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "OneofOptions", "field": [{ "name": "features", "number": 1, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "EnumOptions", "field": [{ "name": "allow_alias", "number": 2, "type": 8, "label": 1 }, { "name": "deprecated", "number": 3, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "deprecated_legacy_json_field_conflicts", "number": 6, "type": 8, "label": 1, "options": { "deprecated": true } }, { "name": "features", "number": 7, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "EnumValueOptions", "field": [{ "name": "deprecated", "number": 1, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "features", "number": 2, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "debug_redact", "number": 3, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "feature_support", "number": 4, "type": 11, "label": 1, "typeName": ".google.protobuf.FieldOptions.FeatureSupport" }, { "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "ServiceOptions", "field": [{ "name": "features", "number": 34, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "deprecated", "number": 33, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "MethodOptions", "field": [{ "name": "deprecated", "number": 33, "type": 8, "label": 1, "defaultValue": "false" }, { "name": "idempotency_level", "number": 34, "type": 14, "label": 1, "typeName": ".google.protobuf.MethodOptions.IdempotencyLevel", "defaultValue": "IDEMPOTENCY_UNKNOWN" }, { "name": "features", "number": 35, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "uninterpreted_option", "number": 999, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption" }], "enumType": [{ "name": "IdempotencyLevel", "value": [{ "name": "IDEMPOTENCY_UNKNOWN", "number": 0 }, { "name": "NO_SIDE_EFFECTS", "number": 1 }, { "name": "IDEMPOTENT", "number": 2 }] }], "extensionRange": [{ "start": 1e3, "end": 536870912 }] }, { "name": "UninterpretedOption", "field": [{ "name": "name", "number": 2, "type": 11, "label": 3, "typeName": ".google.protobuf.UninterpretedOption.NamePart" }, { "name": "identifier_value", "number": 3, "type": 9, "label": 1 }, { "name": "positive_int_value", "number": 4, "type": 4, "label": 1 }, { "name": "negative_int_value", "number": 5, "type": 3, "label": 1 }, { "name": "double_value", "number": 6, "type": 1, "label": 1 }, { "name": "string_value", "number": 7, "type": 12, "label": 1 }, { "name": "aggregate_value", "number": 8, "type": 9, "label": 1 }], "nestedType": [{ "name": "NamePart", "field": [{ "name": "name_part", "number": 1, "type": 9, "label": 2 }, { "name": "is_extension", "number": 2, "type": 8, "label": 2 }] }] }, { "name": "FeatureSet", "field": [{ "name": "field_presence", "number": 1, "type": 14, "label": 1, "typeName": ".google.protobuf.FeatureSet.FieldPresence", "options": { "retention": 1, "targets": [4, 1], "editionDefaults": [{ "value": "EXPLICIT", "edition": 900 }, { "value": "IMPLICIT", "edition": 999 }, { "value": "EXPLICIT", "edition": 1e3 }] } }, { "name": "enum_type", "number": 2, "type": 14, "label": 1, "typeName": ".google.protobuf.FeatureSet.EnumType", "options": { "retention": 1, "targets": [6, 1], "editionDefaults": [{ "value": "CLOSED", "edition": 900 }, { "value": "OPEN", "edition": 999 }] } }, { "name": "repeated_field_encoding", "number": 3, "type": 14, "label": 1, "typeName": ".google.protobuf.FeatureSet.RepeatedFieldEncoding", "options": { "retention": 1, "targets": [4, 1], "editionDefaults": [{ "value": "EXPANDED", "edition": 900 }, { "value": "PACKED", "edition": 999 }] } }, { "name": "utf8_validation", "number": 4, "type": 14, "label": 1, "typeName": ".google.protobuf.FeatureSet.Utf8Validation", "options": { "retention": 1, "targets": [4, 1], "editionDefaults": [{ "value": "NONE", "edition": 900 }, { "value": "VERIFY", "edition": 999 }] } }, { "name": "message_encoding", "number": 5, "type": 14, "label": 1, "typeName": ".google.protobuf.FeatureSet.MessageEncoding", "options": { "retention": 1, "targets": [4, 1], "editionDefaults": [{ "value": "LENGTH_PREFIXED", "edition": 900 }] } }, { "name": "json_format", "number": 6, "type": 14, "label": 1, "typeName": ".google.protobuf.FeatureSet.JsonFormat", "options": { "retention": 1, "targets": [3, 6, 1], "editionDefaults": [{ "value": "LEGACY_BEST_EFFORT", "edition": 900 }, { "value": "ALLOW", "edition": 999 }] } }, { "name": "enforce_naming_style", "number": 7, "type": 14, "label": 1, "typeName": ".google.protobuf.FeatureSet.EnforceNamingStyle", "options": { "retention": 2, "targets": [1, 2, 3, 4, 5, 6, 7, 8, 9], "editionDefaults": [{ "value": "STYLE_LEGACY", "edition": 900 }, { "value": "STYLE2024", "edition": 1001 }] } }, { "name": "default_symbol_visibility", "number": 8, "type": 14, "label": 1, "typeName": ".google.protobuf.FeatureSet.VisibilityFeature.DefaultSymbolVisibility", "options": { "retention": 2, "targets": [1], "editionDefaults": [{ "value": "EXPORT_ALL", "edition": 900 }, { "value": "EXPORT_TOP_LEVEL", "edition": 1001 }] } }], "nestedType": [{ "name": "VisibilityFeature", "enumType": [{ "name": "DefaultSymbolVisibility", "value": [{ "name": "DEFAULT_SYMBOL_VISIBILITY_UNKNOWN", "number": 0 }, { "name": "EXPORT_ALL", "number": 1 }, { "name": "EXPORT_TOP_LEVEL", "number": 2 }, { "name": "LOCAL_ALL", "number": 3 }, { "name": "STRICT", "number": 4 }] }] }], "enumType": [{ "name": "FieldPresence", "value": [{ "name": "FIELD_PRESENCE_UNKNOWN", "number": 0 }, { "name": "EXPLICIT", "number": 1 }, { "name": "IMPLICIT", "number": 2 }, { "name": "LEGACY_REQUIRED", "number": 3 }] }, { "name": "EnumType", "value": [{ "name": "ENUM_TYPE_UNKNOWN", "number": 0 }, { "name": "OPEN", "number": 1 }, { "name": "CLOSED", "number": 2 }] }, { "name": "RepeatedFieldEncoding", "value": [{ "name": "REPEATED_FIELD_ENCODING_UNKNOWN", "number": 0 }, { "name": "PACKED", "number": 1 }, { "name": "EXPANDED", "number": 2 }] }, { "name": "Utf8Validation", "value": [{ "name": "UTF8_VALIDATION_UNKNOWN", "number": 0 }, { "name": "VERIFY", "number": 2 }, { "name": "NONE", "number": 3 }] }, { "name": "MessageEncoding", "value": [{ "name": "MESSAGE_ENCODING_UNKNOWN", "number": 0 }, { "name": "LENGTH_PREFIXED", "number": 1 }, { "name": "DELIMITED", "number": 2 }] }, { "name": "JsonFormat", "value": [{ "name": "JSON_FORMAT_UNKNOWN", "number": 0 }, { "name": "ALLOW", "number": 1 }, { "name": "LEGACY_BEST_EFFORT", "number": 2 }] }, { "name": "EnforceNamingStyle", "value": [{ "name": "ENFORCE_NAMING_STYLE_UNKNOWN", "number": 0 }, { "name": "STYLE2024", "number": 1 }, { "name": "STYLE_LEGACY", "number": 2 }] }], "extensionRange": [{ "start": 1e3, "end": 9995 }, { "start": 9995, "end": 1e4 }, { "start": 1e4, "end": 10001 }] }, { "name": "FeatureSetDefaults", "field": [{ "name": "defaults", "number": 1, "type": 11, "label": 3, "typeName": ".google.protobuf.FeatureSetDefaults.FeatureSetEditionDefault" }, { "name": "minimum_edition", "number": 4, "type": 14, "label": 1, "typeName": ".google.protobuf.Edition" }, { "name": "maximum_edition", "number": 5, "type": 14, "label": 1, "typeName": ".google.protobuf.Edition" }], "nestedType": [{ "name": "FeatureSetEditionDefault", "field": [{ "name": "edition", "number": 3, "type": 14, "label": 1, "typeName": ".google.protobuf.Edition" }, { "name": "overridable_features", "number": 4, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }, { "name": "fixed_features", "number": 5, "type": 11, "label": 1, "typeName": ".google.protobuf.FeatureSet" }] }] }, { "name": "SourceCodeInfo", "field": [{ "name": "location", "number": 1, "type": 11, "label": 3, "typeName": ".google.protobuf.SourceCodeInfo.Location" }], "nestedType": [{ "name": "Location", "field": [{ "name": "path", "number": 1, "type": 5, "label": 3, "options": { "packed": true } }, { "name": "span", "number": 2, "type": 5, "label": 3, "options": { "packed": true } }, { "name": "leading_comments", "number": 3, "type": 9, "label": 1 }, { "name": "trailing_comments", "number": 4, "type": 9, "label": 1 }, { "name": "leading_detached_comments", "number": 6, "type": 9, "label": 3 }] }], "extensionRange": [{ "start": 536e6, "end": 536000001 }] }, { "name": "GeneratedCodeInfo", "field": [{ "name": "annotation", "number": 1, "type": 11, "label": 3, "typeName": ".google.protobuf.GeneratedCodeInfo.Annotation" }], "nestedType": [{ "name": "Annotation", "field": [{ "name": "path", "number": 1, "type": 5, "label": 3, "options": { "packed": true } }, { "name": "source_file", "number": 2, "type": 9, "label": 1 }, { "name": "begin", "number": 3, "type": 5, "label": 1 }, { "name": "end", "number": 4, "type": 5, "label": 1 }, { "name": "semantic", "number": 5, "type": 14, "label": 1, "typeName": ".google.protobuf.GeneratedCodeInfo.Annotation.Semantic" }], "enumType": [{ "name": "Semantic", "value": [{ "name": "NONE", "number": 0 }, { "name": "SET", "number": 1 }, { "name": "ALIAS", "number": 2 }] }] }] }], "enumType": [{ "name": "Edition", "value": [{ "name": "EDITION_UNKNOWN", "number": 0 }, { "name": "EDITION_LEGACY", "number": 900 }, { "name": "EDITION_PROTO2", "number": 998 }, { "name": "EDITION_PROTO3", "number": 999 }, { "name": "EDITION_2023", "number": 1e3 }, { "name": "EDITION_2024", "number": 1001 }, { "name": "EDITION_UNSTABLE", "number": 9999 }, { "name": "EDITION_1_TEST_ONLY", "number": 1 }, { "name": "EDITION_2_TEST_ONLY", "number": 2 }, { "name": "EDITION_99997_TEST_ONLY", "number": 99997 }, { "name": "EDITION_99998_TEST_ONLY", "number": 99998 }, { "name": "EDITION_99999_TEST_ONLY", "number": 99999 }, { "name": "EDITION_MAX", "number": 2147483647 }] }, { "name": "SymbolVisibility", "value": [{ "name": "VISIBILITY_UNSET", "number": 0 }, { "name": "VISIBILITY_LOCAL", "number": 1 }, { "name": "VISIBILITY_EXPORT", "number": 2 }] }] });
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
  Edition2[Edition2["EDITION_2023"] = 1e3] = "EDITION_2023";
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

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/from-binary.js
function makeReadContext(options) {
  return Object.assign(Object.assign({ readUnknownFields: true, recursionLimit: 100 }, options), { depth: 0 });
}
function fromBinary(schema, bytes, options) {
  const message = create(schema);
  compiledReader(schema).read(message, new BinaryReader(bytes), makeReadContext(options), bytes.byteLength);
  return message;
}
var compiledReaders = /* @__PURE__ */ new WeakMap();
function compiledReader(desc) {
  let compiled = compiledReaders.get(desc);
  if (compiled === void 0) {
    compiled = compileMessage(desc);
  }
  return compiled;
}
function compileMessage(desc) {
  const descString = String(desc);
  const fieldReaders = /* @__PURE__ */ new Map();
  const compiled = {
    read: compileMessageReader(descString, fieldReaders),
    readGroup: compileGroupReader(descString, fieldReaders)
  };
  compiledReaders.set(desc, compiled);
  for (const field of desc.fields) {
    fieldReaders.set(field.number, compileFieldReader(field));
  }
  return compiled;
}
function compileMessageReader(descString, fieldReaders) {
  return (message, reader, ctx, length) => {
    var _a;
    if (++ctx.depth > ctx.recursionLimit) {
      throw new Error(`cannot decode ${descString} from binary: maximum recursion depth of ${ctx.recursionLimit} reached`);
    }
    const end = reader.pos + length;
    const unknownFields = (_a = message.$unknown) !== null && _a !== void 0 ? _a : [];
    while (reader.pos < end) {
      const [fieldNo, wireType] = reader.tag();
      const fieldReader = fieldReaders.get(fieldNo);
      if (fieldReader === void 0) {
        const data = reader.skip(wireType, fieldNo, ctx.recursionLimit - ctx.depth);
        if (ctx.readUnknownFields) {
          unknownFields.push({ no: fieldNo, wireType, data });
        }
        continue;
      }
      fieldReader(message, reader, ctx, wireType);
    }
    if (unknownFields.length > 0) {
      message.$unknown = unknownFields;
    }
    ctx.depth--;
  };
}
function compileGroupReader(descString, fieldReaders) {
  return (message, reader, ctx, fieldNo) => {
    var _a;
    if (++ctx.depth > ctx.recursionLimit) {
      throw new Error(`cannot decode ${descString} from binary: maximum recursion depth of ${ctx.recursionLimit} reached`);
    }
    let recordFieldNo;
    let wireType;
    const unknownFields = (_a = message.$unknown) !== null && _a !== void 0 ? _a : [];
    while (reader.pos < reader.len) {
      [recordFieldNo, wireType] = reader.tag();
      if (wireType == WireType.EndGroup) {
        break;
      }
      const fieldReader = fieldReaders.get(recordFieldNo);
      if (fieldReader === void 0) {
        const data = reader.skip(wireType, recordFieldNo, ctx.recursionLimit - ctx.depth);
        if (ctx.readUnknownFields) {
          unknownFields.push({ no: recordFieldNo, wireType, data });
        }
        continue;
      }
      fieldReader(message, reader, ctx, wireType);
    }
    if (wireType != WireType.EndGroup || recordFieldNo !== fieldNo) {
      throw new Error("invalid end group tag");
    }
    if (unknownFields.length > 0) {
      message.$unknown = unknownFields;
    }
    ctx.depth--;
  };
}
function compileFieldReader(field) {
  switch (field.fieldKind) {
    case "scalar":
      return compileScalarFieldReader(field);
    case "enum":
      return compileEnumFieldReader(field);
    case "message":
      return compileMessageFieldReader(field);
    case "list":
      return compileListFieldReader(field);
    case "map":
      return compileMapFieldReader(field);
  }
}
function compileScalarFieldReader(field) {
  const readScalar = compileScalarReader(field.scalar, field.utf8Validation, field.longAsString);
  const localName = field.localName;
  if (field.oneof) {
    const oneofLocalName = field.oneof.localName;
    return (message, reader) => {
      message[oneofLocalName] = {
        case: localName,
        value: readScalar(reader)
      };
    };
  }
  return (message, reader) => {
    message[localName] = readScalar(reader);
  };
}
function compileEnumFieldReader(field) {
  var _a;
  const localName = field.localName;
  const oneofLocalName = (_a = field.oneof) === null || _a === void 0 ? void 0 : _a.localName;
  if (field.enum.open) {
    if (oneofLocalName !== void 0) {
      return (message, reader) => {
        message[oneofLocalName] = { case: localName, value: reader.int32() };
      };
    }
    return (message, reader) => {
      message[localName] = reader.int32();
    };
  }
  const values = field.enum.values;
  const fieldNo = field.number;
  return (message, reader, ctx, wireType) => {
    var _a2;
    const val = reader.int32();
    if (values.some((v) => v.number === val)) {
      if (oneofLocalName !== void 0) {
        message[oneofLocalName] = { case: localName, value: val };
      } else {
        message[localName] = val;
      }
    } else if (ctx.readUnknownFields) {
      const bytes = [];
      varint32write(val, bytes);
      const unknownFields = (_a2 = message.$unknown) !== null && _a2 !== void 0 ? _a2 : [];
      unknownFields.push({
        no: fieldNo,
        wireType,
        data: new Uint8Array(bytes)
      });
      message.$unknown = unknownFields;
    }
  };
}
function compileMessageFieldReader(field) {
  const localName = field.localName;
  const { toMessage, toLocal } = localMessageMapper(field);
  const readChild = compileChildReader(field);
  if (field.oneof) {
    const oneofLocalName = field.oneof.localName;
    return (message, reader, ctx) => {
      const oneof = message[oneofLocalName];
      const child = toMessage(oneof.case === localName ? oneof.value : void 0);
      readChild(child, reader, ctx);
      message[oneofLocalName] = { case: localName, value: toLocal(child) };
    };
  }
  return (message, reader, ctx) => {
    const child = toMessage(message[localName]);
    readChild(child, reader, ctx);
    message[localName] = toLocal(child);
  };
}
function compileChildReader(field) {
  const compiledChild = compiledReader(field.message);
  if (field.delimitedEncoding) {
    const fieldNo = field.number;
    return (child, reader, ctx) => compiledChild.readGroup(child, reader, ctx, fieldNo);
  }
  return (child, reader, ctx) => compiledChild.read(child, reader, ctx, reader.uint32());
}
function compileListFieldReader(field) {
  const localName = field.localName;
  if (field.listKind == "message") {
    const { toMessage, toLocal } = localMessageMapper(field);
    const readChild = compileChildReader(field);
    return (message, reader, ctx) => {
      const child = toMessage(void 0);
      readChild(child, reader, ctx);
      message[localName].push(toLocal(child));
    };
  }
  const scalarType = field.listKind == "enum" ? ScalarType.INT32 : field.scalar;
  const longAsString = field.listKind == "scalar" ? field.longAsString : false;
  const readScalar = compileScalarReader(scalarType, field.utf8Validation, longAsString);
  const packedPossible = scalarType != ScalarType.STRING && scalarType != ScalarType.BYTES;
  return (message, reader, ctx, wireType) => {
    const items = message[localName];
    if (wireType == WireType.LengthDelimited && packedPossible) {
      const end = reader.uint32() + reader.pos;
      while (reader.pos < end) {
        items.push(readScalar(reader));
      }
    } else {
      items.push(readScalar(reader));
    }
  };
}
function compileMapFieldReader(field) {
  const localName = field.localName;
  const readKey = compileScalarReader(field.mapKey, field.utf8Validation, false);
  const keyZero = scalarZeroValue(field.mapKey, false);
  let readValue;
  let valueDefault;
  switch (field.mapKind) {
    case "scalar": {
      const scalar = field.scalar;
      const readScalar = compileScalarReader(scalar, field.utf8Validation, false);
      readValue = (reader) => readScalar(reader);
      if (scalar == ScalarType.BYTES) {
        valueDefault = () => new Uint8Array(0);
      } else {
        const zero = scalarZeroValue(scalar, false);
        valueDefault = () => zero;
      }
      break;
    }
    case "enum": {
      const zero = field.enum.values[0].number;
      readValue = (reader) => reader.int32();
      valueDefault = () => zero;
      break;
    }
    case "message": {
      const { toMessage, toLocal } = localMessageMapper(field);
      const readChild = compiledReader(field.message).read;
      readValue = (reader, ctx) => {
        const child = toMessage(void 0);
        readChild(child, reader, ctx, reader.uint32());
        return toLocal(child);
      };
      valueDefault = () => toLocal(toMessage(void 0));
      break;
    }
  }
  return (message, reader, ctx) => {
    const record = message[localName];
    let key;
    let val;
    const len = reader.uint32();
    const end = reader.pos + len;
    while (reader.pos < end) {
      const [fieldNo] = reader.tag();
      switch (fieldNo) {
        case 1:
          key = readKey(reader);
          break;
        case 2:
          val = readValue(reader, ctx);
          break;
      }
    }
    if (key === void 0) {
      key = keyZero;
    }
    if (val === void 0) {
      val = valueDefault();
    }
    record[key] = val;
  };
}
function compileScalarReader(type, utf8Validation, longAsString) {
  switch (type) {
    case ScalarType.STRING:
      return (reader) => reader.string(utf8Validation);
    case ScalarType.BOOL:
      return (reader) => reader.bool();
    case ScalarType.DOUBLE:
      return (reader) => reader.double();
    case ScalarType.FLOAT:
      return (reader) => reader.float();
    case ScalarType.INT32:
      return (reader) => reader.int32();
    case ScalarType.INT64:
      if (longAsString) {
        return (reader) => String(reader.int64());
      }
      return (reader) => reader.int64();
    case ScalarType.UINT64:
      if (longAsString) {
        return (reader) => String(reader.uint64());
      }
      return (reader) => reader.uint64();
    case ScalarType.FIXED64:
      if (longAsString) {
        return (reader) => String(reader.fixed64());
      }
      return (reader) => reader.fixed64();
    case ScalarType.BYTES:
      return (reader) => reader.bytes();
    case ScalarType.FIXED32:
      return (reader) => reader.fixed32();
    case ScalarType.SFIXED32:
      return (reader) => reader.sfixed32();
    case ScalarType.SFIXED64:
      if (longAsString) {
        return (reader) => String(reader.sfixed64());
      }
      return (reader) => reader.sfixed64();
    case ScalarType.SINT64:
      if (longAsString) {
        return (reader) => String(reader.sint64());
      }
      return (reader) => reader.sint64();
    case ScalarType.UINT32:
      return (reader) => reader.uint32();
    case ScalarType.SINT32:
      return (reader) => reader.sint32();
  }
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/codegenv2/file.js
function fileDesc(b64, imports) {
  var _a;
  const root = fromBinary(FileDescriptorProtoSchema, base64Decode(b64));
  root.messageType.forEach(restoreJsonNames);
  root.dependency = (_a = imports === null || imports === void 0 ? void 0 : imports.map((f) => f.proto.name)) !== null && _a !== void 0 ? _a : [];
  const reg = createFileRegistry(root, (protoFileName) => imports === null || imports === void 0 ? void 0 : imports.find((f) => f.proto.name === protoFileName));
  return reg.getFile(root.name);
}

// node_modules/.bun/@bufbuild+protobuf@2.15.0/node_modules/@bufbuild/protobuf/dist/esm/to-binary.js
var IMPLICIT3 = 2;
var LEGACY_REQUIRED2 = 3;
var writeDefaults = {
  writeUnknownFields: true
};
function makeWriteOptions(options) {
  return options ? Object.assign(Object.assign({}, writeDefaults), options) : writeDefaults;
}
function toBinary(schema, message, options) {
  const writer = new BinaryWriter();
  compiledWriter(schema)(writer, makeWriteOptions(options), message);
  return writer.finish();
}
var compiledWriters = /* @__PURE__ */ new WeakMap();
function compiledWriter(desc) {
  let compiled = compiledWriters.get(desc);
  if (compiled === void 0) {
    compiled = compileMessage2(desc);
  }
  return compiled;
}
function compileMessage2(desc) {
  const typeName = desc.typeName;
  const sortedFields = desc.fields.concat().sort((a, b) => a.number - b.number);
  const foreignField = sortedFields[0];
  const fieldWriters = [];
  const compiled = (writer, opts, message) => {
    if (message.$typeName !== typeName && foreignField !== void 0) {
      throw new FieldError(foreignField, `cannot use ${foreignField} with message ${message.$typeName}`, "ForeignFieldError");
    }
    for (let i = 0; i < fieldWriters.length; i++) {
      fieldWriters[i](writer, opts, message);
    }
    const unknown = message.$unknown;
    if (unknown !== void 0 && opts.writeUnknownFields) {
      for (let i = 0; i < unknown.length; i++) {
        const { no, wireType, data } = unknown[i];
        writer.tag(no, wireType).raw(data);
      }
    }
  };
  compiledWriters.set(desc, compiled);
  for (const field of sortedFields) {
    fieldWriters.push(compileField(field));
  }
  return compiled;
}
function compileField(field) {
  switch (field.fieldKind) {
    case "message":
    case "scalar":
    case "enum":
      return compileSingularField(field);
    case "list":
      return compileListField(field);
    case "map":
      return compileMapField(field);
  }
}
function compileSingularField(field) {
  const writeValue = compileSingularValue(field);
  const localName = field.localName;
  if (field.oneof) {
    const oneofLocalName = field.oneof.localName;
    return (writer, opts, message) => {
      const oneof = message[oneofLocalName];
      if (oneof.case === localName) {
        writeValue(writer, opts, oneof.value);
      }
    };
  }
  if (field.presence != IMPLICIT3) {
    const requiredError = field.presence == LEGACY_REQUIRED2 ? `cannot encode ${field} to binary: required field not set` : void 0;
    return (writer, opts, message) => {
      const value = message[localName];
      if (value !== void 0 && Object.prototype.hasOwnProperty.call(message, localName)) {
        writeValue(writer, opts, value);
      } else if (requiredError !== void 0) {
        throw new Error(requiredError);
      }
    };
  }
  if (field.fieldKind == "enum") {
    const zero = field.enum.values[0].number;
    return (writer, opts, message) => {
      const value = message[localName];
      if (value !== zero) {
        writeValue(writer, opts, value);
      }
    };
  }
  switch (field.scalar) {
    case ScalarType.BOOL:
      return (writer, opts, message) => {
        const value = message[localName];
        if (value !== false) {
          writeValue(writer, opts, value);
        }
      };
    case ScalarType.STRING:
      return (writer, opts, message) => {
        const value = message[localName];
        if (value !== "") {
          writeValue(writer, opts, value);
        }
      };
    case ScalarType.BYTES:
      return (writer, opts, message) => {
        const value = message[localName];
        if (!(value instanceof Uint8Array) || value.byteLength > 0) {
          writeValue(writer, opts, value);
        }
      };
    case ScalarType.DOUBLE:
    case ScalarType.FLOAT:
      return (writer, opts, message) => {
        const value = message[localName];
        if (!Object.is(value, 0)) {
          writeValue(writer, opts, value);
        }
      };
    default:
      return (writer, opts, message) => {
        const value = message[localName];
        if (value != 0) {
          writeValue(writer, opts, value);
        }
      };
  }
}
function compileSingularValue(field) {
  switch (field.fieldKind) {
    case "message": {
      const { toMessage } = localMessageMapper(field);
      const writeChild = compileChildWriter(field);
      return (writer, opts, value) => {
        writeChild(writer, opts, toMessage(value));
      };
    }
    case "scalar":
    case "enum": {
      const scalarType = field.fieldKind == "enum" ? ScalarType.INT32 : field.scalar;
      const fieldNo = field.number;
      const wireType = writeTypeOfScalar(scalarType);
      const writeScalar = compileScalarValue(scalarType, field.parent.typeName, field.name);
      return (writer, opts, value) => {
        writer.tag(fieldNo, wireType);
        writeScalar(writer, value);
      };
    }
  }
}
function compileListField(field) {
  const localName = field.localName;
  const fieldNo = field.number;
  switch (field.listKind) {
    case "message": {
      const { toMessage } = localMessageMapper(field);
      const writeChild = compileChildWriter(field);
      return (writer, opts, message) => {
        const items = message[localName];
        for (let i = 0; i < items.length; i++) {
          writeChild(writer, opts, toMessage(items[i]));
        }
      };
    }
    case "scalar":
    case "enum": {
      const scalarType = field.listKind == "enum" ? ScalarType.INT32 : field.scalar;
      const writeScalar = compileScalarValue(scalarType, field.parent.typeName, field.name);
      if (field.packed) {
        return (writer, opts, message) => {
          const items = message[localName];
          if (items.length == 0) {
            return;
          }
          writer.tag(fieldNo, WireType.LengthDelimited).fork();
          for (let i = 0; i < items.length; i++) {
            writeScalar(writer, items[i]);
          }
          writer.join();
        };
      }
      const wireType = writeTypeOfScalar(scalarType);
      return (writer, opts, message) => {
        const items = message[localName];
        for (let i = 0; i < items.length; i++) {
          writer.tag(fieldNo, wireType);
          writeScalar(writer, items[i]);
        }
      };
    }
  }
}
function compileMapField(field) {
  const localName = field.localName;
  const fieldNo = field.number;
  const writeKey = compileMapKey(field);
  if (field.mapKind == "message") {
    const { toMessage } = localMessageMapper(field);
    const writeMessage = compiledWriter(field.message);
    return (writer, opts, message) => {
      const record = message[localName];
      const keys = Object.keys(record);
      for (let i = 0; i < keys.length; i++) {
        const key = keys[i];
        writer.tag(fieldNo, WireType.LengthDelimited).fork();
        writeKey(writer, key);
        writer.tag(2, WireType.LengthDelimited).fork();
        writeMessage(writer, opts, toMessage(record[key]));
        writer.join();
        writer.join();
      }
    };
  }
  const scalarType = field.mapKind == "enum" ? ScalarType.INT32 : field.scalar;
  const valueWireType = writeTypeOfScalar(scalarType);
  const writeScalar = compileScalarValue(scalarType, field.parent.typeName, field.name);
  return (writer, opts, message) => {
    const record = message[localName];
    const keys = Object.keys(record);
    for (let i = 0; i < keys.length; i++) {
      const key = keys[i];
      writer.tag(fieldNo, WireType.LengthDelimited).fork();
      writeKey(writer, key);
      writer.tag(2, valueWireType);
      writeScalar(writer, record[key]);
      writer.join();
    }
  };
}
function compileMapKey(field) {
  const wireType = writeTypeOfScalar(field.mapKey);
  const writeScalar = compileScalarValue(field.mapKey, field.parent.typeName, field.name);
  const convertKey = compileMapKeyConverter(field.mapKey);
  return (writer, key) => {
    writer.tag(1, wireType);
    writeScalar(writer, convertKey(key));
  };
}
function compileMapKeyConverter(type) {
  switch (type) {
    case ScalarType.STRING:
      return (key) => key;
    case ScalarType.BOOL:
      return (key) => key === "true" ? true : key === "false" ? false : key;
    case ScalarType.UINT64:
    case ScalarType.FIXED64:
      return (key) => {
        try {
          return protoInt64.uParse(key);
        } catch (_a) {
          return key;
        }
      };
    case ScalarType.INT64:
    case ScalarType.SFIXED64:
    case ScalarType.SINT64:
      return (key) => {
        try {
          return protoInt64.parse(key);
        } catch (_a) {
          return key;
        }
      };
    default:
      return (key) => {
        const n = Number.parseInt(key);
        return Number.isFinite(n) ? n : key;
      };
  }
}
function compileScalarValue(type, messageName, fieldName) {
  const writeScalar = compileScalarWrite(type);
  return (writer, value) => {
    try {
      writeScalar(writer, value);
    } catch (e) {
      if (e instanceof Error) {
        throw new Error(`cannot encode field ${messageName}.${fieldName} to binary: ${e.message}`);
      }
      throw e;
    }
  };
}
function compileScalarWrite(type) {
  switch (type) {
    case ScalarType.STRING:
      return (writer, value) => writer.string(value);
    case ScalarType.BOOL:
      return (writer, value) => writer.bool(value);
    case ScalarType.DOUBLE:
      return (writer, value) => writer.double(value);
    case ScalarType.FLOAT:
      return (writer, value) => writer.float(value);
    case ScalarType.INT32:
      return (writer, value) => writer.int32(value);
    case ScalarType.INT64:
      return (writer, value) => writer.int64(value);
    case ScalarType.UINT64:
      return (writer, value) => writer.uint64(value);
    case ScalarType.FIXED64:
      return (writer, value) => writer.fixed64(value);
    case ScalarType.BYTES:
      return (writer, value) => writer.bytes(value);
    case ScalarType.FIXED32:
      return (writer, value) => writer.fixed32(value);
    case ScalarType.SFIXED32:
      return (writer, value) => writer.sfixed32(value);
    case ScalarType.SFIXED64:
      return (writer, value) => writer.sfixed64(value);
    case ScalarType.SINT64:
      return (writer, value) => writer.sint64(value);
    case ScalarType.UINT32:
      return (writer, value) => writer.uint32(value);
    case ScalarType.SINT32:
      return (writer, value) => writer.sint32(value);
  }
}
function compileChildWriter(field) {
  const fieldNo = field.number;
  const writeMessage = compiledWriter(field.message);
  if (field.delimitedEncoding) {
    return (writer, opts, child) => {
      writer.tag(fieldNo, WireType.StartGroup);
      writeMessage(writer, opts, child);
      writer.tag(fieldNo, WireType.EndGroup);
    };
  }
  return (writer, opts, child) => {
    writer.tag(fieldNo, WireType.LengthDelimited).fork();
    writeMessage(writer, opts, child);
    writer.join();
  };
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

// proto/typescript/src/gen/program_pb.ts
var program_pb_exports = {};
__export(program_pb_exports, {
  ActorEntrypointSchema: () => ActorEntrypointSchema,
  ActorFailedSchema: () => ActorFailedSchema,
  ActorInterruptedSchema: () => ActorInterruptedSchema,
  ActorOutcomeSchema: () => ActorOutcomeSchema,
  ActorStartCauseSchema: () => ActorStartCauseSchema,
  ActorStartRequestedSchema: () => ActorStartRequestedSchema,
  ActorStartSchema: () => ActorStartSchema,
  ActorSucceededSchema: () => ActorSucceededSchema,
  ApiCauseSchema: () => ApiCauseSchema,
  CheckpointPauseRequestSchema: () => CheckpointPauseRequestSchema,
  ChildCauseSchema: () => ChildCauseSchema,
  ContinuationCauseSchema: () => ContinuationCauseSchema,
  EntrypointIdentitySchema: () => EntrypointIdentitySchema,
  EntrypointReadySchema: () => EntrypointReadySchema,
  EntrypointReleaseSchema: () => EntrypointReleaseSchema,
  ManualCauseSchema: () => ManualCauseSchema,
  MetadataUpdatedSchema: () => MetadataUpdatedSchema,
  NoPayloadSchema: () => NoPayloadSchema,
  ProgramProcessStartFailedSchema: () => ProgramProcessStartFailedSchema,
  ProgramProcessStartedSchema: () => ProgramProcessStartedSchema,
  ProgramQuiescedSchema: () => ProgramQuiescedSchema,
  ProgramRunRequestSchema: () => ProgramRunRequestSchema,
  ProgramSecretSchema: () => ProgramSecretSchema,
  ProgramSecretsCompleteSchema: () => ProgramSecretsCompleteSchema,
  ProgramStartReleaseSchema: () => ProgramStartReleaseSchema,
  ProgramStartSchema: () => ProgramStartSchema,
  ProgramSupervisorCommandSchema: () => ProgramSupervisorCommandSchema,
  ResumeAckSchema: () => ResumeAckSchema,
  ResumeAttachSchema: () => ResumeAttachSchema,
  ResumeConsumedSchema: () => ResumeConsumedSchema,
  ResumeDecisionSchema: () => ResumeDecisionSchema,
  RunCauseSchema: () => RunCauseSchema,
  RunEventSchema: () => RunEventSchema,
  RunWaitRequestedSchema: () => RunWaitRequestedSchema,
  ScheduleCauseSchema: () => ScheduleCauseSchema,
  SecretEnvBindingSchema: () => SecretEnvBindingSchema,
  SecretFileBindingSchema: () => SecretFileBindingSchema,
  SessionCancelRequestedSchema: () => SessionCancelRequestedSchema,
  SessionCloseRequestedSchema: () => SessionCloseRequestedSchema,
  SessionEventsRequestedSchema: () => SessionEventsRequestedSchema,
  SessionExecutionSchema: () => SessionExecutionSchema,
  SessionOutputWriteRequestedSchema: () => SessionOutputWriteRequestedSchema,
  SessionResumeRequestedSchema: () => SessionResumeRequestedSchema,
  SessionStatusRequestedSchema: () => SessionStatusRequestedSchema,
  SessionStopSchema: () => SessionStopSchema,
  SessionSubmitRequestedSchema: () => SessionSubmitRequestedSchema,
  SessionTurnInterruptRequestedSchema: () => SessionTurnInterruptRequestedSchema,
  SessionTurnRetrieveRequestedSchema: () => SessionTurnRetrieveRequestedSchema,
  StructuredLogRequestedSchema: () => StructuredLogRequestedSchema,
  TaskChildInvokeRequestedSchema: () => TaskChildInvokeRequestedSchema,
  TaskEntrypointSchema: () => TaskEntrypointSchema,
  TaskFailedSchema: () => TaskFailedSchema,
  TaskOutcomeSchema: () => TaskOutcomeSchema,
  TaskPayloadInvalidSchema: () => TaskPayloadInvalidSchema,
  TaskStartSchema: () => TaskStartSchema,
  TaskSucceededSchema: () => TaskSucceededSchema,
  TokenCreateRequestedSchema: () => TokenCreateRequestedSchema,
  TurnExecutionSchema: () => TurnExecutionSchema,
  TurnMessageClaimRequestedSchema: () => TurnMessageClaimRequestedSchema,
  TurnMessageCompleteRequestedSchema: () => TurnMessageCompleteRequestedSchema,
  TurnOutputWriteRequestedSchema: () => TurnOutputWriteRequestedSchema,
  TurnReadyRequestedSchema: () => TurnReadyRequestedSchema,
  TurnSettleRequestedSchema: () => TurnSettleRequestedSchema,
  TurnSettlementBeginRequestedSchema: () => TurnSettlementBeginRequestedSchema,
  WorkspaceAddressSchema: () => WorkspaceAddressSchema,
  WorkspaceCreateRequestedSchema: () => WorkspaceCreateRequestedSchema,
  WorkspaceDeleteRequestedSchema: () => WorkspaceDeleteRequestedSchema,
  WorkspaceExecRequestedSchema: () => WorkspaceExecRequestedSchema,
  WorkspaceRetrieveRequestedSchema: () => WorkspaceRetrieveRequestedSchema,
  WorkspaceSecretPlacementSchema: () => WorkspaceSecretPlacementSchema,
  file_program: () => file_program
});
var file_program = /* @__PURE__ */ fileDesc("Cg1wcm9ncmFtLnByb3RvEhBoZWxtci5wcm9ncmFtLnYwItcCCgxQcm9ncmFtU3RhcnQSHgoWZW50cnlwb2ludF9kZWNsYXJlZF9pZBgBIAEoCRIOCgZydW5faWQYAiABKAkSFgoOYXR0ZW1wdF9udW1iZXIYAyABKA0SKQoFY2F1c2UYBCABKAsyGi5oZWxtci5wcm9ncmFtLnYwLlJ1bkNhdXNlEhUKDWRlcGxveW1lbnRfaWQYBSABKAkSGgoSZGVwbG95bWVudF92ZXJzaW9uGAYgASgJEhQKDHdvcmtzcGFjZV9pZBgHIAEoCRIhChliYXNlX3dvcmtzcGFjZV92ZXJzaW9uX2lkGAggASgJEisKBHRhc2sYCSABKAsyGy5oZWxtci5wcm9ncmFtLnYwLlRhc2tTdGFydEgAEi0KBWFjdG9yGAogASgLMhwuaGVsbXIucHJvZ3JhbS52MC5BY3RvclN0YXJ0SABCDAoKZW50cnlwb2ludCJhCglUYXNrU3RhcnQSMQoKbm9fcGF5bG9hZBgBIAEoCzIbLmhlbG1yLnByb2dyYW0udjAuTm9QYXlsb2FkSAASFgoMcGF5bG9hZF9qc29uGAIgASgMSABCCQoHcGF5bG9hZCILCglOb1BheWxvYWQijgEKCkFjdG9yU3RhcnQSEgoKc2Vzc2lvbl9pZBgBIAEoCRIQCgNrZXkYAiABKAlIAIgBARIcChRzdGFydF9pbnB1dF9zZXF1ZW5jZRgDIAEoAxIcChRpbnB1dF9oaWdoX3dhdGVybWFyaxgEIAEoAxIWCg5ydW5fZ2VuZXJhdGlvbhgFIAEoA0IGCgRfa2V5IskCCghSdW5DYXVzZRIpCgNhcGkYASABKAsyGi5oZWxtci5wcm9ncmFtLnYwLkFwaUNhdXNlSAASLwoGbWFudWFsGAIgASgLMh0uaGVsbXIucHJvZ3JhbS52MC5NYW51YWxDYXVzZUgAEi0KBWNoaWxkGAMgASgLMhwuaGVsbXIucHJvZ3JhbS52MC5DaGlsZENhdXNlSAASMwoIc2NoZWR1bGUYBCABKAsyHy5oZWxtci5wcm9ncmFtLnYwLlNjaGVkdWxlQ2F1c2VIABI4CgthY3Rvcl9zdGFydBgFIAEoCzIhLmhlbG1yLnByb2dyYW0udjAuQWN0b3JTdGFydENhdXNlSAASOwoMY29udGludWF0aW9uGAYgASgLMiMuaGVsbXIucHJvZ3JhbS52MC5Db250aW51YXRpb25DYXVzZUgAQgYKBGtpbmQiCgoIQXBpQ2F1c2UiDQoLTWFudWFsQ2F1c2UiIwoKQ2hpbGRDYXVzZRIVCg1wYXJlbnRfcnVuX2lkGAEgASgJIqIBCg1TY2hlZHVsZUNhdXNlEhMKC3NjaGVkdWxlX2lkGAEgASgJEhwKFHNjaGVkdWxlZF9hdF91bml4X21zGAIgASgDEioKHXByZXZpb3VzX3NjaGVkdWxlZF9hdF91bml4X21zGAMgASgDSACIAQESEAoIdGltZXpvbmUYBCABKAlCIAoeX3ByZXZpb3VzX3NjaGVkdWxlZF9hdF91bml4X21zIhEKD0FjdG9yU3RhcnRDYXVzZSITChFDb250aW51YXRpb25DYXVzZSK5AgoRUHJvZ3JhbVJ1blJlcXVlc3QSDgoGcnVuX2lkGAEgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAIgASgNEhQKDHJ1bl9sZWFzZV9pZBgDIAEoCRIbChNwcm9ncmFtX3N0YXJ0X2ZyYW1lGAQgASgMEhQKDHNlY3JldF9jb3VudBgFIAEoDRIeChZzdGFydF9kZWFkbGluZV91bml4X21zGAYgASgDEkwKDXByb3RlY3RlZF9lbnYYByADKAsyNS5oZWxtci5wcm9ncmFtLnYwLlByb2dyYW1SdW5SZXF1ZXN0LlByb3RlY3RlZEVudkVudHJ5EhAKCHByb3h5X2NhGAggASgMGjMKEVByb3RlY3RlZEVudkVudHJ5EgsKA2tleRgBIAEoCRINCgV2YWx1ZRgCIAEoCToCOAEiSgoNUHJvZ3JhbVNlY3JldBINCgNlbnYYASABKAlIABIOCgRmaWxlGAIgASgJSAASDQoFdmFsdWUYAyABKAxCCwoJcGxhY2VtZW50ImwKFlByb2dyYW1TZWNyZXRzQ29tcGxldGUSDgoGcnVuX2lkGAEgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAIgASgNEhQKDHJ1bl9sZWFzZV9pZBgDIAEoCRIUCgxzZWNyZXRfY291bnQYBCABKA0iqgIKGFByb2dyYW1TdXBlcnZpc29yQ29tbWFuZBI6Cg9zZWNyZXRfZGVsaXZlcnkYASABKAsyHy5oZWxtci5wcm9ncmFtLnYwLlByb2dyYW1TZWNyZXRIABJEChBzZWNyZXRzX2NvbXBsZXRlGAIgASgLMiguaGVsbXIucHJvZ3JhbS52MC5Qcm9ncmFtU2VjcmV0c0NvbXBsZXRlSAASPgoNc3RhcnRfcmVsZWFzZRgDIAEoCzIlLmhlbG1yLnByb2dyYW0udjAuUHJvZ3JhbVN0YXJ0UmVsZWFzZUgAEkEKEmVudHJ5cG9pbnRfcmVsZWFzZRgEIAEoCzIjLmhlbG1yLnByb2dyYW0udjAuRW50cnlwb2ludFJlbGVhc2VIAEIJCgdjb21tYW5kIlUKFVByb2dyYW1Qcm9jZXNzU3RhcnRlZBIOCgZydW5faWQYASABKAkSFgoOYXR0ZW1wdF9udW1iZXIYAiABKA0SFAoMcnVuX2xlYXNlX2lkGAMgASgJInwKGVByb2dyYW1Qcm9jZXNzU3RhcnRGYWlsZWQSDgoGcnVuX2lkGAEgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAIgASgNEhQKDHJ1bl9sZWFzZV9pZBgDIAEoCRINCgVwaGFzZRgEIAEoCRISCgpkaWFnbm9zdGljGAUgASgJIlMKE1Byb2dyYW1TdGFydFJlbGVhc2USDgoGcnVuX2lkGAEgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAIgASgNEhQKDHJ1bl9sZWFzZV9pZBgDIAEoCSKXAQoSRW50cnlwb2ludElkZW50aXR5EhMKC2RlY2xhcmVkX2lkGAEgASgJEjAKBHRhc2sYAiABKAsyIC5oZWxtci5wcm9ncmFtLnYwLlRhc2tFbnRyeXBvaW50SAASMgoFYWN0b3IYAyABKAsyIS5oZWxtci5wcm9ncmFtLnYwLkFjdG9yRW50cnlwb2ludEgAQgYKBGtpbmQiEAoOVGFza0VudHJ5cG9pbnQiEQoPQWN0b3JFbnRyeXBvaW50InMKD0VudHJ5cG9pbnRSZWFkeRIOCgZydW5faWQYASABKAkSFgoOYXR0ZW1wdF9udW1iZXIYAiABKA0SOAoKZW50cnlwb2ludBgDIAEoCzIkLmhlbG1yLnByb2dyYW0udjAuRW50cnlwb2ludElkZW50aXR5InUKEUVudHJ5cG9pbnRSZWxlYXNlEg4KBnJ1bl9pZBgBIAEoCRIWCg5hdHRlbXB0X251bWJlchgCIAEoDRI4CgplbnRyeXBvaW50GAMgASgLMiQuaGVsbXIucHJvZ3JhbS52MC5FbnRyeXBvaW50SWRlbnRpdHki/hMKCFJ1bkV2ZW50EhYKDHN0ZG91dF9jaHVuaxgBIAEoDEgAEhYKDHN0ZGVycl9jaHVuaxgCIAEoDEgAEkAKEnJ1bl93YWl0X3JlcXVlc3RlZBgFIAEoCzIiLmhlbG1yLnByb2dyYW0udjAuUnVuV2FpdFJlcXVlc3RlZEgAEj0KEG1ldGFkYXRhX3VwZGF0ZWQYByABKAsyIS5oZWxtci5wcm9ncmFtLnYwLk1ldGFkYXRhVXBkYXRlZEgAEkgKFnRva2VuX2NyZWF0ZV9yZXF1ZXN0ZWQYCCABKAsyJi5oZWxtci5wcm9ncmFtLnYwLlRva2VuQ3JlYXRlUmVxdWVzdGVkSAASOwoPcmVzdW1lX2NvbnN1bWVkGAYgASgLMiAuaGVsbXIucHJvZ3JhbS52MC5SZXN1bWVDb25zdW1lZEgAEkoKF3Byb2dyYW1fcHJvY2Vzc19zdGFydGVkGAsgASgLMicuaGVsbXIucHJvZ3JhbS52MC5Qcm9ncmFtUHJvY2Vzc1N0YXJ0ZWRIABI9ChBlbnRyeXBvaW50X3JlYWR5GAwgASgLMiEuaGVsbXIucHJvZ3JhbS52MC5FbnRyeXBvaW50UmVhZHlIABI1Cgx0YXNrX291dGNvbWUYDSABKAsyHS5oZWxtci5wcm9ncmFtLnYwLlRhc2tPdXRjb21lSAASPQoQcHJvZ3JhbV9xdWllc2NlZBgOIAEoCzIhLmhlbG1yLnByb2dyYW0udjAuUHJvZ3JhbVF1aWVzY2VkSAASNwoNYWN0b3Jfb3V0Y29tZRgPIAEoCzIeLmhlbG1yLnByb2dyYW0udjAuQWN0b3JPdXRjb21lSAASRgoVdHVybl9zZXR0bGVfcmVxdWVzdGVkGBAgASgLMiUuaGVsbXIucHJvZ3JhbS52MC5UdXJuU2V0dGxlUmVxdWVzdGVkSAASUQobdHVybl9vdXRwdXRfd3JpdGVfcmVxdWVzdGVkGBEgASgLMiouaGVsbXIucHJvZ3JhbS52MC5UdXJuT3V0cHV0V3JpdGVSZXF1ZXN0ZWRIABJMChhzZXNzaW9uX3N1Ym1pdF9yZXF1ZXN0ZWQYEiABKAsyKC5oZWxtci5wcm9ncmFtLnYwLlNlc3Npb25TdWJtaXRSZXF1ZXN0ZWRIABJMChhzdHJ1Y3R1cmVkX2xvZ19yZXF1ZXN0ZWQYEyABKAsyKC5oZWxtci5wcm9ncmFtLnYwLlN0cnVjdHVyZWRMb2dSZXF1ZXN0ZWRIABJRCht0YXNrX2NoaWxkX2ludm9rZV9yZXF1ZXN0ZWQYFCABKAsyKi5oZWxtci5wcm9ncmFtLnYwLlRhc2tDaGlsZEludm9rZVJlcXVlc3RlZEgAEkYKFWFjdG9yX3N0YXJ0X3JlcXVlc3RlZBgVIAEoCzIlLmhlbG1yLnByb2dyYW0udjAuQWN0b3JTdGFydFJlcXVlc3RlZEgAEkwKGHNlc3Npb25fc3RhdHVzX3JlcXVlc3RlZBgWIAEoCzIoLmhlbG1yLnByb2dyYW0udjAuU2Vzc2lvblN0YXR1c1JlcXVlc3RlZEgAEkoKF3Nlc3Npb25fY2xvc2VfcmVxdWVzdGVkGBcgASgLMicuaGVsbXIucHJvZ3JhbS52MC5TZXNzaW9uQ2xvc2VSZXF1ZXN0ZWRIABJMChhzZXNzaW9uX2V2ZW50c19yZXF1ZXN0ZWQYGCABKAsyKC5oZWxtci5wcm9ncmFtLnYwLlNlc3Npb25FdmVudHNSZXF1ZXN0ZWRIABJQChp3b3Jrc3BhY2VfY3JlYXRlX3JlcXVlc3RlZBgZIAEoCzIqLmhlbG1yLnByb2dyYW0udjAuV29ya3NwYWNlQ3JlYXRlUmVxdWVzdGVkSAASVAocd29ya3NwYWNlX3JldHJpZXZlX3JlcXVlc3RlZBgaIAEoCzIsLmhlbG1yLnByb2dyYW0udjAuV29ya3NwYWNlUmV0cmlldmVSZXF1ZXN0ZWRIABJMChh3b3Jrc3BhY2VfZXhlY19yZXF1ZXN0ZWQYHiABKAsyKC5oZWxtci5wcm9ncmFtLnYwLldvcmtzcGFjZUV4ZWNSZXF1ZXN0ZWRIABJQChp3b3Jrc3BhY2VfZGVsZXRlX3JlcXVlc3RlZBgfIAEoCzIqLmhlbG1yLnByb2dyYW0udjAuV29ya3NwYWNlRGVsZXRlUmVxdWVzdGVkSAASUwoccHJvZ3JhbV9wcm9jZXNzX3N0YXJ0X2ZhaWxlZBggIAEoCzIrLmhlbG1yLnByb2dyYW0udjAuUHJvZ3JhbVByb2Nlc3NTdGFydEZhaWxlZEgAElcKHnNlc3Npb25fb3V0cHV0X3dyaXRlX3JlcXVlc3RlZBghIAEoCzItLmhlbG1yLnByb2dyYW0udjAuU2Vzc2lvbk91dHB1dFdyaXRlUmVxdWVzdGVkSAASRAoUdHVybl9yZWFkeV9yZXF1ZXN0ZWQYIiABKAsyJC5oZWxtci5wcm9ncmFtLnYwLlR1cm5SZWFkeVJlcXVlc3RlZEgAElMKHHR1cm5fbWVzc2FnZV9jbGFpbV9yZXF1ZXN0ZWQYIyABKAsyKy5oZWxtci5wcm9ncmFtLnYwLlR1cm5NZXNzYWdlQ2xhaW1SZXF1ZXN0ZWRIABJZCh90dXJuX21lc3NhZ2VfY29tcGxldGVfcmVxdWVzdGVkGCQgASgLMi4uaGVsbXIucHJvZ3JhbS52MC5UdXJuTWVzc2FnZUNvbXBsZXRlUmVxdWVzdGVkSAASWQofdHVybl9zZXR0bGVtZW50X2JlZ2luX3JlcXVlc3RlZBglIAEoCzIuLmhlbG1yLnByb2dyYW0udjAuVHVyblNldHRsZW1lbnRCZWdpblJlcXVlc3RlZEgAElkKH3Nlc3Npb25fdHVybl9yZXRyaWV2ZV9yZXF1ZXN0ZWQYJiABKAsyLi5oZWxtci5wcm9ncmFtLnYwLlNlc3Npb25UdXJuUmV0cmlldmVSZXF1ZXN0ZWRIABJbCiBzZXNzaW9uX3R1cm5faW50ZXJydXB0X3JlcXVlc3RlZBgnIAEoCzIvLmhlbG1yLnByb2dyYW0udjAuU2Vzc2lvblR1cm5JbnRlcnJ1cHRSZXF1ZXN0ZWRIABJMChhzZXNzaW9uX3Jlc3VtZV9yZXF1ZXN0ZWQYKCABKAsyKC5oZWxtci5wcm9ncmFtLnYwLlNlc3Npb25SZXN1bWVSZXF1ZXN0ZWRIABJMChhzZXNzaW9uX2NhbmNlbF9yZXF1ZXN0ZWQYKSABKAsyKC5oZWxtci5wcm9ncmFtLnYwLlNlc3Npb25DYW5jZWxSZXF1ZXN0ZWRIAEIHCgVldmVudEoECAMQBEoECAkQCkoECAoQC0oECBsQHEoECBwQHUoECB0QHiK/AQoLVGFza091dGNvbWUSNAoJc3VjY2VlZGVkGAEgASgLMh8uaGVsbXIucHJvZ3JhbS52MC5UYXNrU3VjY2VlZGVkSAASLgoGZmFpbGVkGAIgASgLMhwuaGVsbXIucHJvZ3JhbS52MC5UYXNrRmFpbGVkSAASPwoPcGF5bG9hZF9pbnZhbGlkGAMgASgLMiQuaGVsbXIucHJvZ3JhbS52MC5UYXNrUGF5bG9hZEludmFsaWRIAEIJCgdvdXRjb21lIiQKDVRhc2tTdWNjZWVkZWQSEwoLb3V0cHV0X2pzb24YASABKAkiSQoKVGFza0ZhaWxlZBIPCgdtZXNzYWdlGAEgASgJEhkKDGRldGFpbHNfanNvbhgCIAEoCUgAiAEBQg8KDV9kZXRhaWxzX2pzb24iUQoSVGFza1BheWxvYWRJbnZhbGlkEg8KB21lc3NhZ2UYASABKAkSGQoMZGV0YWlsc19qc29uGAIgASgJSACIAQFCDwoNX2RldGFpbHNfanNvbiLUAQoMQWN0b3JPdXRjb21lEhYKDnJ1bl9nZW5lcmF0aW9uGAEgASgDEjUKCXN1Y2NlZWRlZBgCIAEoCzIgLmhlbG1yLnByb2dyYW0udjAuQWN0b3JTdWNjZWVkZWRIABIvCgZmYWlsZWQYAyABKAsyHS5oZWxtci5wcm9ncmFtLnYwLkFjdG9yRmFpbGVkSAASOQoLaW50ZXJydXB0ZWQYBCABKAsyIi5oZWxtci5wcm9ncmFtLnYwLkFjdG9ySW50ZXJydXB0ZWRIAEIJCgdvdXRjb21lIhAKDkFjdG9yU3VjY2VlZGVkIkoKC0FjdG9yRmFpbGVkEg8KB21lc3NhZ2UYASABKAkSGQoMZGV0YWlsc19qc29uGAIgASgJSACIAQFCDwoNX2RldGFpbHNfanNvbiJmChBTZXNzaW9uRXhlY3V0aW9uEhIKCnNlc3Npb25faWQYASABKAkSDgoGcnVuX2lkGAIgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAMgASgNEhYKDnJ1bl9nZW5lcmF0aW9uGAQgASgDIlUKDVR1cm5FeGVjdXRpb24SMwoHc2Vzc2lvbhgBIAEoCzIiLmhlbG1yLnByb2dyYW0udjAuU2Vzc2lvbkV4ZWN1dGlvbhIPCgd0dXJuX2lkGAIgASgJIkUKEEFjdG9ySW50ZXJydXB0ZWQSDwoHaG9sZF9pZBgBIAEoCRIUCgd0dXJuX2lkGAIgASgJSACIAQFCCgoIX3R1cm5faWQihwEKC1Nlc3Npb25TdG9wEjUKCWV4ZWN1dGlvbhgBIAEoCzIiLmhlbG1yLnByb2dyYW0udjAuU2Vzc2lvbkV4ZWN1dGlvbhIUCgd0dXJuX2lkGAIgASgJSACIAQESDwoHaG9sZF9pZBgDIAEoCRIOCgZyZWFzb24YBCABKAlCCgoIX3R1cm5faWQiagocVHVyblNldHRsZW1lbnRCZWdpblJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRIyCglleGVjdXRpb24YAiABKAsyHy5oZWxtci5wcm9ncmFtLnYwLlR1cm5FeGVjdXRpb24i5wEKE1R1cm5TZXR0bGVSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSHQoVdGFyZ2V0X2lucHV0X3NlcXVlbmNlGAIgASgDEjIKCWV4ZWN1dGlvbhgDIAEoCzIfLmhlbG1yLnByb2dyYW0udjAuVHVybkV4ZWN1dGlvbhITCgtkaXNwb3NpdGlvbhgEIAEoCRIYCgtyZXN1bHRfanNvbhgFIAEoCUgAiAEBEhcKCmVycm9yX2pzb24YBiABKAlIAYgBAUIOCgxfcmVzdWx0X2pzb25CDQoLX2Vycm9yX2pzb24iYAoSVHVyblJlYWR5UmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEjIKCWV4ZWN1dGlvbhgCIAEoCzIfLmhlbG1yLnByb2dyYW0udjAuVHVybkV4ZWN1dGlvbiJ8ChlUdXJuTWVzc2FnZUNsYWltUmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEjIKCWV4ZWN1dGlvbhgCIAEoCzIfLmhlbG1yLnByb2dyYW0udjAuVHVybkV4ZWN1dGlvbhITCgtkZWxpdmVyeV9pZBgDIAEoCSLdAQocVHVybk1lc3NhZ2VDb21wbGV0ZVJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRIyCglleGVjdXRpb24YAiABKAsyHy5oZWxtci5wcm9ncmFtLnYwLlR1cm5FeGVjdXRpb24SEgoKbWVzc2FnZV9pZBgDIAEoCRITCgtkZWxpdmVyeV9pZBgEIAEoCRIOCgZzdGF0dXMYBSABKAkSDAoEY29kZRgGIAEoCRIZCgxkZXRhaWxzX2pzb24YByABKAlIAIgBAUIPCg1fZGV0YWlsc19qc29uIuUBChhUdXJuT3V0cHV0V3JpdGVSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSMgoJZXhlY3V0aW9uGAIgASgLMh8uaGVsbXIucHJvZ3JhbS52MC5UdXJuRXhlY3V0aW9uEhEKCWRhdGFfanNvbhgDIAEoCRIcCg9pZGVtcG90ZW5jeV9rZXkYBCABKAlIAIgBARIgChNtZXNzYWdlX2RlbGl2ZXJ5X2lkGAUgASgJSAGIAQFCEgoQX2lkZW1wb3RlbmN5X2tleUIWChRfbWVzc2FnZV9kZWxpdmVyeV9pZCKxAQobU2Vzc2lvbk91dHB1dFdyaXRlUmVxdWVzdGVkEhYKDmNvcnJlbGF0aW9uX2lkGAEgASgJEjUKCWV4ZWN1dGlvbhgCIAEoCzIiLmhlbG1yLnByb2dyYW0udjAuU2Vzc2lvbkV4ZWN1dGlvbhIRCglkYXRhX2pzb24YAyABKAkSHAoPaWRlbXBvdGVuY3lfa2V5GAQgASgJSACIAQFCEgoQX2lkZW1wb3RlbmN5X2tleSK5AQoWU2Vzc2lvblN1Ym1pdFJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJEgwKBG1vZGUYAyABKAkSFAoHdHVybl9pZBgEIAEoCUgAiAEBEhEKCWRhdGFfanNvbhgFIAEoCRIcCg9pZGVtcG90ZW5jeV9rZXkYBiABKAlIAYgBAUIKCghfdHVybl9pZEISChBfaWRlbXBvdGVuY3lfa2V5IlsKHFNlc3Npb25UdXJuUmV0cmlldmVSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSEgoKc2Vzc2lvbl9pZBgCIAEoCRIPCgd0dXJuX2lkGAMgASgJIo4BCh1TZXNzaW9uVHVybkludGVycnVwdFJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJEg8KB3R1cm5faWQYAyABKAkSHAoPaWRlbXBvdGVuY3lfa2V5GAQgASgJSACIAQFCEgoQX2lkZW1wb3RlbmN5X2tleSKHAQoWU2Vzc2lvblJlc3VtZVJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJEg8KB2hvbGRfaWQYAyABKAkSHAoPaWRlbXBvdGVuY3lfa2V5GAQgASgJSACIAQFCEgoQX2lkZW1wb3RlbmN5X2tleSK+AQoTQWN0b3JTdGFydFJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRITCgtkZWNsYXJlZF9pZBgCIAEoCRIUCgx3b3Jrc3BhY2VfaWQYAyABKAkSEAoDa2V5GAUgASgJSACIAQESHAoPaWRlbXBvdGVuY3lfa2V5GAcgASgJSAGIAQESGAoQcnVuX29wdGlvbnNfanNvbhgIIAEoCUIGCgRfa2V5QhIKEF9pZGVtcG90ZW5jeV9rZXkiRAoWU2Vzc2lvblN0YXR1c1JlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJInUKFVNlc3Npb25DbG9zZVJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJEhwKD2lkZW1wb3RlbmN5X2tleRgDIAEoCUgAiAEBQhIKEF9pZGVtcG90ZW5jeV9rZXkidgoWU2Vzc2lvbkNhbmNlbFJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJEhwKD2lkZW1wb3RlbmN5X2tleRgDIAEoCUgAiAEBQhIKEF9pZGVtcG90ZW5jeV9rZXkicQoWU2Vzc2lvbkV2ZW50c1JlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRISCgpzZXNzaW9uX2lkGAIgASgJEhIKBWFmdGVyGAMgASgDSACIAQESDQoFbGltaXQYBCABKA1CCAoGX2FmdGVyIigKEFdvcmtzcGFjZUFkZHJlc3MSFAoMd29ya3NwYWNlX2lkGAEgASgJIkcKEFNlY3JldEVudkJpbmRpbmcSDAoEbmFtZRgBIAEoCRIMCgRtb2RlGAIgASgJEhcKD2FsbG93ZWRfb3JpZ2lucxgDIAMoCSIhChFTZWNyZXRGaWxlQmluZGluZxIMCgRwYXRoGAEgASgJIp8BChhXb3Jrc3BhY2VTZWNyZXRQbGFjZW1lbnQSDgoGc2VjcmV0GAEgASgJEjEKA2VudhgCIAEoCzIiLmhlbG1yLnByb2dyYW0udjAuU2VjcmV0RW52QmluZGluZ0gAEjMKBGZpbGUYAyABKAsyIy5oZWxtci5wcm9ncmFtLnYwLlNlY3JldEZpbGVCaW5kaW5nSABCCwoJcGxhY2VtZW50ItABChhXb3Jrc3BhY2VDcmVhdGVSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSEwoLZGVjbGFyZWRfaWQYAiABKAkSEAoDa2V5GAMgASgJSACIAQESOwoHc2VjcmV0cxgEIAMoCzIqLmhlbG1yLnByb2dyYW0udjAuV29ya3NwYWNlU2VjcmV0UGxhY2VtZW50EhwKD2lkZW1wb3RlbmN5X2tleRgFIAEoCUgBiAEBQgYKBF9rZXlCEgoQX2lkZW1wb3RlbmN5X2tleSJrChpXb3Jrc3BhY2VSZXRyaWV2ZVJlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRI1Cgl3b3Jrc3BhY2UYAiABKAsyIi5oZWxtci5wcm9ncmFtLnYwLldvcmtzcGFjZUFkZHJlc3MizgIKFldvcmtzcGFjZUV4ZWNSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSNQoJd29ya3NwYWNlGAIgASgLMiIuaGVsbXIucHJvZ3JhbS52MC5Xb3Jrc3BhY2VBZGRyZXNzEg8KB2NvbW1hbmQYAyADKAkSEAoDY3dkGAQgASgJSACIAQESPgoDZW52GAUgAygLMjEuaGVsbXIucHJvZ3JhbS52MC5Xb3Jrc3BhY2VFeGVjUmVxdWVzdGVkLkVudkVudHJ5Eg0KBXN0ZGluGAYgASgMEhcKCnRpbWVvdXRfbXMYByABKARIAYgBARIXCg9pZGVtcG90ZW5jeV9rZXkYCCABKAkaKgoIRW52RW50cnkSCwoDa2V5GAEgASgJEg0KBXZhbHVlGAIgASgJOgI4AUIGCgRfY3dkQg0KC190aW1lb3V0X21zIpsBChhXb3Jrc3BhY2VEZWxldGVSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSNQoJd29ya3NwYWNlGAIgASgLMiIuaGVsbXIucHJvZ3JhbS52MC5Xb3Jrc3BhY2VBZGRyZXNzEhwKD2lkZW1wb3RlbmN5X2tleRgDIAEoCUgAiAEBQhIKEF9pZGVtcG90ZW5jeV9rZXkiTwoPUHJvZ3JhbVF1aWVzY2VkEg4KBnJ1bl9pZBgBIAEoCRIWCg5hdHRlbXB0X251bWJlchgCIAEoDRIUCgxydW5fbGVhc2VfaWQYAyABKAkivwMKEFJ1bldhaXRSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSDAoEa2luZBgCIAEoCRITCgtwYXJhbXNfanNvbhgDIAEoCRIaCg1tZXRhZGF0YV9qc29uGAQgASgJSACIAQESFwoKdGltZW91dF9tcxgFIAEoBEgBiAEBEgwKBHRhZ3MYBiADKAkSEwoLcnVuX3dhaXRfaWQYByABKAkSGAoQcmVzdW1lX2F0dGFjaF9pZBgIIAEoCRIcCg9pZGxlX3RpbWVvdXRfbXMYCSABKARIAogBARItCiBhY3Rvcl9zcGVjdWxhdGl2ZV9pbnB1dF9zZXF1ZW5jZRgKIAEoA0gDiAEBEjUKCWV4ZWN1dGlvbhgLIAEoCzIiLmhlbG1yLnByb2dyYW0udjAuU2Vzc2lvbkV4ZWN1dGlvbhIUCgd0dXJuX2lkGAwgASgJSASIAQFCEAoOX21ldGFkYXRhX2pzb25CDQoLX3RpbWVvdXRfbXNCEgoQX2lkbGVfdGltZW91dF9tc0IjCiFfYWN0b3Jfc3BlY3VsYXRpdmVfaW5wdXRfc2VxdWVuY2VCCgoIX3R1cm5faWQixAEKFFRva2VuQ3JlYXRlUmVxdWVzdGVkEhcKCnRpbWVvdXRfbXMYASABKARIAIgBARIWCg5jb3JyZWxhdGlvbl9pZBgCIAEoCRIcCg9pZGVtcG90ZW5jeV9rZXkYAyABKAlIAYgBARIMCgR0YWdzGAQgAygJEhoKDW1ldGFkYXRhX2pzb24YBSABKAlIAogBAUINCgtfdGltZW91dF9tc0ISChBfaWRlbXBvdGVuY3lfa2V5QhAKDl9tZXRhZGF0YV9qc29uItgDChhUYXNrQ2hpbGRJbnZva2VSZXF1ZXN0ZWQSFgoOY29ycmVsYXRpb25faWQYASABKAkSEwoLZGVjbGFyZWRfaWQYAiABKAkSDgoGbWV0aG9kGAMgASgJEhcKD3BheWxvYWRfcHJlc2VudBgEIAEoCBIZCgxwYXlsb2FkX2pzb24YBSABKAlIAIgBARIWCg53b3Jrc3BhY2VfanNvbhgGIAEoCRIUCgxvcHRpb25zX2pzb24YByABKAkSHAoPaWRlbXBvdGVuY3lfa2V5GAggASgJSAGIAQESLQogYWN0b3Jfc3BlY3VsYXRpdmVfaW5wdXRfc2VxdWVuY2UYCSABKANIAogBARITCgtydW5fd2FpdF9pZBgKIAEoCRIYChByZXN1bWVfYXR0YWNoX2lkGAsgASgJEjUKCWV4ZWN1dGlvbhgMIAEoCzIiLmhlbG1yLnByb2dyYW0udjAuU2Vzc2lvbkV4ZWN1dGlvbhIUCgd0dXJuX2lkGA0gASgJSAOIAQFCDwoNX3BheWxvYWRfanNvbkISChBfaWRlbXBvdGVuY3lfa2V5QiMKIV9hY3Rvcl9zcGVjdWxhdGl2ZV9pbnB1dF9zZXF1ZW5jZUIKCghfdHVybl9pZCKxAgoWQ2hlY2twb2ludFBhdXNlUmVxdWVzdBITCgtydW5fd2FpdF9pZBgBIAEoCRIVCg1jaGVja3BvaW50X2lkGAIgASgJEg4KBnJ1bl9pZBgEIAEoCRIWCg5hdHRlbXB0X251bWJlchgFIAEoDRIUCgxydW5fbGVhc2VfaWQYBiABKAkSGAoQcmVzdW1lX2F0dGFjaF9pZBgHIAEoCRIiChpjaGVja3BvaW50X3JlcXVlc3RfdmVyc2lvbhgIIAEoAxIWCg5jb3JyZWxhdGlvbl9pZBgJIAEoCRI1CglleGVjdXRpb24YCiABKAsyIi5oZWxtci5wcm9ncmFtLnYwLlNlc3Npb25FeGVjdXRpb24SFAoHdHVybl9pZBgLIAEoCUgAiAEBQgoKCF90dXJuX2lkIqMCCgxSZXN1bWVBdHRhY2gSFQoNY2hlY2twb2ludF9pZBgBIAEoCRITCgtydW5fd2FpdF9pZBgCIAEoCRIUCgxydW5fbGVhc2VfaWQYAyABKAkSDgoGcnVuX2lkGAQgASgJEhYKDmF0dGVtcHRfbnVtYmVyGAUgASgNEhgKEHJlc3VtZV9hdHRhY2hfaWQYBiABKAkSHgoWcmVzdW1lX3JlcXVlc3RfdmVyc2lvbhgHIAEoAxIWCg5jb3JyZWxhdGlvbl9pZBgIIAEoCRI1CglleGVjdXRpb24YCSABKAsyIi5oZWxtci5wcm9ncmFtLnYwLlNlc3Npb25FeGVjdXRpb24SFAoHdHVybl9pZBgKIAEoCUgAiAEBQgoKCF90dXJuX2lkIvYBCg5SZXN1bWVEZWNpc2lvbhITCgtydW5fd2FpdF9pZBgBIAEoCRIMCgRraW5kGAIgASgJEhEKCWRhdGFfanNvbhgDIAEoCRIcChRyZXF1aXJlX2NvbnN1bWVkX2FjaxgEIAEoCBIVCg1jaGVja3BvaW50X2lkGAUgASgJEhgKEHJlc3VtZV9hdHRhY2hfaWQYBiABKAkSHgoWcmVzdW1lX3JlcXVlc3RfdmVyc2lvbhgHIAEoAxIUCgxydW5fbGVhc2VfaWQYCCABKAkSFgoOY29ycmVsYXRpb25faWQYCSABKAkSEQoJbm9fcmVzdWx0GAogASgIIp8BCglSZXN1bWVBY2sSEwoLcnVuX3dhaXRfaWQYASABKAkSFQoNY2hlY2twb2ludF9pZBgCIAEoCRIYChByZXN1bWVfYXR0YWNoX2lkGAMgASgJEh4KFnJlc3VtZV9yZXF1ZXN0X3ZlcnNpb24YBCABKAMSFAoMcnVuX2xlYXNlX2lkGAUgASgJEhYKDmNvcnJlbGF0aW9uX2lkGAYgASgJIqQBCg5SZXN1bWVDb25zdW1lZBITCgtydW5fd2FpdF9pZBgBIAEoCRIVCg1jaGVja3BvaW50X2lkGAIgASgJEhgKEHJlc3VtZV9hdHRhY2hfaWQYAyABKAkSHgoWcmVzdW1lX3JlcXVlc3RfdmVyc2lvbhgEIAEoAxIUCgxydW5fbGVhc2VfaWQYBSABKAkSFgoOY29ycmVsYXRpb25faWQYBiABKAkixgEKD01ldGFkYXRhVXBkYXRlZBIRCglvcGVyYXRpb24YASABKAkSEAoDa2V5GAIgASgJSACIAQESFwoKdmFsdWVfanNvbhgDIAEoCUgBiAEBEhcKCnBhdGNoX2pzb24YBCABKAlIAogBARITCgZhbW91bnQYBSABKAFIA4gBARIWCg5jb3JyZWxhdGlvbl9pZBgGIAEoCUIGCgRfa2V5Qg0KC192YWx1ZV9qc29uQg0KC19wYXRjaF9qc29uQgkKB19hbW91bnQiaQoWU3RydWN0dXJlZExvZ1JlcXVlc3RlZBIWCg5jb3JyZWxhdGlvbl9pZBgBIAEoCRINCgVsZXZlbBgCIAEoCRIPCgdtZXNzYWdlGAMgASgJEhcKD2F0dHJpYnV0ZXNfanNvbhgEIAEoCUJCWkBnaXRodWIuY29tL2hlbG1yZG90ZGV2L2hlbG1yL2ludGVybmFsL3Byb3RvL3Byb2dyYW0vdjA7cHJvZ3JhbXYwYgZwcm90bzM");
var ProgramStartSchema = /* @__PURE__ */ messageDesc(file_program, 0);
var TaskStartSchema = /* @__PURE__ */ messageDesc(file_program, 1);
var NoPayloadSchema = /* @__PURE__ */ messageDesc(file_program, 2);
var ActorStartSchema = /* @__PURE__ */ messageDesc(file_program, 3);
var RunCauseSchema = /* @__PURE__ */ messageDesc(file_program, 4);
var ApiCauseSchema = /* @__PURE__ */ messageDesc(file_program, 5);
var ManualCauseSchema = /* @__PURE__ */ messageDesc(file_program, 6);
var ChildCauseSchema = /* @__PURE__ */ messageDesc(file_program, 7);
var ScheduleCauseSchema = /* @__PURE__ */ messageDesc(file_program, 8);
var ActorStartCauseSchema = /* @__PURE__ */ messageDesc(file_program, 9);
var ContinuationCauseSchema = /* @__PURE__ */ messageDesc(file_program, 10);
var ProgramRunRequestSchema = /* @__PURE__ */ messageDesc(file_program, 11);
var ProgramSecretSchema = /* @__PURE__ */ messageDesc(file_program, 12);
var ProgramSecretsCompleteSchema = /* @__PURE__ */ messageDesc(file_program, 13);
var ProgramSupervisorCommandSchema = /* @__PURE__ */ messageDesc(file_program, 14);
var ProgramProcessStartedSchema = /* @__PURE__ */ messageDesc(file_program, 15);
var ProgramProcessStartFailedSchema = /* @__PURE__ */ messageDesc(file_program, 16);
var ProgramStartReleaseSchema = /* @__PURE__ */ messageDesc(file_program, 17);
var EntrypointIdentitySchema = /* @__PURE__ */ messageDesc(file_program, 18);
var TaskEntrypointSchema = /* @__PURE__ */ messageDesc(file_program, 19);
var ActorEntrypointSchema = /* @__PURE__ */ messageDesc(file_program, 20);
var EntrypointReadySchema = /* @__PURE__ */ messageDesc(file_program, 21);
var EntrypointReleaseSchema = /* @__PURE__ */ messageDesc(file_program, 22);
var RunEventSchema = /* @__PURE__ */ messageDesc(file_program, 23);
var TaskOutcomeSchema = /* @__PURE__ */ messageDesc(file_program, 24);
var TaskSucceededSchema = /* @__PURE__ */ messageDesc(file_program, 25);
var TaskFailedSchema = /* @__PURE__ */ messageDesc(file_program, 26);
var TaskPayloadInvalidSchema = /* @__PURE__ */ messageDesc(file_program, 27);
var ActorOutcomeSchema = /* @__PURE__ */ messageDesc(file_program, 28);
var ActorSucceededSchema = /* @__PURE__ */ messageDesc(file_program, 29);
var ActorFailedSchema = /* @__PURE__ */ messageDesc(file_program, 30);
var SessionExecutionSchema = /* @__PURE__ */ messageDesc(file_program, 31);
var TurnExecutionSchema = /* @__PURE__ */ messageDesc(file_program, 32);
var ActorInterruptedSchema = /* @__PURE__ */ messageDesc(file_program, 33);
var SessionStopSchema = /* @__PURE__ */ messageDesc(file_program, 34);
var TurnSettlementBeginRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 35);
var TurnSettleRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 36);
var TurnReadyRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 37);
var TurnMessageClaimRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 38);
var TurnMessageCompleteRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 39);
var TurnOutputWriteRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 40);
var SessionOutputWriteRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 41);
var SessionSubmitRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 42);
var SessionTurnRetrieveRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 43);
var SessionTurnInterruptRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 44);
var SessionResumeRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 45);
var ActorStartRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 46);
var SessionStatusRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 47);
var SessionCloseRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 48);
var SessionCancelRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 49);
var SessionEventsRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 50);
var WorkspaceAddressSchema = /* @__PURE__ */ messageDesc(file_program, 51);
var SecretEnvBindingSchema = /* @__PURE__ */ messageDesc(file_program, 52);
var SecretFileBindingSchema = /* @__PURE__ */ messageDesc(file_program, 53);
var WorkspaceSecretPlacementSchema = /* @__PURE__ */ messageDesc(file_program, 54);
var WorkspaceCreateRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 55);
var WorkspaceRetrieveRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 56);
var WorkspaceExecRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 57);
var WorkspaceDeleteRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 58);
var ProgramQuiescedSchema = /* @__PURE__ */ messageDesc(file_program, 59);
var RunWaitRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 60);
var TokenCreateRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 61);
var TaskChildInvokeRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 62);
var CheckpointPauseRequestSchema = /* @__PURE__ */ messageDesc(file_program, 63);
var ResumeAttachSchema = /* @__PURE__ */ messageDesc(file_program, 64);
var ResumeDecisionSchema = /* @__PURE__ */ messageDesc(file_program, 65);
var ResumeAckSchema = /* @__PURE__ */ messageDesc(file_program, 66);
var ResumeConsumedSchema = /* @__PURE__ */ messageDesc(file_program, 67);
var MetadataUpdatedSchema = /* @__PURE__ */ messageDesc(file_program, 68);
var StructuredLogRequestedSchema = /* @__PURE__ */ messageDesc(file_program, 69);

// sdk/typescript/src/internal/utf8.ts
var encoder = new TextEncoder();
var encode = TextEncoder.prototype.encode.call.bind(
  TextEncoder.prototype.encode
);
var charCodeAt = String.prototype.charCodeAt.call.bind(
  String.prototype.charCodeAt
);
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
var arrayPrototype = Array.prototype;
var objectPrototype = Object.prototype;
var hasOwn = Object.hasOwn;
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
function isAsciiAlnum(code) {
  return code >= 48 && code <= 57 || code >= 65 && code <= 90 || code >= 97 && code <= 122;
}

// sdk/typescript/src/internal/runtime.ts
var runtimeOperationsSymbol = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.runtime_operations");
function installRuntimeOperations(operations) {
  const target = globalThis;
  if (target[runtimeOperationsSymbol] !== void 0) {
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
var rejectedBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.MessageRejected");
var MessageRejected = class _MessageRejected extends Error {
  details;
  constructor(message, details) {
    super(message);
    this.name = "MessageRejected";
    if (details !== void 0) this.details = details;
    Object.defineProperty(this, rejectedBrand, { value: true });
  }
  static [Symbol.hasInstance](value) {
    return this === _MessageRejected && typeof value === "object" && value !== null && rejectedBrand in value;
  }
};
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

// sdk/typescript/src/internal/run-handle.ts
function createRunHandle(id) {
  return Object.freeze({
    id: resourceID(id, "Run ID")
  });
}

// sdk/typescript/src/definitions.ts
var privateDefinitionBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.definition");
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
var sourceFileBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.source-file");
var sourceDirectoryBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.source-directory");
var SourceFileValue = class {
  path;
  constructor(path2) {
    this.path = path2;
    Object.defineProperty(this, sourceFileBrand, { value: true });
    Object.freeze(this);
  }
};
var SourceDirectoryValue = class {
  path;
  constructor(path2) {
    this.path = path2;
    Object.defineProperty(this, sourceDirectoryBrand, { value: true });
    Object.freeze(this);
  }
};
var source = Object.freeze({
  file(path2) {
    return new SourceFileValue(path2);
  },
  directory(path2) {
    return new SourceDirectoryValue(path2);
  }
});

// sdk/typescript/src/secret.ts
var secretNamePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/;
function validateSecretName(value) {
  if (typeof value !== "string" || !secretNamePattern.test(value)) {
    throw new Error("Secret name is invalid");
  }
}

// sdk/typescript/src/internal/origin.ts
function canonicalSecretOrigin(value) {
  if (typeof value !== "string" || value.trim() !== value || !/^https:\/\//i.test(value)) {
    throw new Error("Secret origin must be an exact HTTPS origin");
  }
  const authority = value.replace(/^https:\/\//i, "").replace(/\/$/, "");
  if (!/^[A-Za-z0-9.-]+(?::[0-9]+)?$/.test(authority)) throw new Error("Secret origin must contain only a DNS hostname and optional port");
  const [rawHost, port] = authority.split(":");
  const host = rawHost.toLowerCase();
  if (host.length > 253 || host === "localhost" || host.endsWith(".localhost") || /^[0-9.]+$/.test(host) || host.split(".").some((label) => !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label))) {
    throw new Error("Secret origin must use a DNS hostname without wildcards or IP addresses");
  }
  if (port !== void 0 && (!/^[1-9][0-9]*$/.test(port) || Number(port) > 65535)) throw new Error("Secret origin port is invalid");
  return `https://${host}${port === void 0 || port === "443" ? "" : `:${port}`}`;
}

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
var workspaceAddressBrand = /* @__PURE__ */ Symbol.for("helmr.sdk.v0.workspace-address");
var workspaces = Object.freeze({
  ref: createWorkspaceRef
});
function inspectWorkspaceAddress(value) {
  if (typeof value !== "object" || value === null || value[workspaceAddressBrand] !== true) {
    return void 0;
  }
  const address = value;
  if (address.id === void 0) {
    throw new Error("private Workspace address is invalid");
  }
  return createWorkspaceAddress(address.id);
}
function workspaceRefID(value) {
  const address = inspectWorkspaceAddress(value);
  if (address === void 0 || typeof address.id !== "string") {
    throw new Error("Workspace requires a Workspace ref");
  }
  return address.id;
}
function encodeWorkspaceSecrets(inputs) {
  if (inputs === void 0) return Object.freeze([]);
  if (!Array.isArray(inputs) || inputs.length > 64) throw new Error("Workspace secrets must be an array of at most 64 bindings");
  const envNames = /* @__PURE__ */ new Set();
  const files = [];
  let originCount = 0;
  const result = inputs.map((input) => {
    const value = workspaceObject(input, "Workspace Secret");
    validateSecretName(value["secret"]);
    const hasEnv = value["env"] !== void 0;
    const hasFile = value["file"] !== void 0;
    if (hasEnv === hasFile) throw new Error("Workspace Secret requires exactly one of env or file");
    exactBindingKeys(value, ["secret", "env", "file"]);
    if (hasEnv) {
      const env = workspaceObject(value["env"], "Workspace Secret env");
      const mode = env["mode"];
      if (mode !== "raw" && mode !== "protected") throw new Error("Workspace Secret env requires explicit raw or protected mode");
      exactBindingKeys(env, ["name", "mode", "allowedOrigins"]);
      if (mode === "raw" && env["allowedOrigins"] !== void 0) throw new Error("Raw env cannot specify allowedOrigins");
      const name = env["name"];
      if (typeof name !== "string" || !/^[A-Za-z_][A-Za-z0-9_]*$/.test(name) || name.startsWith("HELMR_") || name.startsWith("LD_") || ["NODE_OPTIONS", "NODE_PATH", "NODE_ICU_DATA", "OPENSSL_CONF", "OPENSSL_MODULES", "OPENSSL_ENGINES", "GCONV_PATH", "LOCPATH"].includes(name) || ["SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "NODE_USE_SYSTEM_CA"].includes(name.toUpperCase())) throw new Error("Invalid or reserved Secret env name");
      if (envNames.has(name)) throw new Error(`Duplicate Secret env target ${name}`);
      envNames.add(name);
      if (mode === "raw") return Object.freeze({ secret: input.secret, env: Object.freeze({ name, mode }) });
      const origins = env["allowedOrigins"];
      if (!Array.isArray(origins) || origins.length === 0 || origins.length > 16) throw new Error("Protected env requires 1 to 16 exact HTTPS origins");
      const allowed_origins = Object.freeze([...new Set(origins.map(canonicalSecretOrigin))].sort());
      originCount += allowed_origins.length;
      return Object.freeze({ secret: input.secret, env: Object.freeze({ name, mode, allowed_origins }) });
    }
    const file = workspaceObject(value["file"], "Workspace Secret file");
    exactBindingKeys(file, ["path"]);
    const path2 = file["path"];
    if (typeof path2 !== "string" || path2.length > 4096 || !path2.startsWith("/") || path2 === "/" || path2.includes("\0") || path2.split("/").slice(1).some((part) => part === "" || part === "." || part === "..") || ["/workspace", "/var/lib/helmr", "/dev", "/opt/helmr", "/proc", "/sys", "/.helmr-old-root", "/run/helmr"].some((root) => path2 === root || path2.startsWith(root + "/"))) throw new Error("Invalid or reserved Secret file path");
    files.push(path2);
    return Object.freeze({ secret: input.secret, file: Object.freeze({ path: path2 }) });
  });
  files.sort();
  if (files.some((path2, index) => index > 0 && (path2 === files[index - 1] || path2.startsWith(files[index - 1] + "/")))) throw new Error("Conflicting Secret file paths");
  if (originCount > 256) throw new Error("Workspace Secret origins exceed 256");
  return Object.freeze(result);
}
function exactBindingKeys(value, allowed) {
  const unknown = Object.keys(value).find((key) => !allowed.includes(key));
  if (unknown !== void 0) throw new Error(`Workspace Secret has unknown member ${JSON.stringify(unknown)}`);
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
  if (key !== void 0 && typeof key !== "string") {
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
  const owner = parseWorkspaceOwner(input["owner"], "Workspace response.owner");
  return Object.freeze({
    id: resourceID(input["id"], "Workspace response.id"),
    ...key === void 0 ? {} : { key },
    sandboxId,
    deploymentId: resourceID(input["deployment_id"], "Workspace response.deployment_id"),
    status,
    ...owner === void 0 ? {} : { owner },
    secrets: Object.freeze(input["secrets"].map(parseWorkspaceSecret)),
    lastActivityAt: workspaceTimestamp(input["last_activity_at"], "last_activity_at"),
    createdAt: workspaceTimestamp(input["created_at"], "created_at"),
    updatedAt: workspaceTimestamp(input["updated_at"], "updated_at")
  });
}
function parseWorkspaceOwner(value, label) {
  if (value === void 0) return void 0;
  const input = workspaceObject(value, label);
  const hasSession = input["session_id"] !== void 0;
  const hasRun = input["run_id"] !== void 0;
  if (hasSession === hasRun) {
    throw new Error(`${label} must name exactly one of session_id or run_id`);
  }
  return Object.freeze(
    hasSession ? { sessionId: resourceID(input["session_id"], `${label}.session_id`) } : { runId: resourceID(input["run_id"], `${label}.run_id`) }
  );
}
function parseWorkspaceSecret(value) {
  const wire = workspaceObject(value, "Workspace Secret");
  exactBindingKeys(wire, wire["env"] !== void 0 ? ["secret", "env"] : ["secret", "file"]);
  let binding;
  if (wire["env"] !== void 0) {
    const env = workspaceObject(wire["env"], "Workspace Secret env");
    exactBindingKeys(env, ["name", "mode", "allowed_origins"]);
    binding = { secret: wire["secret"], env: { name: env["name"], mode: env["mode"], ...env["allowed_origins"] === void 0 ? {} : { allowedOrigins: env["allowed_origins"] } } };
  } else {
    binding = { secret: wire["secret"], file: wire["file"] };
  }
  const normalized = encodeWorkspaceSecrets([binding])[0];
  return normalized.env !== void 0 ? Object.freeze({ secret: normalized.secret, env: Object.freeze({ name: normalized.env.name, mode: normalized.env.mode, ...normalized.env.allowed_origins === void 0 ? {} : { allowedOrigins: normalized.env.allowed_origins } }) }) : Object.freeze({ secret: normalized.secret, file: normalized.file });
}
function parseWorkspaceExecResult(value) {
  const response = workspaceObject(value, "Workspace exec response");
  const exitCode = response["exit_code"];
  if (!Number.isSafeInteger(exitCode)) {
    throw new Error("Workspace exec response.exit_code must be an integer");
  }
  return Object.freeze({
    exitCode,
    stdout: decodeWorkspaceBase64(
      response["stdout_base64"],
      "Workspace exec response.stdout_base64"
    ),
    stderr: decodeWorkspaceBase64(
      response["stderr_base64"],
      "Workspace exec response.stderr_base64"
    )
  });
}
function parseWorkspaceDeleteReceipt(value) {
  const response = workspaceObject(value, "Workspace delete response");
  return Object.freeze({
    workspaceId: resourceID(
      response["workspace_id"],
      "Workspace delete response.workspace_id"
    )
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
  for (let index = 0; index < binary.length; index++) {
    output[index] = binary.charCodeAt(index);
  }
  return output;
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
    const objectValue2 = value;
    assertPlainObject(objectValue2);
    const entries = Object.keys(objectValue2).sort().map((key) => {
      assertUnicodeString(key);
      return `${JSON.stringify(key)}:${serialize(objectValue2[key], ancestors)}`;
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

// sdk/typescript/src/internal/session.ts
function parseSession(value) {
  const v = objectValue(value, "Session");
  const status = sessionStatus(v["status"]);
  const actorId = requiredString(v["actor_id"], "actor_id");
  validateTaskId(actorId);
  const failure = v["failure"] === void 0 ? void 0 : parseSessionFailure(v["failure"]);
  if (status === "failed" !== (failure !== void 0))
    throw new Error("Session response has an inconsistent failure projection");
  return Object.freeze({
    id: resourceID(v["id"], "Session.id"),
    actorId,
    deploymentId: resourceID(v["deployment_id"], "Session.deployment_id"),
    workspaceId: resourceID(v["workspace_id"], "Session.workspace_id"),
    ...v["key"] === void 0 ? {} : { key: requiredString(v["key"], "Session.key") },
    status,
    ...v["cancel_requested_at"] === void 0 ? {} : { cancelRequestedAt: timestampString(v["cancel_requested_at"], "Session.cancel_requested_at") },
    createdAt: timestampString(v["created_at"], "Session.created_at"),
    updatedAt: timestampString(v["updated_at"], "Session.updated_at"),
    currentRunId: nullableID(v["current_run_id"], "Session.current_run_id"),
    activeTurnId: nullableID(v["active_turn_id"], "Session.active_turn_id"),
    dispatch: parseDispatch(v["dispatch"]),
    ...failure === void 0 ? {} : { failure }
  });
}
function parseDispatch(value) {
  const v = objectValue(value, "Session.dispatch");
  if (v["state"] === "ready") return Object.freeze({ state: "ready" });
  if (v["state"] !== "held" || ![
    "interrupt_requested",
    "interrupted",
    "recovery_required",
    "recovered"
  ].includes(v["reason"]))
    throw new Error("Session.dispatch is invalid");
  return Object.freeze({
    state: "held",
    holdId: resourceID(v["hold_id"], "Session.dispatch.hold_id"),
    reason: v["reason"]
  });
}
function parseTurnSource(value) {
  const v = objectValue(value, "Turn.source");
  if (v["type"] === "external") return Object.freeze({ type: "external" });
  if (v["type"] === "run")
    return Object.freeze({
      type: "run",
      runId: resourceID(v["run_id"], "Turn.source.run_id")
    });
  throw new Error("Turn.source.type is invalid");
}
function parseTurnState(value) {
  const v = objectValue(value, "Turn");
  if (!["queued", "running", "completed", "failed", "interrupted", "cancelled"].includes(
    v["status"]
  ))
    throw new Error("Turn.status is invalid");
  return Object.freeze({
    id: resourceID(v["id"], "Turn.id"),
    sessionId: resourceID(v["session_id"], "Turn.session_id"),
    sequence: safeSequence(v["sequence"], "Turn.sequence"),
    input: jsonValue(v["input"]),
    source: parseTurnSource(v["source"]),
    status: v["status"],
    createdAt: timestampString(v["created_at"], "Turn.created_at"),
    interruptRequested: booleanValue(
      v["interrupt_requested"],
      "Turn.interrupt_requested"
    ),
    acceptsMessages: booleanValue(
      v["accepts_messages"],
      "Turn.accepts_messages"
    ),
    ...v["terminal_event_id"] === void 0 ? {} : {
      terminalEventId: resourceID(
        v["terminal_event_id"],
        "Turn.terminal_event_id"
      )
    },
    ...v["workspace_version_id"] === void 0 ? {} : {
      workspaceVersionId: resourceID(
        v["workspace_version_id"],
        "Turn.workspace_version_id"
      )
    },
    ...Object.hasOwn(v, "result") ? { result: jsonValue(v["result"]) } : {},
    ...Object.hasOwn(v, "error") ? { error: jsonValue(v["error"]) } : {}
  });
}
function parseSessionAdmissionReceipt(value) {
  const v = objectValue(value, "Session admission");
  const id = resourceID(v["id"], "Session admission.id"), turnId = resourceID(v["turn_id"], "Session admission.turn_id");
  if (v["kind"] === "enqueued")
    return Object.freeze({ id, kind: v["kind"], turnId });
  if (v["kind"] === "messaged")
    return Object.freeze({
      id,
      kind: v["kind"],
      turnId,
      messageId: resourceID(v["message_id"], "Session admission.message_id")
    });
  throw new Error("Session admission.kind is invalid");
}
function parseSessionMessageReceipt(value) {
  const v = objectValue(value, "Message receipt");
  if (v["status"] !== "accepted")
    throw new Error("Message receipt.status is invalid");
  return Object.freeze({
    id: resourceID(v["id"], "Message receipt.id"),
    turnId: resourceID(v["turn_id"], "Message receipt.turn_id"),
    messageId: resourceID(v["message_id"], "Message receipt.message_id"),
    status: v["status"]
  });
}
function parseSessionCloseReceipt(value) {
  const v = objectValue(value, "Close receipt");
  if (v["status"] !== "accepted")
    throw new Error("Close receipt.status is invalid");
  return Object.freeze({
    id: resourceID(v["id"], "Close receipt.id"),
    sessionId: resourceID(v["session_id"], "Close receipt.session_id"),
    status: v["status"]
  });
}
function parseSessionCancelReceipt(value) {
  const v = objectValue(value, "Cancel receipt");
  if (v["status"] !== "accepted")
    throw new Error("Cancel receipt.status is invalid");
  return Object.freeze({
    id: resourceID(v["id"], "Cancel receipt.id"),
    sessionId: resourceID(v["session_id"], "Cancel receipt.session_id"),
    status: v["status"]
  });
}
function parseTurnInterruptReceipt(value) {
  const v = objectValue(value, "Interrupt receipt");
  if (v["status"] !== "accepted")
    throw new Error("Interrupt receipt.status is invalid");
  return Object.freeze({
    id: resourceID(v["id"], "Interrupt receipt.id"),
    sessionId: resourceID(v["session_id"], "Interrupt receipt.session_id"),
    turnId: resourceID(v["turn_id"], "Interrupt receipt.turn_id"),
    holdId: resourceID(v["hold_id"], "Interrupt receipt.hold_id"),
    status: v["status"]
  });
}
function parseSessionResumeReceipt(value) {
  const v = objectValue(value, "Resume receipt");
  if (v["status"] !== "accepted")
    throw new Error("Resume receipt.status is invalid");
  return Object.freeze({
    id: resourceID(v["id"], "Resume receipt.id"),
    sessionId: resourceID(v["session_id"], "Resume receipt.session_id"),
    holdId: resourceID(v["hold_id"], "Resume receipt.hold_id"),
    status: v["status"]
  });
}
function parseOutputReceipt(value) {
  const v = objectValue(value, "Output receipt");
  return Object.freeze({
    id: resourceID(v["id"], "Output receipt.id"),
    sessionId: resourceID(v["session_id"], "Output receipt.session_id"),
    turnId: nullableID(v["turn_id"], "Output receipt.turn_id"),
    sequence: safeSequence(v["sequence"], "Output receipt.sequence"),
    runId: resourceID(v["run_id"], "Output receipt.run_id"),
    attemptNumber: safeSequence(
      v["attempt_number"],
      "Output receipt.attempt_number"
    ),
    runGeneration: safeSequence(
      v["run_generation"],
      "Output receipt.run_generation"
    )
  });
}
var eventKinds = [
  "output",
  "turn.enqueued",
  "turn.started",
  "turn.interrupt_requested",
  "turn.completed",
  "turn.failed",
  "turn.interrupted",
  "message.accepted",
  "message.handled",
  "message.rejected",
  "message.unknown",
  "session.closing",
  "session.closed",
  "session.cancel_requested",
  "turn.cancelled",
  "session.failed",
  "session.held",
  "session.resumed",
  "session.recovered"
];
function parseSessionEvent(value) {
  const v = objectValue(value, "Session event");
  if (!eventKinds.includes(v["kind"]))
    throw new Error("Session event.kind is invalid");
  const p = v["provenance"] === null ? null : objectValue(v["provenance"], "Session event.provenance");
  return Object.freeze({
    id: resourceID(v["id"], "Session event.id"),
    sessionId: resourceID(v["session_id"], "Session event.session_id"),
    turnId: nullableID(v["turn_id"], "Session event.turn_id"),
    sequence: safeSequence(v["sequence"], "Session event.sequence"),
    createdAt: timestampString(v["created_at"], "Session event.created_at"),
    kind: v["kind"],
    data: jsonValue(v["data"]),
    provenance: p === null ? null : Object.freeze({
      runId: resourceID(p["run_id"], "provenance.run_id"),
      attemptNumber: safeSequence(
        p["attempt_number"],
        "provenance.attempt_number"
      ),
      runGeneration: safeSequence(
        p["run_generation"],
        "provenance.run_generation"
      ),
      deploymentId: resourceID(
        p["deployment_id"],
        "provenance.deployment_id"
      )
    })
  });
}
function parseSessionEventPage(value) {
  const v = objectValue(value, "Session events");
  if (!Array.isArray(v["records"]))
    throw new Error("Session events.records must be an array");
  return Object.freeze({
    records: Object.freeze(v["records"].map(parseSessionEvent)),
    nextAfter: safeSequence(v["next_after"], "Session events.next_after"),
    hasMore: booleanValue(v["has_more"], "Session events.has_more"),
    retainedAfter: safeSequence(
      v["retained_after"],
      "Session events.retained_after"
    )
  });
}
function parseSessionFailure(value) {
  const v = objectValue(value, "Session failure"), d = objectValue(v["details"], "Session failure.details");
  return Object.freeze({
    code: requiredString(v["code"], "Session failure.code"),
    message: requiredString(v["message"], "Session failure.message"),
    details: Object.freeze(
      d["run_id"] === void 0 ? {} : { runId: resourceID(d["run_id"], "Session failure.details.run_id") }
    )
  });
}
function sessionStatus(value, label = "Session.status") {
  if (value !== "open" && value !== "closing" && value !== "closed" && value !== "failed")
    throw new Error(`${label} is invalid`);
  return value;
}
function nullableID(value, label) {
  return value === null ? null : resourceID(value, label);
}
function jsonValue(value) {
  canonicalizeJsonValue(value);
  return value;
}
function requiredString(value, label) {
  if (typeof value !== "string" || value.length === 0)
    throw new Error(`${label} must be a non-empty string`);
  return value;
}
function booleanValue(value, label) {
  if (typeof value !== "boolean") throw new Error(`${label} must be a boolean`);
  return value;
}
function objectValue(value, label) {
  if (value === null || typeof value !== "object" || Array.isArray(value))
    throw new Error(`${label} must be an object`);
  return value;
}
function safeSequence(value, label) {
  if (!Number.isSafeInteger(value) || value < 0)
    throw new Error(`${label} must be a non-negative safe integer`);
  return value;
}

// sdk/typescript/src/internal/strings.ts
var goSpaceEdges = /^[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/gu;
function trimGoSpace(value) {
  return value.replace(goSpaceEdges, "");
}

// runtime/typescript/src/program.ts
import { createWriteStream, promises as fs } from "node:fs";
import { randomUUIDv7 as newUUIDv7 } from "node:crypto";
import { AsyncLocalStorage } from "node:async_hooks";
import { Readable } from "node:stream";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
var MAX_PROGRAM_FRAME_BYTES = 256 * 1024 * 1024;
var MAX_TASK_OUTPUT_BYTES = 16 * 1024 * 1024;
var MAX_TASK_ERROR_BYTES = 16 * 1024;
var MAX_RUN_LOG_MESSAGE_BYTES = 4 * 1024;
var MAX_RUN_LOG_ATTRIBUTES_BYTES = 16 * 1024;
var MAX_TASK_ERROR_MESSAGE_BYTES = 1024;
var MAX_ACTOR_INPUT_BYTES = 1 * 1024 * 1024;
var FrameReader = class {
  #iterator;
  #stream;
  #closePromise;
  #chunk = new Uint8Array();
  #offset = 0;
  constructor(input) {
    this.#iterator = input[Symbol.asyncIterator]();
    this.#stream = input instanceof Readable ? input : void 0;
  }
  async read(maxBytes = MAX_PROGRAM_FRAME_BYTES) {
    const header = await this.#readExact(4);
    const size = new DataView(
      header.buffer,
      header.byteOffset,
      header.byteLength
    ).getUint32(0);
    if (size > maxBytes) {
      throw new Error(`runtime frame length ${size} exceeds max ${maxBytes}`);
    }
    return this.#readExact(size);
  }
  close() {
    if (this.#closePromise !== void 0) return this.#closePromise;
    this.#closePromise = this.#closeIterator();
    return this.#closePromise;
  }
  async #closeIterator() {
    this.#stream?.destroy();
    const close = this.#iterator.return;
    if (close !== void 0) await close.call(this.#iterator);
  }
  async #readExact(size) {
    const result = new Uint8Array(size);
    let written = 0;
    while (written < size) {
      if (this.#offset === this.#chunk.byteLength) {
        const next = await this.#iterator.next();
        if (next.done) {
          throw new Error(
            `runtime frame ended after ${written} of ${size} bytes`
          );
        }
        this.#chunk = typeof next.value === "string" ? new TextEncoder().encode(next.value) : next.value;
        this.#offset = 0;
        if (this.#chunk.byteLength === 0) continue;
      }
      const count = Math.min(
        size - written,
        this.#chunk.byteLength - this.#offset
      );
      result.set(
        this.#chunk.subarray(this.#offset, this.#offset + count),
        written
      );
      this.#offset += count;
      written += count;
    }
    return result;
  }
};
var ResumeDecisionRouter = class {
  #reader;
  #pending = /* @__PURE__ */ new Map();
  #reading = false;
  #control;
  #controlFailure;
  constructor(reader) {
    this.#reader = reader;
  }
  register(correlationId) {
    if (this.#pending.has(correlationId)) {
      return Promise.reject(new Error("duplicate runtime correlation id"));
    }
    const { promise, resolve, reject } = Promise.withResolvers();
    this.#pending.set(correlationId, { resolve, reject });
    this.#pump();
    return promise;
  }
  cancel(correlationId) {
    this.#pending.delete(correlationId);
  }
  abandonPending() {
    this.#pending.clear();
  }
  listenForControl(control, failed) {
    this.#control = control;
    this.#controlFailure = failed;
    this.#pump();
  }
  stopControl() {
    this.#control = void 0;
    this.#controlFailure = void 0;
  }
  #pump() {
    if (this.#reading) return;
    this.#reading = true;
    void (async () => {
      try {
        while (this.#pending.size > 0 || this.#control !== void 0) {
          const decision = fromBinary(
            program_pb_exports.ResumeDecisionSchema,
            await this.#reader.read()
          );
          if (decision.kind === "session_stop") {
            if (this.#control === void 0) {
              throw new RuntimeProtocolError("Session stop has no Actor execution owner");
            }
            this.#control(decision);
            continue;
          }
          const pending = this.#pending.get(decision.correlationId);
          if (pending === void 0) {
            throw new Error("resume decision did not match a pending runtime operation");
          }
          this.#pending.delete(decision.correlationId);
          pending.resolve(decision);
        }
      } catch (error) {
        const failure = error instanceof Error ? error : new Error(String(error));
        for (const pending of this.#pending.values()) pending.reject(failure);
        this.#pending.clear();
        this.#controlFailure?.(new RuntimeProtocolError("Actor control transport failed", { cause: failure }));
        this.stopControl();
      } finally {
        this.#reading = false;
        if (this.#pending.size > 0) this.#pump();
      }
    })();
  }
};
var RuntimeProtocolError = class extends Error {
  constructor(message, options) {
    super(message, options);
    this.name = "RuntimeProtocolError";
  }
};
var ActorCancellationError = class extends Error {
  code;
  constructor(reasonCode) {
    super(`Actor execution was cancelled: ${reasonCode}`);
    this.name = "AbortError";
    this.code = reasonCode;
  }
};
var RunOperationState = class {
  controller = new AbortController();
  #active = 0;
  #drainable = /* @__PURE__ */ new Set();
  #protocolFault;
  admission;
  track(operation) {
    try {
      this.admission?.();
    } catch (error) {
      const rejected = Promise.reject(error);
      void rejected.catch(() => {
      });
      return rejected;
    }
    this.#active++;
    const result = (async () => {
      try {
        return await operation();
      } catch (error) {
        if (error instanceof RuntimeProtocolError && this.#protocolFault === void 0) {
          this.#protocolFault = error;
        }
        throw error;
      } finally {
        this.#active--;
      }
    })();
    void result.catch(() => {
    });
    return result;
  }
  trackDrainable(operation) {
    const result = this.track(operation);
    this.#drainable.add(result);
    void result.finally(() => {
      this.#drainable.delete(result);
    }).catch(() => {
    });
    return result;
  }
  async drainForCompletion() {
    while (this.#drainable.size !== 0) {
      await Promise.allSettled([...this.#drainable]);
    }
  }
  cancel(reasonCode) {
    const error = new ActorCancellationError(reasonCode);
    if (!this.controller.signal.aborted) this.controller.abort(error);
    return this.controller.signal.reason;
  }
  assertCanComplete() {
    if (this.#protocolFault !== void 0) throw this.#protocolFault;
    if (this.controller.signal.aborted) {
      throw this.controller.signal.reason;
    }
    if (this.#active !== 0) {
      throw new RuntimeProtocolError("Run handler returned with runtime operations still pending");
    }
  }
  assertDrained() {
    if (this.#protocolFault !== void 0) throw this.#protocolFault;
    if (this.#active !== 0) throw new RuntimeProtocolError("Run has runtime operations still pending");
  }
};
var ConsumingWaitGate = class {
  #pending = false;
  get pending() {
    return this.#pending;
  }
  acquire(error = () => new Error("only one consuming Wait may be pending")) {
    if (this.#pending) throw error();
    this.#pending = true;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.#pending = false;
    };
  }
};
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
    throw new RuntimeProtocolError(
      `${operation} decision did not match the pending Wait`
    );
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
async function acknowledgeResumeConsumed(io, decision) {
  if (!decision.requireConsumedAck) return;
  await writeRuntimeProtocolEvent(io, {
    case: "resumeConsumed",
    value: create(program_pb_exports.ResumeConsumedSchema, {
      runWaitId: decision.runWaitId,
      checkpointId: decision.checkpointId,
      resumeAttachId: decision.resumeAttachId,
      resumeRequestVersion: decision.resumeRequestVersion,
      runLeaseId: decision.runLeaseId,
      correlationId: decision.correlationId
    })
  });
}
function parseRuntimeProtocolValue(label, parse) {
  try {
    return parse();
  } catch (error) {
    if (error instanceof RuntimeProtocolError) throw error;
    throw new RuntimeProtocolError(`${label} was invalid`, { cause: error });
  }
}
async function runProgram(locatorURL, io = defaultProgramIO()) {
  const reader = new FrameReader(io.input);
  const start = fromBinary(program_pb_exports.ProgramStartSchema, await reader.read());
  validateProgramStart(start);
  const index = await loadProgramIndex(locatorURL, io);
  const kind = start.entrypoint.case;
  if (kind !== "task" && kind !== "actor") {
    throw new Error("Program-start entrypoint is required");
  }
  const located = index.declarations.filter(
    (declaration2) => declaration2.kind === kind && declaration2.declaredId === start.entrypointDeclaredId && declaration2.locator !== void 0
  );
  if (located.length !== 1) {
    throw new Error(
      `Program declaration ${kind}:${JSON.stringify(start.entrypointDeclaredId)} was not found exactly once`
    );
  }
  const declaration = located[0];
  const locator = declaration.locator;
  const moduleURL = resolveModuleURL(locatorURL, locator.sourcePath);
  const imported = io.importModule === void 0 ? await (await import("../moduleexecution/loader.mjs")).importSourceExports(moduleURL) : await io.importModule(moduleURL);
  const definition = inspectDefinition(imported[locator.exportName]);
  if (definition === void 0 || definition.kind !== declaration.kind || definition.id !== declaration.declaredId || definition.kind !== "task" && definition.kind !== "actor") {
    throw new Error(
      `Program export ${JSON.stringify(locator.exportName)} does not match ${kind}:${JSON.stringify(start.entrypointDeclaredId)}`
    );
  }
  validateEntrypointContract(start, definition);
  const identity = entrypointIdentity(kind, start.entrypointDeclaredId);
  await writeRunEvent(io, {
    case: "entrypointReady",
    value: create(program_pb_exports.EntrypointReadySchema, {
      runId: start.runId,
      attemptNumber: start.attemptNumber,
      entrypoint: identity
    })
  });
  const release = fromBinary(
    program_pb_exports.EntrypointReleaseSchema,
    await reader.read()
  );
  validateEntrypointRelease(release, start, kind);
  const decisions = new ResumeDecisionRouter(reader);
  if (definition.kind === "task") {
    await runTask(start, definition, io, decisions);
  } else {
    await runActor(start, definition, io, decisions);
  }
  await reader.close();
}
async function loadProgramIndex(url, io) {
  const raw = io.readLocator === void 0 ? await fs.readFile(url, "utf8") : await io.readLocator(url);
  const value = JSON.parse(raw);
  if (typeof value !== "object" || value === null) {
    throw new Error("Program index must be an object");
  }
  const record = value;
  if (record["architecture"] !== "x86_64" || record["runtimeContract"] !== "helmr.runtime.v0" || typeof record["configResultDigest"] !== "string" || !Array.isArray(record["queues"]) || !Array.isArray(record["declarations"]) || record["declarations"].length === 0) {
    throw new Error("Program index has an invalid v0 shape");
  }
  const declarations = record["declarations"].map(
    (entry, index) => parseProgramIndexDeclaration(entry, index)
  );
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
    if (record["locator"] !== void 0) {
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
  if (typeof located["exportName"] !== "string" || located["exportName"] === "" || typeof located["sourcePath"] !== "string" || located["slot"] !== "handler") {
    throw new Error(`Program index declaration ${index} locator is invalid`);
  }
  return {
    kind: record["kind"],
    declaredId: record["declaredId"],
    locator: {
      exportName: located["exportName"],
      sourcePath: validateSourcePath(located["sourcePath"]),
      slot: "handler"
    }
  };
}
function validateSourcePath(value) {
  const components = value.split("/");
  if (components.some((part) => part === "" || part === "." || part === ".." || part === "node_modules" || part.includes("\\") || /[\u0000-\u001f\u007f-\u009f]/.test(part)) || components[0] === "helmr" || value === "helmr.config.ts" || !/\.(?:[cm]?js|jsx|[cm]?ts|tsx)$/.test(value) || /\.d\.[cm]?ts$/.test(value)) {
    throw new Error("declaration sourcePath must identify a project source module");
  }
  return value;
}
function resolveModuleURL(locatorURL, sourcePath) {
  const root = path.dirname(path.dirname(fileURLToPath(locatorURL)));
  const resolved = path.resolve(root, sourcePath);
  const relative = path.relative(root, resolved);
  if (relative === "" || relative === ".." || relative.startsWith(`..${path.sep}`) || path.isAbsolute(relative)) {
    throw new Error("declaration sourcePath escapes the Program root");
  }
  return pathToFileURL(resolved);
}
function validateProgramStart(start) {
  if (start.runId === "" || start.attemptNumber === 0 || start.entrypointDeclaredId === "" || start.deploymentId === "" || start.deploymentVersion === "" || start.workspaceId === "" || start.baseWorkspaceVersionId === "" || start.cause === void 0 || start.cause.kind.case === void 0) {
    throw new Error("Program-start frame is missing required logical fields");
  }
}
function validateEntrypointContract(start, definition) {
  if (definition.kind === "actor") {
    if (start.entrypoint.case !== "actor" || start.entrypoint.value.startInputSequence < 0n || start.entrypoint.value.runGeneration <= 0n || start.entrypoint.value.inputHighWatermark < start.entrypoint.value.startInputSequence) {
      throw new Error("Program-start Actor cursor authority is invalid");
    }
    return;
  }
  const payload = start.entrypoint.case === "task" ? start.entrypoint.value.payload.case : void 0;
  if (definition.hasPayload && payload !== "payloadJson" || !definition.hasPayload && payload !== "noPayload") {
    throw new Error(
      `Program-start payload presence does not match task ${JSON.stringify(definition.id)}`
    );
  }
}
function entrypointIdentity(kind, declaredId) {
  return create(program_pb_exports.EntrypointIdentitySchema, {
    declaredId,
    kind: kind === "task" ? {
      case: "task",
      value: create(program_pb_exports.TaskEntrypointSchema)
    } : {
      case: "actor",
      value: create(program_pb_exports.ActorEntrypointSchema)
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
      payload = JSON.parse(
        new TextDecoder("utf-8", { fatal: true }).decode(
          start.entrypoint.value.payload.value
        )
      );
      const parsed = await definition.payloadSchema["~standard"].validate(
        payload
      );
      if ("issues" in parsed && parsed.issues !== void 0) {
        failureDetails = validationDetails(parsed.issues);
      } else {
        payload = parsed.value;
      }
    } catch (error) {
      failureDetails = {
        message: boundedUtf8(errorMessage(error), 2048)
      };
    }
    if (failureDetails !== void 0) {
      await writeTaskFailure(
        io,
        "payload_invalid",
        "task payload failed validation",
        failureDetails
      );
      return;
    }
  }
  const context = taskContext(start);
  const runOperations = new RunOperationState();
  const uninstallRuntime = installRuntimeOperations(
    programRuntimeOperations(
      start,
      io,
      decisions,
      new ConsumingWaitGate(),
      runOperations
    )
  );
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
      throw new Error(
        `task output exceeds ${MAX_TASK_OUTPUT_BYTES} bytes`
      );
    }
  } catch (error) {
    if (error instanceof RuntimeProtocolError) throw error;
    await runOperations.drainForCompletion();
    runOperations.assertCanComplete();
    await writeTaskFailure(io, "failed", errorMessage(error));
    return;
  } finally {
    uninstallRuntime();
  }
  await writeRunEvent(io, {
    case: "taskOutcome",
    value: create(program_pb_exports.TaskOutcomeSchema, {
      outcome: {
        case: "succeeded",
        value: create(program_pb_exports.TaskSucceededSchema, {
          outputJson: new TextDecoder().decode(normalized)
        })
      }
    })
  });
}
function programRuntimeOperations(start, io, decisions, waitGate, runOperations, actor) {
  const performTaskStart = async (target, payload, options) => {
    if (options.signal?.aborted) throw abortSignalReason(options.signal);
    const idempotencyKey = options.idempotencyKey === "" ? void 0 : options.idempotencyKey;
    if (idempotencyKey === void 0 && process.env["NODE_ENV"] !== "production") {
      process.emitWarning(
        `Task "${target.declaredId}" was started without an idempotencyKey; retrying the parent Run may create another child Run.`,
        { code: "HELMR_KEYLESS_CHILD_TASK_START" }
      );
    }
    const correlationId = newUUIDv7();
    const payloadJson = target.payloadPresent ? new TextDecoder().decode(canonicalizeJsonValue(payload)) : void 0;
    const workspaceJson = new TextDecoder().decode(
      canonicalizeJsonValue({ id: workspaceRefID(options.workspace) })
    );
    const requestOptions = {
      ...options.queue === void 0 ? {} : { queue: options.queue },
      ...options.concurrencyKey === void 0 ? {} : { concurrency_key: options.concurrencyKey },
      ...options.priority === void 0 ? {} : { priority: options.priority },
      ...options.ttl === void 0 ? {} : { ttl: options.ttl },
      ...options.retry === void 0 ? {} : { retry: taskRetryRequest(options.retry) },
      ...options.metadata === void 0 ? {} : { metadata: options.metadata },
      ...options.tags === void 0 ? {} : { tags: [...options.tags] }
    };
    const optionsJson = new TextDecoder().decode(
      canonicalizeJsonValue(requestOptions)
    );
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "taskChildInvokeRequested",
        value: create(program_pb_exports.TaskChildInvokeRequestedSchema, {
          correlationId,
          declaredId: target.declaredId,
          method: "start",
          ...actor === void 0 ? {} : { actorSpeculativeInputSequence: actor.cursor.value, execution: actor.execution, ...actor.active === void 0 ? {} : { turnId: actor.active.scope.turnId } },
          payloadPresent: target.payloadPresent,
          ...payloadJson === void 0 ? {} : { payloadJson },
          workspaceJson,
          optionsJson,
          ...idempotencyKey === void 0 ? {} : { idempotencyKey }
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
        const id = resourceID(
          value["run_id"],
          "Task child start result.run_id"
        );
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
        const payloadJson = target.payloadPresent ? new TextDecoder().decode(
          canonicalizeJsonValue(payload)
        ) : void 0;
        const workspaceJson = new TextDecoder().decode(
          canonicalizeJsonValue({ id: workspaceRefID(options.workspace) })
        );
        const requestOptions = {
          ...options.queue === void 0 ? {} : { queue: options.queue },
          ...options.concurrencyKey === void 0 ? {} : { concurrency_key: options.concurrencyKey },
          ...options.priority === void 0 ? {} : { priority: options.priority },
          ...options.ttl === void 0 ? {} : { ttl: options.ttl },
          ...options.retry === void 0 ? {} : { retry: taskRetryRequest(options.retry) },
          ...options.metadata === void 0 ? {} : { metadata: options.metadata },
          ...options.tags === void 0 ? {} : { tags: [...options.tags] }
        };
        const decision = await requestRuntimeDecision(
          io,
          decisions,
          correlationId,
          {
            case: "taskChildInvokeRequested",
            value: create(program_pb_exports.TaskChildInvokeRequestedSchema, {
              correlationId,
              runWaitId,
              resumeAttachId,
              declaredId: target.declaredId,
              method: "call",
              payloadPresent: target.payloadPresent,
              ...payloadJson === void 0 ? {} : { payloadJson },
              workspaceJson,
              optionsJson: new TextDecoder().decode(
                canonicalizeJsonValue(requestOptions)
              ),
              idempotencyKey: options.idempotencyKey,
              ...actor === void 0 ? {} : { actorSpeculativeInputSequence: actor.cursor.value, execution: actor.execution, ...actor.active === void 0 ? {} : { turnId: actor.active.scope.turnId } }
            })
          }
        );
        requireWaitDecision(
          decision,
          correlationId,
          runWaitId,
          resumeAttachId,
          "Task child call"
        );
        if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason;
        await acknowledgeResumeConsumed(io, decision);
        if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason;
        if (decision.kind !== "completed") {
          throw decision.kind === "failed" ? runtimeOperationFailure("Task child call", decision.dataJson) : new RuntimeProtocolError(
            `Task child call was cancelled: ${resumeFailure(decision.dataJson).reasonCode}`
          );
        }
        return parseRuntimeProtocolValue(
          "Task child call result",
          () => parseTaskResult(decision.dataJson)
        );
      } finally {
        try {
          await actor?.resumeMessageReady();
        } finally {
          releaseWait();
        }
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
        value: create(program_pb_exports.RunWaitRequestedSchema, {
          correlationId,
          runWaitId,
          resumeAttachId,
          kind: "timer",
          paramsJson: new TextDecoder().decode(canonicalizeJsonValue(params)),
          timeoutMs: BigInt(timeoutMs),
          ...actor === void 0 ? {} : { actorSpeculativeInputSequence: actor.cursor.value, execution: actor.execution, ...actor.active === void 0 ? {} : { turnId: actor.active.scope.turnId } }
        })
      });
      requireWaitDecision(
        decision,
        correlationId,
        runWaitId,
        resumeAttachId,
        "timer resume"
      );
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason;
      await acknowledgeResumeConsumed(io, decision);
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason;
      if (decision.kind !== "completed") {
        const failure = parseRuntimeProtocolValue(
          "timer Wait failure decision",
          () => resumeFailure(decision.dataJson)
        );
        throw new RuntimeProtocolError(`timer Wait ${decision.kind}: ${failure.reasonCode}`);
      }
    } finally {
      try {
        await actor?.resumeMessageReady();
      } finally {
        releaseWait();
      }
    }
  };
  const wait = (params, timeoutMs) => runOperations.track(() => performWait(params, timeoutMs));
  const performActorStart = async (declaredId, options) => {
    if (options.signal?.aborted) throw abortSignalReason(options.signal);
    const correlationId = newUUIDv7();
    const run = options.run;
    const runOptions = {
      ...run?.queue === void 0 ? {} : { queue: run.queue },
      ...run?.concurrencyKey === void 0 ? {} : { concurrency_key: run.concurrencyKey },
      ...run?.priority === void 0 ? {} : { priority: run.priority },
      ...run?.ttl === void 0 ? {} : { ttl: run.ttl },
      ...run?.retry === void 0 ? {} : { retry: taskRetryRequest(run.retry) },
      ...run?.metadata === void 0 ? {} : { metadata: run.metadata },
      ...run?.tags === void 0 ? {} : { tags: [...run.tags] }
    };
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "actorStartRequested",
        value: create(program_pb_exports.ActorStartRequestedSchema, {
          correlationId,
          declaredId,
          workspaceId: workspaceRefID(options.workspace),
          ...options.key === void 0 ? {} : { key: options.key },
          ...options.idempotencyKey === void 0 ? {} : { idempotencyKey: options.idempotencyKey },
          runOptionsJson: new TextDecoder().decode(
            canonicalizeJsonValue(runOptions)
          )
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Actor start");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Actor start", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Actor start result", () => {
        const value = parseObjectJSON(decision.dataJson, "Actor start result");
        requireExactKeys(value, ["run_id", "session_id"], "Actor start result");
        const sessionId = resourceID(
          stringField(value, "session_id", "Actor start result"),
          "Actor start result.session_id"
        );
        const runId = resourceID(
          stringField(value, "run_id", "Actor start result"),
          "Actor start result.run_id"
        );
        return Object.freeze({ sessionId, runId });
      });
    });
    return abortableRuntimeOperation(operation, options.signal);
  };
  const performSessionStatus = async (sessionId, signal) => {
    if (signal?.aborted) throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "sessionStatusRequested",
        value: create(program_pb_exports.SessionStatusRequestedSchema, {
          correlationId,
          sessionId
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Session retrieve");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Session retrieve", decision.dataJson);
      }
      return parseRuntimeProtocolValue(
        "Session retrieve result",
        () => parseSession(JSON.parse(decision.dataJson))
      );
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const sessionOperation = async (event, parse, signal) => {
    if (signal?.aborted) throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const pending = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, event(correlationId));
      requireRuntimeOperationDecision(decision, correlationId, "Session operation");
      if (decision.kind === "failed") throw runtimeOperationFailure("Session operation", decision.dataJson);
      return parseRuntimeProtocolValue("Session operation", () => parse(JSON.parse(decision.dataJson)));
    });
    return abortableRuntimeOperation(pending, signal);
  };
  const submitSession = (mode, sessionId, data, turnId, request, signal) => {
    const dataJson = jsonText(data);
    if (Buffer.byteLength(dataJson) > MAX_ACTOR_INPUT_BYTES) return Promise.reject(new Error("Session data exceeds maximum size"));
    return sessionOperation((correlationId) => ({ case: "sessionSubmitRequested", value: create(program_pb_exports.SessionSubmitRequestedSchema, {
      correlationId,
      sessionId,
      mode,
      dataJson,
      ...turnId === void 0 ? {} : { turnId },
      idempotencyKey: request?.idempotencyKey ?? newUUIDv7()
    }) }), (value) => mode === "message" ? parseSessionMessageReceipt(value) : parseSessionAdmissionReceipt(value), signal);
  };
  const workspaceAddress = (workspaceId) => create(program_pb_exports.WorkspaceAddressSchema, { workspaceId });
  const performWorkspaceCreate = async (declaredId, request = {}, signal) => {
    if (signal?.aborted) throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceCreateRequested",
        value: create(program_pb_exports.WorkspaceCreateRequestedSchema, {
          correlationId,
          declaredId,
          ...request.key === void 0 ? {} : { key: request.key },
          secrets: encodeWorkspaceSecrets(request.secrets).map(
            (secret) => create(program_pb_exports.WorkspaceSecretPlacementSchema, {
              secret: secret.secret,
              placement: secret.env !== void 0 ? { case: "env", value: create(program_pb_exports.SecretEnvBindingSchema, { name: secret.env.name, mode: secret.env.mode, allowedOrigins: [...secret.env.allowed_origins ?? []] }) } : { case: "file", value: create(program_pb_exports.SecretFileBindingSchema, { path: secret.file.path }) }
            })
          ) ?? [],
          ...request.idempotencyKey === void 0 ? {} : { idempotencyKey: request.idempotencyKey }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace create");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace create", decision.dataJson);
      }
      return parseRuntimeProtocolValue("Workspace create result", () => {
        const value = parseObjectJSON(decision.dataJson, "Workspace create result");
        requireExactKeys(value, ["workspace_id"], "Workspace create result");
        const workspaceId = resourceID(
          stringField(
            value,
            "workspace_id",
            "Workspace create result"
          ),
          "Workspace create result.workspace_id"
        );
        return Object.freeze({ workspaceId });
      });
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceRetrieve = async (workspaceId, signal) => {
    if (signal?.aborted) throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceRetrieveRequested",
        value: create(program_pb_exports.WorkspaceRetrieveRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId)
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace retrieve");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace retrieve", decision.dataJson);
      }
      return parseRuntimeProtocolValue(
        "Workspace retrieve result",
        () => parseWorkspace(JSON.parse(decision.dataJson))
      );
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceExec = async (workspaceId, request, signal) => {
    if (signal?.aborted) throw abortSignalReason(signal);
    const timeoutMs = request.timeout === void 0 ? void 0 : durationMilliseconds(request.timeout, "Workspace exec timeout");
    if (timeoutMs !== void 0 && timeoutMs > 15 * 60 * 1e3) {
      throw new Error("Workspace exec timeout must not exceed 15m");
    }
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceExecRequested",
        value: create(program_pb_exports.WorkspaceExecRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
          command: [...request.command],
          ...request.cwd === void 0 ? {} : { cwd: request.cwd },
          env: request.env === void 0 ? {} : { ...request.env },
          stdin: request.stdin === void 0 ? new Uint8Array() : new Uint8Array(request.stdin),
          ...timeoutMs === void 0 ? {} : { timeoutMs: BigInt(timeoutMs) },
          idempotencyKey: request.idempotencyKey
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace exec");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace exec", decision.dataJson);
      }
      return parseRuntimeProtocolValue(
        "Workspace exec result",
        () => parseWorkspaceExecResult(JSON.parse(decision.dataJson))
      );
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performWorkspaceDelete = async (workspaceId, request = {}, signal) => {
    if (signal?.aborted) throw abortSignalReason(signal);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "workspaceDeleteRequested",
        value: create(program_pb_exports.WorkspaceDeleteRequestedSchema, {
          correlationId,
          workspace: workspaceAddress(workspaceId),
          ...request.idempotencyKey === void 0 ? {} : { idempotencyKey: request.idempotencyKey }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Workspace delete");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Workspace delete", decision.dataJson);
      }
      return parseRuntimeProtocolValue(
        "Workspace delete result",
        () => parseWorkspaceDeleteReceipt(JSON.parse(decision.dataJson))
      );
    });
    return abortableRuntimeOperation(operation, signal);
  };
  const performTokenCreate = async (request) => {
    const correlationId = newUUIDv7();
    const timeoutMs = request.timeout === void 0 ? void 0 : durationMilliseconds(request.timeout, "Token timeout");
    const metadataJson = request.metadata === void 0 ? void 0 : new TextDecoder().decode(canonicalizeJsonValue(request.metadata));
    const idempotencyKey = normalizeTokenIdempotencyKey(request.idempotencyKey);
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "tokenCreateRequested",
        value: create(program_pb_exports.TokenCreateRequestedSchema, {
          correlationId,
          ...timeoutMs === void 0 ? {} : { timeoutMs: BigInt(timeoutMs) },
          ...idempotencyKey === void 0 ? {} : { idempotencyKey },
          tags: request.tags === void 0 ? [] : [...request.tags],
          ...metadataJson === void 0 ? {} : { metadataJson }
        })
      });
      requireRuntimeOperationDecision(decision, correlationId, "Token create");
      if (decision.kind === "failed") {
        throw runtimeOperationFailure("Token create", decision.dataJson);
      }
      return parseRuntimeProtocolValue(
        "Token create result",
        () => parseTokenCreateResult(decision.dataJson)
      );
    });
    return await operation;
  };
  const performTokenWait = async (tokenId, options) => {
    const releaseWait = waitGate.acquire();
    const correlationId = newUUIDv7();
    const runWaitId = newUUIDv7();
    const resumeAttachId = newUUIDv7();
    const timeoutMs = options.timeout === void 0 ? void 0 : durationMilliseconds(options.timeout, "Token Wait timeout");
    const idleTimeoutMs = options.idleTimeout === void 0 ? void 0 : tokenWaitIdleTimeoutMilliseconds(options.idleTimeout);
    try {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "runWaitRequested",
        value: create(program_pb_exports.RunWaitRequestedSchema, {
          correlationId,
          runWaitId,
          resumeAttachId,
          kind: "token",
          paramsJson: JSON.stringify({ token_id: tokenId }),
          ...options.metadata === void 0 ? {} : { metadataJson: new TextDecoder().decode(canonicalizeJsonValue(options.metadata)) },
          ...timeoutMs === void 0 ? {} : { timeoutMs: BigInt(timeoutMs) },
          ...idleTimeoutMs === void 0 ? {} : { idleTimeoutMs: BigInt(idleTimeoutMs) },
          tags: options.tags === void 0 ? [] : [...options.tags],
          ...actor === void 0 ? {} : { actorSpeculativeInputSequence: actor.cursor.value, execution: actor.execution, ...actor.active === void 0 ? {} : { turnId: actor.active.scope.turnId } }
        })
      });
      requireWaitDecision(
        decision,
        correlationId,
        runWaitId,
        resumeAttachId,
        "Token resume"
      );
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason;
      await acknowledgeResumeConsumed(io, decision);
      if (runOperations.controller.signal.aborted) throw runOperations.controller.signal.reason;
      if (decision.kind !== "completed") {
        if (decision.kind !== "failed" && decision.kind !== "cancelled") {
          throw new RuntimeProtocolError("Token resume decision kind was invalid");
        }
        throw tokenWaitFailure(decision.kind, decision.dataJson);
      }
      return parseRuntimeProtocolValue(
        "Token completion result",
        () => JSON.parse(decision.dataJson)
      );
    } finally {
      try {
        await actor?.resumeMessageReady();
      } finally {
        releaseWait();
      }
    }
  };
  const performMetadataMutation = async (request) => {
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "metadataUpdated",
        value: create(program_pb_exports.MetadataUpdatedSchema, {
          correlationId,
          operation: request.operation,
          ...request.operation === "set" ? {
            key: normalizeMetadataKey(request.key),
            valueJson: new TextDecoder().decode(
              canonicalizeJsonValue(request.value)
            )
          } : request.operation === "patch" ? {
            patchJson: new TextDecoder().decode(
              canonicalizeMetadataPatch(request.values)
            )
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
    if (Buffer.byteLength(message) > MAX_RUN_LOG_MESSAGE_BYTES) {
      throw new Error(
        `logger message must be at most ${MAX_RUN_LOG_MESSAGE_BYTES} UTF-8 bytes`
      );
    }
    const attributesJson = canonicalizeLogAttributes(attributes);
    const correlationId = newUUIDv7();
    const operation = runOperations.trackDrainable(async () => {
      const decision = await requestRuntimeDecision(io, decisions, correlationId, {
        case: "structuredLogRequested",
        value: create(program_pb_exports.StructuredLogRequestedSchema, {
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
      if (remainingMs <= 0) return Promise.resolve();
      return wait(
        { date: date.toISOString() },
        boundedTimerMilliseconds(Math.ceil(remainingMs))
      );
    },
    actorStart: performActorStart,
    sessionRetrieve: performSessionStatus,
    async sessionSend(sessionId, data, request, signal) {
      return await submitSession("send", sessionId, data, void 0, request, signal);
    },
    async sessionEnqueue(sessionId, data, request, signal) {
      const result = await submitSession("enqueue", sessionId, data, void 0, request, signal);
      if (!("kind" in result) || result.kind !== "enqueued") throw new RuntimeProtocolError("Enqueue returned a message receipt");
      return result;
    },
    async sessionTurnSend(sessionId, turnId, data, request, signal) {
      return await submitSession("message", sessionId, data, turnId, request, signal);
    },
    sessionTurnRetrieve(sessionId, turnId, signal) {
      return sessionOperation((correlationId) => ({ case: "sessionTurnRetrieveRequested", value: create(program_pb_exports.SessionTurnRetrieveRequestedSchema, { correlationId, sessionId, turnId }) }), parseTurnState, signal);
    },
    sessionTurnInterrupt(sessionId, turnId, request, signal) {
      return sessionOperation((correlationId) => ({ case: "sessionTurnInterruptRequested", value: create(program_pb_exports.SessionTurnInterruptRequestedSchema, { correlationId, sessionId, turnId, idempotencyKey: request?.idempotencyKey ?? newUUIDv7() }) }), parseTurnInterruptReceipt, signal);
    },
    sessionEvents(sessionId, query, signal) {
      return sessionOperation((correlationId) => ({ case: "sessionEventsRequested", value: create(program_pb_exports.SessionEventsRequestedSchema, { correlationId, sessionId, after: BigInt(query?.after ?? 0), limit: query?.limit ?? 100 }) }), parseSessionEventPage, signal);
    },
    sessionClose(sessionId, request, signal) {
      return sessionOperation((correlationId) => ({ case: "sessionCloseRequested", value: create(program_pb_exports.SessionCloseRequestedSchema, { correlationId, sessionId, idempotencyKey: request?.idempotencyKey ?? newUUIDv7() }) }), parseSessionCloseReceipt, signal);
    },
    sessionCancel(sessionId, request, signal) {
      return sessionOperation((correlationId) => ({ case: "sessionCancelRequested", value: create(program_pb_exports.SessionCancelRequestedSchema, { correlationId, sessionId, idempotencyKey: request?.idempotencyKey ?? newUUIDv7() }) }), parseSessionCancelReceipt, signal);
    },
    sessionResume(sessionId, request, signal) {
      return sessionOperation((correlationId) => ({ case: "sessionResumeRequested", value: create(program_pb_exports.SessionResumeRequestedSchema, { correlationId, sessionId, holdId: request.holdId, idempotencyKey: request.idempotencyKey ?? newUUIDv7() }) }), parseSessionResumeReceipt, signal);
    },
    workspaceCreate(declaredId, request, signal) {
      return performWorkspaceCreate(declaredId, request, signal);
    },
    workspaceRetrieve(address, signal) {
      return performWorkspaceRetrieve(address, signal);
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
  if (Buffer.byteLength(value) > 512) {
    throw new Error("metadata key must be at most 512 UTF-8 bytes");
  }
  return value;
}
function canonicalizeMetadataPatch(values) {
  if (values === null || typeof values !== "object" || Array.isArray(values)) {
    throw new Error("metadata.patch() requires an object");
  }
  for (const key of Object.keys(values)) normalizeMetadataKey(key);
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
    throw new Error(
      `logger attributes must be at most ${MAX_RUN_LOG_ATTRIBUTES_BYTES} canonical JSON bytes`
    );
  }
  return normalized;
}
function normalizeTokenIdempotencyKey(value) {
  if (value === void 0) return void 0;
  const normalized = trimGoSpace(value);
  if (Buffer.byteLength(normalized) > 512) {
    throw new Error("Token idempotency key must be at most 512 UTF-8 bytes");
  }
  return normalized === "" ? void 0 : normalized;
}
function taskRetryRequest(retry) {
  if (retry.enabled === false) return { enabled: false };
  return {
    ...retry.enabled === void 0 ? {} : { enabled: retry.enabled },
    max_attempts: retry.maxAttempts,
    ...retry.backoff === void 0 ? {} : {
      backoff: {
        ...retry.backoff.minDelay === void 0 ? {} : { min_delay: retry.backoff.minDelay },
        ...retry.backoff.maxDelay === void 0 ? {} : { max_delay: retry.backoff.maxDelay },
        ...retry.backoff.factor === void 0 ? {} : { factor: retry.backoff.factor },
        ...retry.backoff.jitter === void 0 ? {} : { jitter: retry.backoff.jitter }
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
    id: resourceID(
      stringField(value, "id", "Token create result"),
      "Token create result.id"
    ),
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
    requireExactKeys(
      value,
      ["ok", "output", "run"],
      "Task child call success"
    );
    return Object.freeze({
      ok: true,
      output: jsonValueField(value, "output", "Task child call success"),
      run: parseTaskResultRun(value)
    });
  }
  if (ok !== false) {
    throw new Error("Task child call result.ok must be a boolean");
  }
  requireExactKeys(
    value,
    ["failure", "ok", "run"],
    "Task child call failure"
  );
  const rawFailure = objectField(value, "failure", "Task child call failure");
  requireExactKeys(
    rawFailure,
    ["code", "details", "message"],
    "Task child call failure.failure"
  );
  const details = objectField(
    rawFailure,
    "details",
    "Task child call failure.failure"
  );
  const failure = Object.freeze({
    code: stringField(
      rawFailure,
      "code",
      "Task child call failure.failure"
    ),
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
  const id = resourceID(
    stringField(run, "id", "Task child call result.run"),
    "Task child call result.run.id"
  );
  return createRunHandle(id);
}
function tokenWaitFailure(kind, dataJson) {
  const failure = parseRuntimeProtocolValue(
    "Token Wait failure",
    () => resumeFailure(dataJson)
  );
  const code = failure.reasonCode;
  const error = new Error(
    code === "wait_timeout" ? "Token wait timed out" : code === "token_expired" ? "Token expired" : code === "token_cancelled" ? "Token was cancelled" : `Token Wait ${kind}: ${code}`
  );
  error.name = code === "wait_timeout" ? "WaitTimeoutError" : "HelmrError";
  error.code = code;
  return error;
}
function requireRuntimeOperationDecision(decision, correlationId, operation) {
  if (decision.correlationId !== correlationId || decision.kind !== "completed" && decision.kind !== "failed" || decision.runWaitId !== "" || decision.requireConsumedAck || decision.checkpointId !== "" || decision.resumeAttachId !== "" || decision.resumeRequestVersion !== 0n || decision.runLeaseId !== "" || decision.noResult) {
    throw new RuntimeProtocolError(
      `${operation} decision did not match the pending operation`
    );
  }
}
async function abortableRuntimeOperation(operation, signal) {
  if (signal === void 0) return operation;
  if (signal.aborted) throw abortSignalReason(signal);
  const aborted = Promise.withResolvers();
  const onAbort = () => aborted.reject(abortSignalReason(signal));
  signal.addEventListener("abort", onAbort, { once: true });
  try {
    return await Promise.race([operation, aborted.promise]);
  } finally {
    signal.removeEventListener("abort", onAbort);
  }
}
function abortSignalReason(signal) {
  return signal.reason === void 0 ? new DOMException("The operation was aborted", "AbortError") : signal.reason;
}
function resumeFailure(dataJson) {
  const value = parseObjectJSON(dataJson, "terminal Wait failure data");
  return {
    reasonCode: stringField(
      value,
      "reason_code",
      "terminal Wait failure data"
    )
  };
}
function durationMilliseconds(duration, label = "timer duration") {
  const match = /^([1-9][0-9]*)(ms|s|m|h|d)$/.exec(duration);
  if (match === null) {
    throw new Error(
      `${label} must be a positive integer followed by ms, s, m, h, or d`
    );
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
  const maxMilliseconds = 365 * 24 * 60 * 60 * 1e3;
  if (!Number.isSafeInteger(milliseconds) || milliseconds < 1 || milliseconds > maxMilliseconds) {
    throw new Error("timer duration must be between 1ms and 365d");
  }
  return milliseconds;
}
function tokenWaitIdleTimeoutMilliseconds(duration) {
  const milliseconds = durationMilliseconds(duration, "Token Wait idle timeout");
  if (milliseconds > 60 * 60 * 1e3) {
    throw new Error("Token Wait idle timeout must be between 1ms and 1h");
  }
  return milliseconds;
}
var messageCallback = new AsyncLocalStorage();
var ActorRuntime = class {
  execution;
  cursor;
  #mainWrites = /* @__PURE__ */ new Set();
  #outputError;
  #uncertain;
  #settlement;
  #receiveCorrelation;
  #pendingSettlementTurn;
  active;
  stop;
  start;
  definition;
  io;
  decisions;
  waitGate;
  operations;
  constructor(start, definition, io, decisions, waitGate, operations) {
    this.start = start;
    this.definition = definition;
    this.io = io;
    this.decisions = decisions;
    this.waitGate = waitGate;
    this.operations = operations;
    if (start.entrypoint.case !== "actor")
      throw new Error("Actor start required");
    this.cursor = { value: start.entrypoint.value.startInputSequence };
    this.execution = create(program_pb_exports.SessionExecutionSchema, {
      sessionId: start.entrypoint.value.sessionId,
      runId: start.runId,
      attemptNumber: start.attemptNumber,
      runGeneration: start.entrypoint.value.runGeneration
    });
  }
  assertAdmission() {
    if (this.#uncertain !== void 0) throw this.#uncertain;
    const callback = messageCallback.getStore();
    if (callback !== void 0 && (callback.turn !== this.active || callback.turn.phase === "settled"))
      throw new Error("Message callback no longer owns its Turn");
    if (this.operations.controller.signal.aborted)
      throw this.operations.controller.signal.reason;
    if (this.active?.phase === "settling" && messageCallback.getStore()?.turn !== this.active) {
      throw new Error("Turn settlement has started");
    }
  }
  control(decision) {
    const value = parseRuntimeProtocolValue(
      "Session stop",
      () => parseObjectJSON(decision.dataJson, "Session stop")
    );
    const execution = objectField(value, "execution", "Session stop");
    if (execution["session_id"] !== this.execution.sessionId || execution["run_id"] !== this.execution.runId || execution["attempt_number"] !== this.execution.attemptNumber || execution["run_generation"] !== Number(this.execution.runGeneration)) {
      throw new RuntimeProtocolError(
        "Session stop does not match current execution"
      );
    }
    const turnId = value["turn_id"];
    const pendingReceive = this.active === void 0 && this.#receiveCorrelation !== void 0;
    const pendingSettlement = turnId === null && this.active !== void 0 && this.#pendingSettlementTurn === this.active;
    if (turnId !== (this.active?.scope.turnId ?? null) && !(pendingReceive && typeof turnId === "string") && !pendingSettlement) {
      throw new RuntimeProtocolError("Session stop does not match current Turn");
    }
    const holdId = resourceID(value["hold_id"], "Session stop.hold_id");
    if (this.stop !== void 0 && (this.stop.holdId !== holdId || (this.stop.turnId ?? null) !== turnId))
      throw new RuntimeProtocolError("Session stop binding changed");
    this.stop = {
      holdId,
      ...turnId === null ? {} : { turnId: resourceID(turnId, "Session stop.turn_id") }
    };
    const error = this.operations.cancel(
      stringField(value, "reason", "Session stop")
    );
    this.active?.controller.abort(error);
  }
  uncertain(error) {
    this.#uncertain ??= error;
    this.operations.controller.abort(error);
    this.active?.controller.abort(error);
  }
  async request(event, correlationId, label) {
    const decision = await requestRuntimeDecision(
      this.io,
      this.decisions,
      correlationId,
      event
    );
    requireRuntimeOperationDecision(decision, correlationId, label);
    if (decision.kind === "failed")
      throw runtimeOperationFailure(label, decision.dataJson);
    return decision;
  }
  async receive(options) {
    this.assertAdmission();
    if (this.active !== void 0)
      throw new Error("Current Turn must be explicitly settled before receive");
    const release = this.waitGate.acquire();
    const correlationId = newUUIDv7();
    try {
      const runWaitId = newUUIDv7(), resumeAttachId = newUUIDv7();
      this.#receiveCorrelation = correlationId;
      const decision = await this.operations.track(
        () => requestRuntimeDecision(this.io, this.decisions, correlationId, {
          case: "runWaitRequested",
          value: create(program_pb_exports.RunWaitRequestedSchema, {
            correlationId,
            runWaitId,
            resumeAttachId,
            kind: "actor_input",
            execution: this.execution,
            paramsJson: JSON.stringify({
              session_id: this.execution.sessionId,
              after_input_sequence: Number(this.cursor.value)
            }),
            actorSpeculativeInputSequence: this.cursor.value,
            ...options?.timeout === void 0 ? {} : { timeoutMs: BigInt(durationMilliseconds(options.timeout)) },
            ...options?.idleTimeout === void 0 ? {} : {
              idleTimeoutMs: BigInt(
                durationMilliseconds(options.idleTimeout)
              )
            },
            ...options?.metadata === void 0 ? {} : { metadataJson: jsonText(options.metadata) },
            tags: options?.tags === void 0 ? [] : [...options.tags]
          })
        })
      );
      requireWaitDecision(
        decision,
        correlationId,
        runWaitId,
        resumeAttachId,
        "Session receive"
      );
      if (this.operations.controller.signal.aborted)
        throw this.operations.controller.signal.reason;
      await acknowledgeResumeConsumed(this.io, decision);
      if (this.operations.controller.signal.aborted)
        throw this.operations.controller.signal.reason;
      if (decision.kind !== "completed") {
        const failure = resumeFailure(decision.dataJson);
        if (failure.reasonCode === "session_closed") return null;
        if (decision.kind === "cancelled")
          throw new Error(`Session receive cancelled: ${failure.reasonCode}`);
        throw Object.assign(
          new Error(`Session receive failed: ${failure.reasonCode}`),
          { code: failure.reasonCode }
        );
      }
      const data = parseRuntimeProtocolValue(
        "Turn delivery",
        () => parseObjectJSON(decision.dataJson, "Turn delivery")
      );
      const turn = objectField(data, "turn", "Turn delivery");
      const sequence = safeJSONSequence(turn["sequence"], "Turn sequence");
      if (BigInt(sequence) !== this.cursor.value + 1n || data["run_generation"] !== Number(this.execution.runGeneration)) {
        throw new RuntimeProtocolError(
          "Turn delivery does not match execution frontier"
        );
      }
      const state = {
        scope: create(program_pb_exports.TurnExecutionSchema, {
          session: this.execution,
          turnId: resourceID(turn["id"], "Turn id")
        }),
        sequence: BigInt(sequence),
        controller: new AbortController(),
        phase: "running"
      };
      this.active = state;
      this.cursor.value = state.sequence;
      if (this.operations.controller.signal.aborted)
        throw this.operations.controller.signal.reason;
      const source2 = objectField(turn, "source", "Turn source");
      let parsedSource;
      if (source2["type"] === "external") parsedSource = { type: "external" };
      else if (source2["type"] === "run")
        parsedSource = {
          type: "run",
          runId: resourceID(source2["run_id"], "Turn source Run")
        };
      else throw new RuntimeProtocolError("Invalid Turn source");
      return Object.freeze({
        id: state.scope.turnId,
        sequence,
        input: data["value"],
        source: Object.freeze(parsedSource),
        createdAt: timestampString(turn["created_at"], "Turn created_at"),
        signal: state.controller.signal,
        output: this.writer(state),
        onMessage: (handler) => this.onMessage(state, handler),
        complete: (...result) => this.settle(
          state,
          "completed",
          result[0],
          result.length !== 0 && result[0] !== void 0
        ),
        fail: (error) => this.settle(state, "failed", failureJSON(error))
      });
    } catch (error) {
      if (error instanceof RuntimeProtocolError) this.uncertain(error);
      throw error;
    } finally {
      if (this.#receiveCorrelation === correlationId)
        this.#receiveCorrelation = void 0;
      release();
    }
  }
  onMessage(turn, handler) {
    this.assertTurn(turn);
    if (turn.handler !== void 0)
      return Promise.reject(
        new Error("Turn message handler is already installed")
      );
    if (typeof handler !== "function")
      return Promise.reject(
        new Error("Turn message handler must be a function")
      );
    turn.handler = handler;
    const correlationId = newUUIDv7();
    turn.ready = this.operations.trackDrainable(async () => {
      await this.request(
        {
          case: "turnReadyRequested",
          value: create(program_pb_exports.TurnReadyRequestedSchema, {
            correlationId,
            execution: turn.scope
          })
        },
        correlationId,
        "Turn readiness"
      );
      void this.messageLoop(turn).catch((error) => {
        if (!this.stoppedRejection(error)) this.uncertain(error);
      });
    });
    return turn.ready;
  }
  async resumeMessageReady() {
    const turn = this.active;
    if (turn?.handler === void 0 || turn.phase !== "running" || this.operations.controller.signal.aborted)
      return;
    const correlationId = newUUIDv7();
    turn.ready = this.operations.trackDrainable(async () => {
      await this.request(
        {
          case: "turnReadyRequested",
          value: create(program_pb_exports.TurnReadyRequestedSchema, {
            correlationId,
            execution: turn.scope
          })
        },
        correlationId,
        "Turn readiness after wait"
      );
    });
    await turn.ready;
  }
  async messageLoop(turn) {
    while (turn.phase === "running" && !this.operations.controller.signal.aborted) {
      if (this.waitGate.pending) {
        await new Promise((resolve) => setTimeout(resolve, 25));
        continue;
      }
      const correlationId = newUUIDv7(), deliveryId = newUUIDv7();
      turn.claim = (async () => {
        const response = await this.request(
          {
            case: "turnMessageClaimRequested",
            value: create(program_pb_exports.TurnMessageClaimRequestedSchema, {
              correlationId,
              execution: turn.scope,
              deliveryId
            })
          },
          correlationId,
          "Turn message claim"
        );
        const body = parseRuntimeProtocolValue(
          "Turn message delivery",
          () => parseObjectJSON(response.dataJson, "Turn message delivery")
        );
        if (body["delivery"] === null) return;
        const delivery = objectField(body, "delivery", "Turn message delivery");
        if (delivery["turn_id"] !== turn.scope.turnId || delivery["delivery_id"] !== deliveryId)
          throw new RuntimeProtocolError(
            "Message delivery does not match requested Turn"
          );
        turn.callback = this.handleMessage(
          turn,
          deliveryId,
          resourceID(delivery["message_id"], "Message id"),
          delivery["data"]
        );
      })();
      try {
        await turn.claim;
      } catch (error) {
        if (error.code !== "turn_not_ready") throw error;
      }
      turn.claim = void 0;
      if (turn.callback !== void 0) {
        await turn.callback;
        turn.callback = void 0;
      } else if (turn.phase === "running")
        await new Promise((resolve) => setTimeout(resolve, 25));
    }
  }
  async handleMessage(turn, deliveryId, messageId, data) {
    const context = { turn, deliveryId, writes: /* @__PURE__ */ new Set() };
    let status = "handled", code = "", details;
    try {
      await messageCallback.run(context, async () => {
        await turn.handler({ id: messageId, data });
      });
      await drainPromises(context.writes);
      if (context.error !== void 0) throw context.error;
    } catch (error) {
      await drainPromises(context.writes);
      if (error instanceof MessageRejected && context.error === void 0) {
        status = "rejected";
        code = "handler_rejected";
        details = error.details;
      } else {
        status = "unknown";
        code = "handler_failed";
        details = failureJSON(error);
        this.#uncertain ??= new RuntimeProtocolError(
          "Message handler outcome is unknown",
          { cause: error }
        );
      }
    }
    const correlationId = newUUIDv7();
    await this.request(
      {
        case: "turnMessageCompleteRequested",
        value: create(program_pb_exports.TurnMessageCompleteRequestedSchema, {
          correlationId,
          execution: turn.scope,
          messageId,
          deliveryId,
          status,
          code,
          ...details === void 0 ? {} : { detailsJson: jsonText(details) }
        })
      },
      correlationId,
      "Turn message completion"
    );
    if (this.#uncertain !== void 0) this.uncertain(this.#uncertain);
  }
  assertTurn(turn) {
    this.assertAdmission();
    if (this.active !== turn || turn.phase !== "running")
      throw new Error("Turn is not writable");
  }
  writer(turn) {
    const track = (operation) => {
      const callback = messageCallback.getStore();
      try {
        this.assertAdmission();
        if (turn !== void 0 && (this.active !== turn || turn.phase !== "running" && !(turn.phase === "settling" && callback?.turn === turn)))
          throw new Error("Turn is not writable");
      } catch (error) {
        const rejected = Promise.reject(error);
        void rejected.catch(() => {
        });
        return rejected;
      }
      const writes = callback?.writes ?? this.#mainWrites;
      const pending = this.operations.trackDrainable(async () => {
        try {
          return await operation(callback);
        } catch (error) {
          const failure = error ?? new Error("Actor output failed", { cause: error });
          if (callback !== void 0) callback.error ??= failure;
          else this.#outputError ??= failure;
          throw failure;
        }
      });
      writes.add(pending);
      void pending.finally(() => writes.delete(pending)).catch(() => {
      });
      return pending;
    };
    const write = async (value, options, callback) => {
      await turn?.ready;
      const correlationId = newUUIDv7();
      const common = {
        correlationId,
        dataJson: jsonText(value),
        idempotencyKey: options?.idempotencyKey ?? newUUIDv7()
      };
      const response = await this.request(
        turn === void 0 ? {
          case: "sessionOutputWriteRequested",
          value: create(program_pb_exports.SessionOutputWriteRequestedSchema, {
            ...common,
            execution: this.execution
          })
        } : {
          case: "turnOutputWriteRequested",
          value: create(program_pb_exports.TurnOutputWriteRequestedSchema, {
            ...common,
            execution: turn.scope,
            ...callback === void 0 ? {} : { messageDeliveryId: callback.deliveryId }
          })
        },
        correlationId,
        "Output write"
      );
      const receipt = parseRuntimeProtocolValue(
        "Output receipt",
        () => parseOutputReceipt(JSON.parse(response.dataJson))
      );
      if (receipt.sessionId !== this.execution.sessionId || receipt.turnId !== (turn?.scope.turnId ?? null) || receipt.runId !== this.execution.runId || receipt.attemptNumber !== this.execution.attemptNumber || receipt.runGeneration !== Number(this.execution.runGeneration)) {
        throw new RuntimeProtocolError(
          "Output receipt does not match its producer"
        );
      }
      return receipt;
    };
    return Object.freeze({
      write: (value, options) => track((callback) => write(value, options, callback)),
      pipe: (source2) => track(async (callback) => {
        for await (const value of source2)
          await write(value, void 0, callback);
      })
    });
  }
  settle(turn, disposition, value, present = true) {
    if (messageCallback.getStore() !== void 0)
      return Promise.reject(new Error("Message callbacks cannot settle a Turn"));
    try {
      this.assertTurn(turn);
    } catch (error) {
      return Promise.reject(error);
    }
    turn.phase = "settling";
    const settling = (async () => {
      await turn.ready;
      await turn.claim;
      await drainPromises(this.#mainWrites);
      this.assertOutputSafe();
      const beginId = newUUIDv7();
      await this.request(
        {
          case: "turnSettlementBeginRequested",
          value: create(program_pb_exports.TurnSettlementBeginRequestedSchema, {
            correlationId: beginId,
            execution: turn.scope
          })
        },
        beginId,
        "Turn settlement barrier"
      );
      await turn.callback;
      this.assertOutputSafe();
      if (this.operations.controller.signal.aborted)
        throw this.operations.controller.signal.reason;
      this.operations.assertDrained();
      const correlationId = newUUIDv7();
      this.#pendingSettlementTurn = turn;
      try {
        const response = await requestRuntimeDecision(
          this.io,
          this.decisions,
          correlationId,
          {
            case: "turnSettleRequested",
            value: create(program_pb_exports.TurnSettleRequestedSchema, {
              correlationId,
              execution: turn.scope,
              targetInputSequence: turn.sequence,
              disposition,
              ...disposition === "completed" ? present ? { resultJson: jsonText(value) } : {} : { errorJson: jsonText(value) }
            })
          }
        );
        if (this.stop !== void 0 && this.stop.turnId === void 0 && response.kind !== "committed")
          throw new RuntimeProtocolError(
            "Null-Turn stop was not confirmed by settlement"
          );
        if (response.correlationId !== correlationId || response.kind !== "committed")
          throw new RuntimeProtocolError("Turn settlement was not acknowledged");
        if (this.stop?.turnId !== void 0)
          throw new RuntimeProtocolError(
            "Turn settlement committed after a Turn stop"
          );
        turn.phase = "settled";
        this.active = void 0;
      } finally {
        this.#pendingSettlementTurn = void 0;
      }
    })();
    this.#settlement = settling;
    void settling.catch((error) => {
      if (!this.stoppedRejection(error)) this.#uncertain ??= error;
    });
    return settling;
  }
  assertOutputSafe() {
    if (this.#uncertain !== void 0) throw this.#uncertain;
    if (this.#outputError !== void 0) {
      const code = this.#outputError.code;
      if (this.stop === void 0 || code !== "turn_stopping" && code !== "session_held")
        throw this.#outputError;
    }
  }
  stoppedRejection(error) {
    if (this.stop === void 0) return false;
    if (error === this.operations.controller.signal.reason) return true;
    const code = error?.code;
    return code === "turn_stopping" || code === "session_held" || code === "session_stopped";
  }
  async drain() {
    try {
      await this.#settlement;
    } catch (error) {
      if (!this.stoppedRejection(error)) throw error;
    }
    if (this.active !== void 0) this.active.phase = "settling";
    try {
      await this.active?.ready;
      await this.active?.claim;
    } catch (error) {
      if (!this.stoppedRejection(error)) throw error;
    }
    await this.active?.callback;
    await drainPromises(this.#mainWrites);
    await this.operations.drainForCompletion();
    this.assertOutputSafe();
    this.operations.assertDrained();
  }
};
async function drainPromises(pending) {
  while (pending.size !== 0) await Promise.allSettled([...pending]);
}
function jsonText(value) {
  return new TextDecoder().decode(canonicalizeJsonValue(value));
}
function failureJSON(error) {
  if (error instanceof Error)
    return { message: boundedUtf8(error.message, MAX_TASK_ERROR_MESSAGE_BYTES) };
  try {
    return JSON.parse(jsonText(error));
  } catch {
    return { message: boundedUtf8(String(error), MAX_TASK_ERROR_MESSAGE_BYTES) };
  }
}
async function runActor(start, definition, io, decisions) {
  const operations = new RunOperationState(), waitGate = new ConsumingWaitGate();
  const actor = new ActorRuntime(
    start,
    definition,
    io,
    decisions,
    waitGate,
    operations
  );
  operations.admission = () => actor.assertAdmission();
  const uninstall = installRuntimeOperations(
    programRuntimeOperations(start, io, decisions, waitGate, operations, actor)
  );
  decisions.listenForControl(
    (decision) => actor.control(decision),
    (error) => actor.uncertain(error)
  );
  let failure;
  let failed = false;
  try {
    await definition.handler(
      Object.freeze({
        id: actor.execution.sessionId,
        ...start.entrypoint.case === "actor" && start.entrypoint.value.key !== void 0 ? { key: start.entrypoint.value.key } : {},
        receive: (options) => actor.receive(options),
        output: actor.writer()
      }),
      actorContext(start, operations.controller.signal)
    );
  } catch (error) {
    failed = true;
    failure = error;
  }
  try {
    await actor.drain();
    if (actor.stop !== void 0 && (!failed || actor.stoppedRejection(failure))) {
      await writeRunEvent(io, {
        case: "actorOutcome",
        value: create(program_pb_exports.ActorOutcomeSchema, {
          runGeneration: actor.execution.runGeneration,
          outcome: {
            case: "interrupted",
            value: create(program_pb_exports.ActorInterruptedSchema, actor.stop)
          }
        })
      });
    } else if (failed || actor.active !== void 0) {
      if (failure instanceof RuntimeProtocolError) throw failure;
      await writeActorFailure(
        io,
        actor.execution.runGeneration,
        errorMessage(
          failure ?? new Error("Run returned with an unsettled Turn")
        )
      );
    } else {
      operations.assertCanComplete();
      await writeRunEvent(io, {
        case: "actorOutcome",
        value: create(program_pb_exports.ActorOutcomeSchema, {
          runGeneration: actor.execution.runGeneration,
          outcome: {
            case: "succeeded",
            value: create(program_pb_exports.ActorSucceededSchema)
          }
        })
      });
    }
  } finally {
    decisions.stopControl();
    uninstall();
  }
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
async function writeActorFailure(io, runGeneration, message) {
  const normalizedMessage = canonicalFailureMessage(message, "actor failed");
  await writeRunEvent(io, {
    case: "actorOutcome",
    value: create(program_pb_exports.ActorOutcomeSchema, {
      runGeneration,
      outcome: {
        case: "failed",
        value: create(program_pb_exports.ActorFailedSchema, {
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
        scheduledAt: new Date(
          Number(cause.kind.value.scheduledAtUnixMs)
        ).toISOString(),
        ...cause.kind.value.previousScheduledAtUnixMs === void 0 ? {} : {
          lastScheduledAt: new Date(
            Number(cause.kind.value.previousScheduledAtUnixMs)
          ).toISOString()
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
  if (details !== void 0) {
    detailsJson = new TextDecoder().decode(canonicalizeJsonValue(details));
    const errorBytes = canonicalizeJsonValue({
      message: normalizedMessage,
      details
    }).byteLength;
    if (errorBytes > MAX_TASK_ERROR_BYTES) detailsJson = void 0;
  }
  await writeRunEvent(io, {
    case: "taskOutcome",
    value: create(program_pb_exports.TaskOutcomeSchema, {
      outcome: kind === "failed" ? {
        case: "failed",
        value: create(program_pb_exports.TaskFailedSchema, {
          message: normalizedMessage,
          ...detailsJson === void 0 ? {} : { detailsJson }
        })
      } : {
        case: "payloadInvalid",
        value: create(program_pb_exports.TaskPayloadInvalidSchema, {
          message: normalizedMessage,
          ...detailsJson === void 0 ? {} : { detailsJson }
        })
      }
    })
  });
}
function validationDetails(issues) {
  return {
    issues: issues.slice(0, 5).map((issue) => ({
      message: boundedUtf8(issue.message, 1024),
      ...issue.path === void 0 ? {} : {
        path: issue.path.slice(0, 16).map(
          (part) => boundedUtf8(
            String(
              typeof part === "object" && part !== null && "key" in part ? part.key : part
            ),
            256
          )
        )
      }
    })),
    truncated: issues.length > 5
  };
}
function boundedUtf8(value, maxBytes) {
  if (Buffer.byteLength(value) <= maxBytes) return value;
  const suffix = "\u2026";
  const suffixBytes = Buffer.byteLength(suffix);
  let result = "";
  let size = 0;
  for (const character of value) {
    const characterBytes = Buffer.byteLength(character);
    if (size + characterBytes + suffixBytes > maxBytes) break;
    result += character;
    size += characterBytes;
  }
  return result + suffix;
}
function canonicalFailureMessage(message, fallback) {
  const canonical = trimGoSpace(message);
  return boundedUtf8(
    canonical === "" ? fallback : canonical,
    MAX_TASK_ERROR_MESSAGE_BYTES
  );
}
async function writeRunEvent(io, event) {
  const body = toBinary(
    program_pb_exports.RunEventSchema,
    create(program_pb_exports.RunEventSchema, { event })
  );
  await io.write(frame(body));
}
function frame(body) {
  if (body.byteLength > MAX_PROGRAM_FRAME_BYTES) {
    throw new Error(
      `runtime frame length ${body.byteLength} exceeds max ${MAX_PROGRAM_FRAME_BYTES}`
    );
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
        if (error === null || error === void 0) resolve();
        else reject(error);
      });
    })
  };
}
function errorMessage(error) {
  return error instanceof Error ? error.message : String(error);
}

// runtime/typescript/src/entry.ts
await runProgram(new URL("file:///opt/helmr/program/helmr/declarations.json"));
