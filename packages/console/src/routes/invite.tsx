import { useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { startGitHubInvite, startInviteMagicLink } from "../lib/auth";
import { errorMessage } from "../lib/error";
import { AuthActions, AuthCopy, AuthDivider, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

export function Invite() {
  const [params] = useSearchParams();
  const [error, setError] = createSignal<string | null>(null);
  const [sent, setSent] = createSignal(false);
  const [sentEmail, setSentEmail] = createSignal<string | null>(null);
  const [debugURL, setDebugURL] = createSignal<string | null>(null);
  const [token, setToken] = createSignal<string | null>(null);
  const [magicBusy, setMagicBusy] = createSignal(false);
  const [githubBusy, setGitHubBusy] = createSignal(false);

  onMount(async () => {
    const tokenRaw = params["token"];
    const token = Array.isArray(tokenRaw) ? tokenRaw[0] : tokenRaw;
    history.replaceState({}, "", "/invite");

    if (!token) {
      setError("Missing invite token.");
      return;
    }

    setToken(token);
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
      const kind = e instanceof ApiError ? e.errorKind : null;
      setError(errorMessage(kind, e instanceof Error ? e.message : "Invite failed."));
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
      const kind = e instanceof ApiError ? e.errorKind : null;
      setError(errorMessage(kind, e instanceof Error ? e.message : "Invite failed."));
      setGitHubBusy(false);
    }
  }

  return (
    <AuthScreen>
      <Show when={token()} fallback={
        <>
          <AuthTitle>Invite failed</AuthTitle>
          <p class={ui.error}>{error() ?? "Missing invite token."}</p>
          <a href="/login">Back to login</a>
        </>
      }>
        <Show when={error()}>
          <p class={ui.error}>{error()}</p>
        </Show>
        <Show when={sent()} fallback={
          <>
            <AuthTitle>Accept invite</AuthTitle>
            <AuthCopy>Choose how to verify this invitation.</AuthCopy>
            <button
              class={ui.button}
              type="button"
              onClick={continueWithGitHub}
              disabled={githubBusy() || magicBusy()}
            >
              {githubBusy() ? "Redirecting..." : "Continue with GitHub"}
            </button>
            <AuthDivider>OR</AuthDivider>
            <button
              class={ui.secondaryButton}
              type="button"
              onClick={sendMagicLink}
              disabled={githubBusy() || magicBusy()}
            >
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
              <button
                class={ui.secondaryButton}
                type="button"
                onClick={() => {
                  window.location.href = url();
                }}
              >
                Continue with dev link
              </button>
            )}
          </Show>
          <AuthDivider>OR</AuthDivider>
          <AuthActions>
            <button
              class={ui.button}
              type="button"
              onClick={continueWithGitHub}
              disabled={githubBusy() || magicBusy()}
            >
              {githubBusy() ? "Redirecting..." : "Continue with GitHub"}
            </button>
            <button
              class={ui.secondaryButton}
              type="button"
              onClick={sendMagicLink}
              disabled={githubBusy() || magicBusy()}
            >
              {magicBusy() ? "Sending..." : "Send another invite link"}
            </button>
          </AuthActions>
        </Show>
      </Show>
    </AuthScreen>
  );
}
