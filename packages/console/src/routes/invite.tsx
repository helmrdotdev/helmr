import { A, useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { startGitHubInvite, startInviteMagicLink } from "../lib/auth";
import { AUTH_MESSAGES, describeFailure, type Failure } from "../lib/error";
import { AuthActions, AuthCopy, AuthDivider, AuthError, AuthMissing, AuthWindow, AuthTitle } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

const failure = (error: unknown) => describeFailure(error, AUTH_MESSAGES, "This invite could not be accepted. Try again, or ask for a new invite link.");

export function Invite() {
  const [params] = useSearchParams();
  const [error, setError] = createSignal<Failure | null>(null);
  const [sent, setSent] = createSignal(false);
  const [sentEmail, setSentEmail] = createSignal<string | null>(null);
  const [debugURL, setDebugURL] = createSignal<string | null>(null);
  const [token, setToken] = createSignal<string | null>(null);
  const [checked, setChecked] = createSignal(false);
  const [magicBusy, setMagicBusy] = createSignal(false);
  const [githubBusy, setGitHubBusy] = createSignal(false);

  onMount(() => {
    const tokenRaw = params["token"];
    const value = Array.isArray(tokenRaw) ? tokenRaw[0] : tokenRaw;
    history.replaceState({}, "", "/invite");
    setToken(value || null);
    setChecked(true);
  });

  async function sendMagicLink() {
    const currentToken = token();
    if (!currentToken) return;
    setMagicBusy(true);
    setError(null);
    try {
      const result = await startInviteMagicLink(currentToken);
      setSent(true);
      setSentEmail(result.email ?? null);
      setDebugURL(result.debug_url ?? null);
    } catch (e) {
      setError(failure(e));
    } finally {
      setMagicBusy(false);
    }
  }

  async function continueWithGitHub() {
    const currentToken = token();
    if (!currentToken) return;
    setGitHubBusy(true);
    setError(null);
    try {
      const { redirect_url } = await startGitHubInvite(currentToken);
      window.location.href = redirect_url;
    } catch (e) {
      setError(failure(e));
      setGitHubBusy(false);
    }
  }

  return (
    <AuthWindow>
      <Show when={token()} fallback={
        <Show when={checked()}>
          <AuthMissing title="This invite link is incomplete" action={<A class={ui.secondaryButton} href="/login">Back to login</A>}>
            The link is missing its invitation. Open the full link from your invite email, or ask the person who invited you for a new one.
          </AuthMissing>
        </Show>
      }>
        <Show when={sent()} fallback={
          <>
            <AuthTitle>Accept invite</AuthTitle>
            <AuthCopy>Choose how to verify this invitation.</AuthCopy>
            <button class={ui.button} type="button" onClick={continueWithGitHub} disabled={githubBusy() || magicBusy()}>
              {githubBusy() ? "Redirecting..." : "Continue with GitHub"}
            </button>
            <AuthDivider>OR</AuthDivider>
            <button class={ui.secondaryButton} type="button" onClick={sendMagicLink} disabled={githubBusy() || magicBusy()}>
              {magicBusy() ? "Sending..." : "Send invite link"}
            </button>
          </>
        }>
          <AuthTitle>Check your email</AuthTitle>
          <Show
            when={sentEmail()}
            fallback={<AuthCopy>We sent a sign-in link to the email address on this invite.</AuthCopy>}
          >
            {(email) => (
              <AuthCopy>
                We sent a sign-in link to <strong>{email()}</strong>. Open it in this browser
                to accept the invite.
              </AuthCopy>
            )}
          </Show>
          <Show when={debugURL()}>
            {(url) => (
              <button class={ui.secondaryButton} type="button" onClick={() => { window.location.href = url(); }}>
                Continue with dev link
              </button>
            )}
          </Show>
          <AuthDivider>OR</AuthDivider>
          <AuthActions>
            <button class={ui.secondaryButton} type="button" onClick={sendMagicLink} disabled={githubBusy() || magicBusy()}>
              {magicBusy() ? "Sending..." : "Send another invite link"}
            </button>
            <button class={ui.button} type="button" onClick={continueWithGitHub} disabled={githubBusy() || magicBusy()}>
              {githubBusy() ? "Redirecting..." : "Continue with GitHub"}
            </button>
          </AuthActions>
        </Show>
        <Show when={error()}>{(current) => <AuthError message={current().message} details={current().details} />}</Show>
      </Show>
    </AuthWindow>
  );
}
