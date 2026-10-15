import { formatID } from "../../ui/id";

export function deploymentHref(id: string): string {
  return `/deployments/${encodeURIComponent(id)}`;
}

// A version names its Deployment by date and ID ("20260101.<uuid>"); the short
// form keeps the date and the ID's short form: "20260101 · 000000000501".
export function shortVersion(version: string): string {
  const separator = version.indexOf(".");
  return separator > 0 ? `${version.slice(0, separator)} · ${formatID(version.slice(separator + 1))}` : formatID(version);
}
