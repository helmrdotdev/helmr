import { A, useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { EmailSignIn } from "../components/EmailSignIn";
import { ApiError, chooseOrganization, postJson, releasePageIdentity, setOrganization } from "../lib/api";
import { getAccount, getAuthConfig, type Account, type AuthConfig } from "../lib/auth";
import { describeFailure, errorMessage, type Failure } from "../lib/error";
import { assertPasskey } from "../lib/passkey";
import { AuthCopy, AuthDivider, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { LinesSkeleton } from "../ui/Skeleton";
import { Notice } from "../ui/Notice";
import { cx, ui } from "../ui/styles";

type Summary = { organization_id: string; organization_name: string; email: string; expires_at: string };

const LINK_FAILURE = "The link is invalid or has expired.";
// Only these mean the link itself is dead; anything else is worth retrying.
const DEAD_LINK = new Set(["invalid_token", "bad_request", "not_found"]);

// An invitation names the organization and the invited email. Accepting needs that
// email's account, so a different signed-in account is told why and offered the way out.
export function Invite() {
  const [params] = useSearchParams();
  const [token, setToken] = createSignal("");
  const [summary, setSummary] = createSignal<Summary>();
  const [linkFailure, setLinkFailure] = createSignal<Failure>();
  const [loadFailure, setLoadFailure] = createSignal<Failure>();
  const [account, setAccount] = createSignal<Account>();
  const [config, setConfig] = createSignal<AuthConfig>();
  const [error, setError] = createSignal("");
  const [busy, setBusy] = createSignal(false);
  async function refresh() { try { setAccount(await getAccount()); } catch { setAccount(undefined); } }
  async function run(fn: () => Promise<void>) {
    setBusy(true); setError("");
    try { await fn(); } catch (cause) { setError(errorMessage(cause, "Unable to continue. Try again.")); } finally { setBusy(false); }
  }
  onMount(() => {
    const raw = params["token"];
    const value = Array.isArray(raw) ? raw[0] : raw;
    history.replaceState({}, "", "/invite");
    setToken(value ?? "");
    if (!value) { setLinkFailure({ message: "Open the full invitation link from your email.", details: undefined }); return; }
    void (async () => {
      setBusy(true);
      try {
        setSummary(await postJson<{ token: string }, Summary>("/api/auth/invitations/resolve", { token: value }, { redirectOnUnauthorized: false }));
        setConfig(await getAuthConfig());
        await refresh();
      } catch (cause) {
        if (cause instanceof ApiError && DEAD_LINK.has(cause.code ?? "")) setLinkFailure({ message: LINK_FAILURE, details: undefined });
        else setLoadFailure(describeFailure(cause, {}, "Your invitation could not be opened. Try again."));
      }
      finally { setBusy(false); }
    })();
  });
  const matches = (current: Account, invited: Summary) => current.primary_email.toLowerCase() === invited.email.toLowerCase();
  const signOut = () => run(async () => {
    await postJson("/api/auth/logout", {}, { redirectOnUnauthorized: false });
    setOrganization(""); setAccount(undefined); releasePageIdentity();
  });
  return (
    <AuthWindow>
      <Show when={!linkFailure()} fallback={
        <div class={ui.blockStack}>
          <AuthTitle>This invitation link does not work</AuthTitle>
          <Notice tone="danger">{linkFailure()!.message}</Notice>
          <p class={cx(ui.muted, "m-0")}>Invitations expire and work once. Ask the person who invited you to send a new one.</p>
          <div><A class={ui.secondaryButton} href="/login">Sign in to Helmr</A></div>
        </div>
      }>
        <Show when={summary()} fallback={<Show when={loadFailure()} fallback={<LinesSkeleton label="Opening your invitation" />}>{failure => (
          <div class={ui.blockStack}>
            <AuthTitle>Join an organization</AuthTitle>
            <Notice tone="danger">{failure().message}</Notice>
            <div><button type="button" class={ui.button} onClick={() => window.location.reload()}>Try again</button></div>
          </div>
        )}</Show>}>
          {invited => (
            <div class={ui.blockStack}>
              <AuthTitle>Join {invited().organization_name}</AuthTitle>
              <p class="m-0 text-body">This invitation is for <strong>{invited().email}</strong>. Accepting gives that account membership in {invited().organization_name}.</p>
              <Show when={error()}><Notice tone="danger">{error()}</Notice></Show>
              <Show when={account()} fallback={
                <div>
                  <Show when={config()?.methods.includes("email")}><EmailSignIn email={invited().email} invitationToken={token()} complete={() => void refresh()} /></Show>
                  <Show when={config()?.methods.includes("passkey")}>
                    <AuthDivider>or</AuthDivider>
                    <button type="button" class={cx(ui.secondaryButton, "w-full")} disabled={busy()} onClick={() => void run(async () => { await assertPasskey("/api/auth/passkey/options", "/api/auth/passkey/verify"); await refresh(); })}>Sign in with a passkey</button>
                  </Show>
                  <AuthCopy><A href="/login">Other sign-in methods</A>: sign in, then reopen your invitation link to accept.</AuthCopy>
                </div>
              }>
                {current => (
                  <Show when={matches(current(), invited())} fallback={<>
                    <Notice tone="warning">You are signed in as <span class="font-mono">{current().primary_email}</span>. Sign in with the invited email to accept.</Notice>
                    <div><button type="button" class={ui.button} disabled={busy()} onClick={() => void signOut()}>Sign out to use another account</button></div>
                  </>}>
                    <p class={cx(ui.muted, "m-0")}>Signed in as {current().primary_email}.</p>
                    <div class={ui.buttonGroup}>
                      <button type="button" class={ui.button} disabled={busy()} onClick={() => void run(async () => { const accepted = await postJson<{ token: string }, { organization_id: string }>("/api/auth/invitations/accept", { token: token() }, { redirectOnUnauthorized: false }); chooseOrganization(accepted.organization_id); })}>Accept invitation</button>
                      <button type="button" class={ui.secondaryButton} disabled={busy()} onClick={() => void signOut()}>Use another account</button>
                    </div>
                  </Show>
                )}
              </Show>
            </div>
          )}
        </Show>
      </Show>
    </AuthWindow>
  );
}
