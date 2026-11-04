import { A } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { EmailSignIn } from "../components/EmailSignIn";
import { ApiError, chooseOrganization, postJson, releasePageIdentity, setOrganization } from "../lib/api";
import { getAccount, getAuthConfig, type Account, type AuthConfig } from "../lib/auth";
import { AUTH_MESSAGES, describeFailure, loadError, type Failure } from "../lib/error";
import { confirmSSOAssociation, getSSOConfirmation, restartSSO, type SSOConfirmation } from "../lib/sso";
import { AuthCopy, AuthLink, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { LinesSkeleton } from "../ui/Skeleton";
import { Notice } from "../ui/Notice";
import { cx, ui } from "../ui/styles";

// First association: the identity provider vouched for an email Helmr has not
// linked yet. The person proves that mailbox with an ordinary email sign-in,
// sees the account that will be used, and links it to the organization.
export function SSOConfirmPage() {
  const [confirmation, setConfirmation] = createSignal<SSOConfirmation>();
  const [failure, setFailure] = createSignal<Failure>();
  const [account, setAccount] = createSignal<Account>();
  const [config, setConfig] = createSignal<AuthConfig>();
  const [error, setError] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  // The company sign-in aged out on the way; it is repeated with a fresh one.
  const [stale, setStale] = createSignal(false);
  async function refresh() {
    try { setAccount(await getAccount()); } catch { setAccount(undefined); }
    setConfirmation(await getSSOConfirmation());
  }
  async function run(fn: () => Promise<void>) {
    setBusy(true); setError("");
    try { await fn(); } catch (cause) { setStale(cause instanceof ApiError && cause.code === "sso_reauthentication_required"); setError(loadError(cause, "Unable to continue. Try again.", AUTH_MESSAGES)); } finally { setBusy(false); }
  }
  onMount(() => void (async () => {
    try { setConfig(await getAuthConfig()); await refresh(); }
    catch (cause) { setFailure(describeFailure(cause, { invalid_token: "This sign-in has expired. Sign in with single sign-on again." }, "Your sign-in could not be continued.")); }
  })());
  const matches = (current: Account, pending: SSOConfirmation) => current.primary_email.toLowerCase() === pending.asserted_email.toLowerCase();
  const signOut = () => run(async () => {
    await postJson("/api/auth/logout", {}, { redirectOnUnauthorized: false });
    setOrganization(""); setAccount(undefined); releasePageIdentity();
  });
  return (
    <AuthWindow>
      <Show when={confirmation()} fallback={<Show when={failure()} fallback={<LinesSkeleton label="Continuing your sign-in" />}>{current => (
        <div class={ui.blockStack}>
          <AuthTitle>Sign in with single sign-on</AuthTitle>
          <Notice tone="danger">{current().message}</Notice>
          <A class={cx(ui.button, "auth-full")} href="/login">Sign in to Helmr</A>
        </div>
      )}</Show>}>
        {pending => (
          <div class={ui.blockStack}>
            <AuthTitle>Link your company account</AuthTitle>
            <p class="m-0 text-body">Your company identity provider signed you in as <strong>{pending().asserted_email}</strong> for {pending().organization_name}. Confirm that email once to link it to your Helmr account.</p>
            <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
            <Show when={stale()}><button type="button" class={ui.button} disabled={busy()} onClick={() => void run(async () => { if (!await restartSSO()) window.location.assign("/login"); })}>Sign in again with single sign-on</button></Show>
            <Show when={pending().asserted_email} fallback={<Notice tone="danger">Your identity provider did not send an email address. Ask an owner of {pending().organization_name} to configure it.</Notice>}>
              <Show when={account()} fallback={
                <Show when={config()?.methods.includes("email")} fallback={<Notice tone="warning">Email sign-in is not available on this server. Sign in to your account, then link your company account from Sign-in and security.</Notice>}>
                  <EmailSignIn email={pending().asserted_email} complete={() => void run(refresh)} />
                </Show>
              }>
                {current => (
                  <Show when={matches(current(), pending())} fallback={<>
                    <Notice tone="warning">You are signed in as <span class="font-mono">{current().primary_email}</span>. Sign out to continue as {pending().asserted_email}, or link single sign-on to this account from Sign-in and security.</Notice>
                    <button type="button" class={ui.button} disabled={busy()} onClick={() => void signOut()}>Sign out to use another account</button>
                  </>}>
                    <AuthCopy>Helmr will use the account {current().primary_email} for {pending().organization_name}.</AuthCopy>
                    <button type="button" class={ui.button} disabled={busy()} onClick={() => void run(async () => { const linked = await confirmSSOAssociation(); chooseOrganization(linked.organization_id, linked.redirect_after); })}>Link and continue</button>
                    <AuthLink disabled={busy()} onClick={() => void signOut()}>Use another account</AuthLink>
                  </Show>
                )}
              </Show>
            </Show>
          </div>
        )}
      </Show>
    </AuthWindow>
  );
}
