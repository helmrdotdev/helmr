import { useLocation, useNavigate } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createEffect, createMemo, Show, type JSX } from "solid-js";
import { ApiError } from "../lib/api";
import { getMe, onboardingRedirectPath, type AuthRequirement } from "../lib/auth";
import { locationDestination, withNext } from "../lib/continuation";
import { AuthLoading, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

export function RequireAuth(props: { children: JSX.Element; requirement?: AuthRequirement }) {
  const navigate = useNavigate();
  const location = useLocation();
  const me = createQuery(() => ({
    queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000,
  }));
  const redirectPath = createMemo(() => {
    const next = locationDestination(location);
    if (me.error instanceof ApiError && me.error.status === 401) return withNext("/login", next);
    if (!me.data || me.isError) return null;
    const path = onboardingRedirectPath(me.data, props.requirement ?? "project");
    return path ? withNext(path, next) : null;
  });
  createEffect(() => {
    const path = redirectPath();
    if (path) navigate(path, { replace: true });
  });
  return (
    <Show when={!me.isPending && !redirectPath()} fallback={<AuthLoading>Loading...</AuthLoading>}>
      <Show when={!me.isError} fallback={
        <AuthScreen>
          <AuthTitle>Could not load your account</AuthTitle>
          <p class={ui.error}>Please try again.</p>
          <button class={ui.button} onClick={() => void me.refetch()}>Retry</button>
        </AuthScreen>
      }>
        {props.children}
      </Show>
    </Show>
  );
}
