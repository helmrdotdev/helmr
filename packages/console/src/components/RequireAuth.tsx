import { useNavigate } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { onMount, Show, type JSX } from "solid-js";
import { getMe } from "../lib/auth";
import { AuthLoading } from "../ui/AuthScreen";

function LoginRedirect() {
  const navigate = useNavigate();
  onMount(() => {
    const next = encodeURIComponent(window.location.pathname + window.location.search);
    navigate(`/login?next=${next}`, { replace: true });
  });
  return null;
}

export function RequireAuth(props: { children: JSX.Element }) {
  const me = createQuery(() => ({
    queryKey: ["me"],
    queryFn: getMe,
    retry: false,
    staleTime: 60_000,
  }));

  return (
    <Show when={!me.isPending} fallback={<AuthLoading>Loading...</AuthLoading>}>
      <Show
        when={!me.isError}
        fallback={<LoginRedirect />}
      >
        {props.children}
      </Show>
    </Show>
  );
}
