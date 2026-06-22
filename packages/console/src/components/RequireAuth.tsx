import { useNavigate } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, onMount, Show, type JSX } from "solid-js";
import { getMe, onboardingRedirectPath } from "../lib/auth";
import { AuthLoading } from "../ui/AuthScreen";

function LoginRedirect() {
  const navigate = useNavigate();
  onMount(() => {
    navigate("/login", { replace: true });
  });
  return null;
}

export function RequireAuth(props: { children: JSX.Element; allowOnboarding?: boolean }) {
  const navigate = useNavigate();
  const me = createQuery(() => ({
    queryKey: ["me"],
    queryFn: getMe,
    retry: false,
    staleTime: 60_000,
  }));
  const onboardingPath = createMemo(() => {
    if (!me.data) return null;
    return onboardingRedirectPath(me.data);
  });
  const redirectPath = createMemo(() => {
    const path = onboardingPath();
    if (!path || props.allowOnboarding) return null;
    return path;
  });

  createEffect(() => {
    const path = redirectPath();
    if (path) navigate(path, { replace: true });
  });

  return (
    <Show when={!me.isPending} fallback={<AuthLoading>Loading...</AuthLoading>}>
      <Show
        when={!me.isError}
        fallback={<LoginRedirect />}
      >
        <Show when={!redirectPath()}>
          {props.children}
        </Show>
      </Show>
    </Show>
  );
}
