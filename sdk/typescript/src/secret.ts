declare const secretNameRefTypeBrand: unique symbol

const secretNameRefBrand = Symbol.for("helmr.sdk.v0.secret-name-ref")
const secretNamePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/

export interface SecretNameRef {
  readonly [secretNameRefTypeBrand]: true
  readonly name: string
}

export interface SecretReferences {
  fromName(name: string): SecretNameRef
}

class SecretName {
  readonly name: string

  constructor(name: string) {
    validateSecretName(name)
    this.name = name
    Object.defineProperty(this, secretNameRefBrand, { value: true })
    Object.freeze(this)
  }
}

export const secrets: SecretReferences = Object.freeze({
  fromName(name: string): SecretNameRef {
    return new SecretName(name) as SecretNameRef
  },
})

export function inspectSecretNameRef(value: unknown): string | undefined {
  if (
    typeof value !== "object" ||
    value === null ||
    (value as Record<PropertyKey, unknown>)[secretNameRefBrand] !== true
  ) {
    return undefined
  }
  const name = (value as { readonly name?: unknown }).name
  if (typeof name !== "string") {
    throw new Error("private Secret name reference is invalid")
  }
  validateSecretName(name)
  return name
}

export function validateSecretName(value: string): void {
  if (!secretNamePattern.test(value)) {
    throw new Error("Secret name is invalid")
  }
}
