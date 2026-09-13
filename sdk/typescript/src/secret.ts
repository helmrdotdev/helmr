const secretNamePattern = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$/

export function validateSecretName(value: string): void {
  if (typeof value !== "string" || !secretNamePattern.test(value)) {
    throw new Error("Secret name is invalid")
  }
}
