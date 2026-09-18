// Return destinations travel with each request, never in shared browser storage.
const baseURL = "https://console.invalid";
const intermediatePaths = new Set([
  "/login", "/auth/github/callback", "/auth/magic-link/callback",
  "/organizations/new", "/projects/new", "/access-required",
]);

export function returnPath(value: string | null | undefined): string {
  if (!value || value.length > 256 || !value.startsWith("/") || value.startsWith("//") || /[\\\u0000-\u001f\u007f]/.test(value)) return "/";
  try {
    const url = new URL(value, baseURL);
    const path = decodeURIComponent(url.pathname).replace(/\/+$/, "") || "/";
    if (url.origin !== baseURL || path.startsWith("//") || path.includes("\\") || (intermediatePaths.has(path) && !(path === "/projects/new" && !url.search && !url.hash))) return "/";
    return url.pathname + url.search + url.hash;
  } catch {
    return "/";
  }
}

export function withNext(path: string, next: string): string {
  const destination = returnPath(next);
  return destination === "/" ? path : `${path}?${new URLSearchParams({ next: destination })}`;
}

export function locationDestination(location: { pathname: string; search: string; hash?: string }): string {
  if (intermediatePaths.has(location.pathname) && !(location.pathname === "/projects/new" && !location.search && !location.hash)) {
    return returnPath(new URLSearchParams(location.search).get("next"));
  }
  return returnPath(location.pathname + location.search + (location.hash ?? ""));
}
