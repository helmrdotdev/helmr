const encoder = new TextEncoder()
const encode = TextEncoder.prototype.encode.call.bind(
  TextEncoder.prototype.encode,
) as (encoder: TextEncoder, value: string) => Uint8Array
const charCodeAt = String.prototype.charCodeAt.call.bind(
  String.prototype.charCodeAt,
) as (value: string, index: number) => number

export function compareUTF8(left: string, right: string): number {
  const leftBytes = encode(encoder, left)
  const rightBytes = encode(encoder, right)
  const length = leftBytes.length < rightBytes.length
    ? leftBytes.length
    : rightBytes.length
  for (let index = 0; index < length; index++) {
    const difference = (leftBytes[index] as number) - (rightBytes[index] as number)
    if (difference !== 0) return difference
  }
  return leftBytes.length - rightBytes.length
}

export function hasOnlyUnicodeScalarValues(value: string): boolean {
  for (let index = 0; index < value.length; index++) {
    const unit = charCodeAt(value, index)
    if (unit >= 0xd800 && unit <= 0xdbff) {
      if (index + 1 === value.length) return false
      const next = charCodeAt(value, index + 1)
      if (next < 0xdc00 || next > 0xdfff) return false
      index++
    } else if (unit >= 0xdc00 && unit <= 0xdfff) {
      return false
    }
  }
  return true
}

export function assertUnicodeString(value: string): void {
  if (!hasOnlyUnicodeScalarValues(value)) {
    throw new Error("canonical JSON contains an unpaired surrogate")
  }
}
