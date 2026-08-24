import { assertUnicodeString } from "./utf8"

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

const textEncoder = new TextEncoder()

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
