import { Navigate, useSearchParams } from "@solidjs/router";

function readParam(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

export function Setup() {
  const [params] = useSearchParams();
  const next = readParam(params["next"]);
  const href = next ? `/login?next=${encodeURIComponent(next)}` : "/login";

  return <Navigate href={href} />;
}
