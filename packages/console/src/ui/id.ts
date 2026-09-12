const TAIL = 12;

export function formatID(value: string): string {
  const separator = value.indexOf(":");
  if (separator > 0 && separator < 12) {
    const digest = value.slice(separator + 1);
    return digest.length > TAIL ? `${value.slice(0, separator)}:…${digest.slice(-TAIL)}` : value;
  }
  return value.length > TAIL + 4 ? `…${value.slice(-TAIL)}` : value;
}
