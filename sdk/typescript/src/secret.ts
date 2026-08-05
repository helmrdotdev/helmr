declare const secretAddressTypeBrand: unique symbol

const secretAddressBrand = Symbol.for("helmr.sdk.v0.secret-address")
const secretNamePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/

export interface SecretAddress {
  readonly [secretAddressTypeBrand]: true
  readonly name: string
}

export interface SecretAddresses {
  fromName(name: string): SecretAddress
}

class SecretNameAddress {
  readonly name: string

  constructor(name: string) {
    validateSecretName(name)
    this.name = name
    Object.defineProperty(this, secretAddressBrand, { value: true })
    Object.freeze(this)
  }
}

export const secrets: SecretAddresses = Object.freeze({
  fromName(name: string): SecretAddress {
    return new SecretNameAddress(name) as SecretAddress
  },
})

export function inspectSecretAddress(value: unknown): string | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    (value as Record<PropertyKey, unknown>)[secretAddressBrand] !== true
  ) {
    return undefined
  }
  const name = (value as { readonly name?: unknown }).name
  if (typeof name !== "string") {
    throw new Error("private Secret address is invalid")
  }
  validateSecretName(name)
  return name
}

export function validateSecretName(value: string): void {
  if (!secretNamePattern.test(value)) {
    throw new Error("Secret name is invalid")
  }
}
