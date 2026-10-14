import { useLocation, useNavigate } from "@solidjs/router";
import { createEffect, createMemo, Show, type JSX } from "solid-js";
import { ApiError } from "../lib/api";
import { onboardingRedirectPath, type AuthRequirement } from "../lib/auth";
import { useMe } from "../lib/me";
import { locationDestination, withNext } from "../lib/continuation";
import { errorDetails } from "../lib/error";
import { AuthError, AuthWorking, AuthWindow } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

export function RequireAuth(props: { children: JSX.Element; requirement?: AuthRequirement }) {
  const navigate = useNavigate();
  const location = useLocation();
  const me = useMe();
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
    <Show when={!me.isPending && !redirectPath()} fallback={<AuthWorking>Loading...</AuthWorking>}>
      <Show when={!me.isError} fallback={
        <AuthWindow>
          <AuthError
            title="Could not load your account"
            message="Please try again."
            details={errorDetails(me.error)}
            action={<button type="button" class={ui.button} onClick={() => void me.refetch()}>Retry</button>}
          />
        </AuthWindow>
      }>
        {props.children}
      </Show>
    </Show>
  );
}
