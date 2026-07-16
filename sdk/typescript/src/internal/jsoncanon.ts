export type JsonValue =
  | null
  | boolean
  | number
  | string
  | readonly JsonValue[]
  | JsonObject

export interface JsonObject {
  readonly [key: string]: JsonValue
}

const textDecoder = new TextDecoder("utf-8", { fatal: true })
const textEncoder = new TextEncoder()

export function parseJson(raw: string | Uint8Array): JsonValue {
  if (
    typeof raw !== "string" &&
    raw.length >= 3 &&
    raw[0] === 0xef &&
    raw[1] === 0xbb &&
    raw[2] === 0xbf
  ) {
    throw new Error("canonical JSON cannot start with a byte order mark")
  }
  let source: string
  try {
    source = typeof raw === "string" ? raw : textDecoder.decode(raw)
  } catch {
    throw new Error("canonical JSON is not valid UTF-8")
  }
  if (source.charCodeAt(0) === 0xfeff) {
    throw new Error("canonical JSON cannot start with a byte order mark")
  }
  return new JsonParser(source).parse()
}

export function canonicalizeJson(raw: string | Uint8Array): Uint8Array {
  return canonicalizeJsonValue(parseJson(raw))
}

export function canonicalizeJsonValue(value: JsonValue): Uint8Array {
  return textEncoder.encode(serialize(value, new Set<object>()))
}

function serialize(value: JsonValue, ancestors: Set<object>): string {
  if (value === null || typeof value === "boolean") {
    return String(value)
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) {
      throw new Error("canonical JSON numbers must be finite IEEE 754 doubles")
    }
    return JSON.stringify(value)
  }
  if (typeof value === "string") {
    assertUnicodeString(value)
    return JSON.stringify(value)
  }
  if (typeof value !== "object") {
    throw new Error(`canonical JSON does not support ${typeof value}`)
  }
  if (ancestors.has(value)) {
    throw new Error("canonical JSON does not support cyclic values")
  }

  ancestors.add(value)
  try {
    if (Array.isArray(value)) {
      assertPlainArray(value)
      const items = value.map((item) => serialize(item, ancestors))
      return `[${items.join(",")}]`
    }

    const objectValue = value as JsonObject
    assertPlainObject(objectValue)
    const entries = Object.keys(objectValue)
      .sort()
      .map((key) => {
        assertUnicodeString(key)
        return `${JSON.stringify(key)}:${serialize(objectValue[key] as JsonValue, ancestors)}`
      })
    return `{${entries.join(",")}}`
  } finally {
    ancestors.delete(value)
  }
}

function assertPlainArray(value: readonly JsonValue[]): void {
  const keys = Reflect.ownKeys(value)
  const expected = Array.from({ length: value.length }, (_, index) => String(index))
  expected.push("length")
  if (keys.length !== expected.length || keys.some((key, index) => key !== expected[index])) {
    throw new Error("canonical JSON arrays must be dense and have no extra properties")
  }
}

function assertPlainObject(value: JsonObject): void {
  const prototype = Object.getPrototypeOf(value)
  if (prototype !== Object.prototype && prototype !== null) {
    throw new Error("canonical JSON objects must have a plain or null prototype")
  }
  for (const key of Reflect.ownKeys(value)) {
    if (typeof key !== "string") {
      throw new Error("canonical JSON objects cannot have symbol properties")
    }
    const descriptor = Object.getOwnPropertyDescriptor(value, key)
    if (!descriptor?.enumerable || !("value" in descriptor)) {
      throw new Error("canonical JSON object properties must be enumerable data properties")
    }
  }
}

function assertUnicodeString(value: string): void {
  for (let index = 0; index < value.length; index++) {
    const unit = value.charCodeAt(index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      const next = value.charCodeAt(index + 1)
      if (next < 0xdc00 || next > 0xdfff) {
        throw new Error("canonical JSON contains an unpaired high surrogate")
      }
      index++
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      throw new Error("canonical JSON contains an unpaired low surrogate")
    }
  }
}

class JsonParser {
  private index = 0
  private readonly source: string

  constructor(source: string) {
    this.source = source
  }

  parse(): JsonValue {
    this.skipWhitespace()
    const value = this.parseValue()
    this.skipWhitespace()
    if (this.index !== this.source.length) {
      throw new Error("canonical JSON contains trailing data")
    }
    return value
  }

  private parseValue(): JsonValue {
    const current = this.source[this.index]
    switch (current) {
      case "{":
        return this.parseObject()
      case "[":
        return this.parseArray()
      case "\"":
        return this.parseString()
      case "t":
        return this.parseLiteral("true", true)
      case "f":
        return this.parseLiteral("false", false)
      case "n":
        return this.parseLiteral("null", null)
      default:
        return this.parseNumber()
    }
  }

  private parseObject(): JsonObject {
    this.index++
    this.skipWhitespace()
    const value = Object.create(null) as Record<string, JsonValue>
    const names = new Set<string>()
    if (this.consume("}")) {
      return value
    }
    while (true) {
      if (this.source[this.index] !== "\"") {
        throw new Error("canonical JSON object names must be strings")
      }
      const name = this.parseString()
      if (names.has(name)) {
        throw new Error(`canonical JSON contains duplicate object name ${JSON.stringify(name)}`)
      }
      names.add(name)
      this.skipWhitespace()
      this.expect(":")
      this.skipWhitespace()
      Object.defineProperty(value, name, {
        configurable: true,
        enumerable: true,
        value: this.parseValue(),
        writable: true,
      })
      this.skipWhitespace()
      if (this.consume("}")) {
        return value
      }
      this.expect(",")
      this.skipWhitespace()
    }
  }

  private parseArray(): JsonValue[] {
    this.index++
    this.skipWhitespace()
    const value: JsonValue[] = []
    if (this.consume("]")) {
      return value
    }
    while (true) {
      value.push(this.parseValue())
      this.skipWhitespace()
      if (this.consume("]")) {
        return value
      }
      this.expect(",")
      this.skipWhitespace()
    }
  }

  private parseString(): string {
    this.expect("\"")
    let value = ""
    while (this.index < this.source.length) {
      const unit = this.source.charCodeAt(this.index)
      const character = this.source[this.index] as string
      if (character === "\"") {
        this.index++
        return value
      }
      if (character === "\\") {
        this.index++
        value += this.parseEscape()
        continue
      }
      if (unit < 0x20) {
        throw new Error("canonical JSON strings cannot contain unescaped control characters")
      }
      if (unit >= 0xd800 && unit <= 0xdbff) {
        const next = this.source.charCodeAt(this.index + 1)
        if (next < 0xdc00 || next > 0xdfff) {
          throw new Error("canonical JSON contains an unpaired high surrogate")
        }
        value += this.source.slice(this.index, this.index + 2)
        this.index += 2
        continue
      }
      if (unit >= 0xdc00 && unit <= 0xdfff) {
        throw new Error("canonical JSON contains an unpaired low surrogate")
      }
      value += character
      this.index++
    }
    throw new Error("canonical JSON contains an unterminated string")
  }

  private parseEscape(): string {
    const escape = this.source[this.index++]
    switch (escape) {
      case "\"": return "\""
      case "\\": return "\\"
      case "/": return "/"
      case "b": return "\b"
      case "f": return "\f"
      case "n": return "\n"
      case "r": return "\r"
      case "t": return "\t"
      case "u": {
        const first = this.parseHexUnit()
        if (first >= 0xdc00 && first <= 0xdfff) {
          throw new Error("canonical JSON contains an unpaired low surrogate")
        }
        if (first >= 0xd800 && first <= 0xdbff) {
          if (this.source.slice(this.index, this.index + 2) !== "\\u") {
            throw new Error("canonical JSON contains an unpaired high surrogate")
          }
          this.index += 2
          const second = this.parseHexUnit()
          if (second < 0xdc00 || second > 0xdfff) {
            throw new Error("canonical JSON high surrogate is not followed by a low surrogate")
          }
          return String.fromCharCode(first, second)
        }
        return String.fromCharCode(first)
      }
      default:
        throw new Error(`canonical JSON contains invalid escape \\${escape ?? ""}`)
    }
  }

  private parseHexUnit(): number {
    const digits = this.source.slice(this.index, this.index + 4)
    if (!/^[0-9A-Fa-f]{4}$/.test(digits)) {
      throw new Error("canonical JSON contains an invalid Unicode escape")
    }
    this.index += 4
    return Number.parseInt(digits, 16)
  }

  private parseNumber(): number {
    const rest = this.source.slice(this.index)
    const match = /^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?/.exec(rest)
    if (!match) {
      throw new Error(`canonical JSON contains an unexpected token at byte ${this.index}`)
    }
    this.index += match[0].length
    const value = Number(match[0])
    if (!Number.isFinite(value)) {
      throw new Error(`canonical JSON number ${match[0]} is not an I-JSON double`)
    }
    return value
  }

  private parseLiteral<T extends null | boolean>(literal: string, value: T): T {
    if (this.source.slice(this.index, this.index + literal.length) !== literal) {
      throw new Error(`canonical JSON contains an unexpected token at byte ${this.index}`)
    }
    this.index += literal.length
    return value
  }

  private skipWhitespace(): void {
    while (/^[\x20\x09\x0a\x0d]$/.test(this.source[this.index] ?? "")) {
      this.index++
    }
  }

  private consume(expected: string): boolean {
    if (this.source[this.index] !== expected) {
      return false
    }
    this.index++
    return true
  }

  private expect(expected: string): void {
    if (!this.consume(expected)) {
      throw new Error(`canonical JSON expected ${JSON.stringify(expected)} at byte ${this.index}`)
    }
  }
}
