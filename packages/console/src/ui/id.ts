const TAIL = 12;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-([0-9a-f]{12})$/i;

// Short form of an identifier: the last hyphen segment of a UUID, or the
// algorithm plus the last 12 characters of a digest. Any other value is shown
// unchanged. There is no leading ellipsis; the full value belongs in `title`.
export function formatID(value: string): string {
  const separator = value.indexOf(":");
  if (separator > 0 && separator < 12) {
    const digest = value.slice(separator + 1);
    return digest.length > TAIL ? `${value.slice(0, separator)}:${digest.slice(-TAIL)}` : value;
  }
  return UUID.exec(value)?.[1] ?? value;
}
