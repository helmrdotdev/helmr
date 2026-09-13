export function canonicalSecretOrigin(value: string): string {
  if (typeof value !== "string" || value.trim() !== value || !/^https:\/\//i.test(value)) {
    throw new Error("Secret origin must be an exact HTTPS origin")
  }
  const authority = value.replace(/^https:\/\//i, "").replace(/\/$/, "")
  if (!/^[A-Za-z0-9.-]+(?::[0-9]+)?$/.test(authority)) throw new Error("Secret origin must contain only a DNS hostname and optional port")
  const [rawHost, port] = authority.split(":")
  const host = rawHost!.toLowerCase()
  if (host.length > 253 || host === "localhost" || host.endsWith(".localhost") || /^[0-9.]+$/.test(host) || host.split(".").some(label => !/^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label))) {
    throw new Error("Secret origin must use a DNS hostname without wildcards or IP addresses")
  }
  if (port !== undefined && (!/^[1-9][0-9]*$/.test(port) || Number(port) > 65535)) throw new Error("Secret origin port is invalid")
  return `https://${host}${port === undefined || port === "443" ? "" : `:${port}`}`
}
