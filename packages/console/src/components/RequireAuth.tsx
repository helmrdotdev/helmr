import { A, useLocation, useNavigate } from "@solidjs/router";
import { createEffect, createMemo, Show, type JSX } from "solid-js";
import { ApiError } from "../lib/api";
import { onboardingRedirectPath, type AuthRequirement } from "../lib/auth";
import { useMe } from "../lib/me";
import { locationDestination, withNext } from "../lib/continuation";
import { errorDetails } from "../lib/error";
import { AuthError, AuthWindow } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

export function RequireAuth(props: { children: JSX.Element; requirement?: AuthRequirement }) {
  const navigate = useNavigate();
  const location = useLocation();
  const me = useMe();
  const authenticationLost = createMemo(() => me.error instanceof ApiError && (me.error.status === 401 || me.error.code === "authentication_required"));
  createEffect(() => {
    if (authenticationLost() && me.data) window.dispatchEvent(new CustomEvent("helmr-confirmation-required", {detail:{operator:false,signIn:true}}));
  });
  const redirectPath = createMemo(() => {
    const next = locationDestination(location);
    if (authenticationLost()) return me.data ? null : withNext("/login", next);
    if (!me.data || me.isError) return null;
    const path = onboardingRedirectPath(me.data, props.requirement ?? "project");
    return path ? withNext(path, next) : null;
  });
  createEffect(() => {
    const path = redirectPath();
    if (path) navigate(path, { replace: true });
  });
  return (
    <Show when={!me.isPending && !redirectPath()}>
      <Show when={!me.isError || (authenticationLost() && !!me.data)} fallback={
        <AuthWindow>
          <AuthError
            title="Could not load your account"
            message="Please try again."
            details={errorDetails(me.error)}
            action={<button type="button" class={ui.button} onClick={() => void me.refetch()}>Retry</button>}
          />
        </AuthWindow>
      }>
        <Show when={props.requirement === "session" || !me.data?.organization_access_denied} fallback={<AuthWindow><AuthError title="Organization access is unavailable" message="Confirm your sign-in to refresh access. If your membership is suspended, an administrator must resume it."/><A class={ui.button} href="/settings/account">Confirm sign-in</A><A class={ui.secondaryButton} href={withNext("/organizations",locationDestination(location))}>Choose another organization</A><button class={ui.secondaryButton} onClick={()=>void me.refetch()}>Retry</button></AuthWindow>}><div data-account-active style={{display:"contents"}}>{props.children}</div></Show>
      </Show>
    </Show>
  );
}
