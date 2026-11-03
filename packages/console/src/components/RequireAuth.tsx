import { useLocation, useNavigate } from "@solidjs/router";
import { createEffect, createMemo, createSignal, Show, type JSX } from "solid-js";
import { ApiError } from "../lib/api";
import { onboardingRedirectPath, type AuthRequirement } from "../lib/auth";
import { useMe } from "../lib/me";
import { locationDestination, withNext } from "../lib/continuation";
import { AUTH_MESSAGES, errorDetails, loadError } from "../lib/error";
import { startSSO } from "../lib/sso";
import { AuthCopy, AuthError, AuthTitle, AuthWindow, AuthLink, AuthLinks } from "../ui/AuthWindow";
import { cx, ui } from "../ui/styles";

export function RequireAuth(props: { children: JSX.Element; requirement?: AuthRequirement }) {
  const navigate = useNavigate();
  const location = useLocation();
  const me = useMe();
  const authenticationLost = createMemo(() => me.error instanceof ApiError && (me.error.status === 401 || me.error.code === "authentication_required"));
  createEffect(() => {
    if (authenticationLost() && me.data) window.dispatchEvent(new CustomEvent("helmr-confirmation-required", {detail:{operator:false}}));
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
        <Show when={props.requirement === "session" || !me.data?.organization_sso} fallback={<SSORequired organization={me.data!.organization_sso!} next={locationDestination(location)} />}>
        <Show when={props.requirement === "session" || !me.data?.organization_access_denied} fallback={<AuthWindow><AuthError title="Organization access is unavailable" message="Confirm your sign-in to refresh access. If your membership is suspended, an administrator must resume it."/><button class={ui.button} onClick={() => window.dispatchEvent(new CustomEvent("helmr-confirmation-required", {detail:{operator:false}}))}>Confirm sign-in</button><AuthLinks><AuthLink href={withNext("/organizations",locationDestination(location))}>Choose another organization</AuthLink><AuthLink onClick={()=>void me.refetch()}>Retry</AuthLink></AuthLinks></AuthWindow>}><div data-account-active style={{display:"contents"}}>{props.children}</div></Show>
        </Show>
      </Show>
    </Show>
  );
}

// The selected organization requires single sign-on and this browser holds no
// live proof for it, or one older than the member's sign-in cutoff.
function SSORequired(props: { organization: { id: string; slug: string; name: string; reauthenticate: boolean }; next: string }) {
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal("");
  const start = async () => {
    setBusy(true); setError("");
    try { window.location.assign((await startSSO({ organization: props.organization.slug, next: props.next, reauthenticate: props.organization.reauthenticate })).redirect_url); }
    catch (cause) { setError(loadError(cause, "Single sign-on could not start.", AUTH_MESSAGES)); setBusy(false); }
  };
  return (
    <AuthWindow>
      <AuthTitle>{props.organization.name} uses single sign-on</AuthTitle>
      <AuthCopy>{props.organization.reauthenticate ? "Sign in through your company identity provider again to continue." : "Sign in through your company identity provider to open this organization."}</AuthCopy>
      <Show when={error()}><AuthError message={error()} /></Show>
      <button type="button" class={cx(ui.button, "auth-full")} disabled={busy()} onClick={() => void start()}>Continue with company SSO</button>
      <AuthLinks>
        <AuthLink href={withNext("/organizations", props.next)}>Choose another organization</AuthLink>
        <AuthLink href="/settings/security">Company email differs from your Helmr email? Link it from Sign-in and security</AuthLink>
        <AuthLink href={`/sso/recover?organization=${encodeURIComponent(props.organization.id)}`}>Owners: use a recovery code</AuthLink>
      </AuthLinks>
    </AuthWindow>
  );
}
