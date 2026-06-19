import { useSearchParams } from "@solidjs/router";
import { createSignal, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { startGitHubLogin, startMagicLinkLogin } from "../lib/auth";
import { errorMessage } from "../lib/error";
import { AuthCopy, AuthDivider, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

function readParam(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

export function Login() {
  const [params] = useSearchParams();
  const [busy, setBusy] = createSignal(false);
  const [githubBusy, setGitHubBusy] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [email, setEmail] = createSignal("");
  const [sentEmail, setSentEmail] = createSignal<string | null>(null);
  const [debugURL, setDebugURL] = createSignal<string | null>(null);

  const nextPath = () => {
    const next = readParam(params["next"]);
    if (!next) return undefined;
    return next;
  };
  const buttonText = () => {
    if (busy()) return "Sending...";
    return "Send sign-in link";
  };
  const githubButtonText = () => {
    if (githubBusy()) return "Redirecting...";
    return "Continue with GitHub";
  };

  async function signIn(event: SubmitEvent) {
    event.preventDefault();
    const trimmedEmail = email().trim();
    if (!trimmedEmail) {
      setError("Enter your email address.");
      return;
    }

    setBusy(true);
    setError(null);
    setDebugURL(null);
    try {
      const next = nextPath();
      const result = await startMagicLinkLogin(
        next ? { email: trimmedEmail, next } : { email: trimmedEmail },
      );
      setSentEmail(trimmedEmail);
      setDebugURL(result.debug_url ?? null);
    } catch (e) {
      const kind = e instanceof ApiError ? e.errorKind : null;
      setError(errorMessage(kind, e instanceof Error ? e.message : "Sign in failed."));
    } finally {
      setBusy(false);
    }
  }

  async function signInWithGitHub() {
    setGitHubBusy(true);
    setError(null);
    try {
      const { redirect_url } = await startGitHubLogin(nextPath());
      window.location.href = redirect_url;
    } catch (e) {
      const kind = e instanceof ApiError ? e.errorKind : null;
      setError(errorMessage(kind, e instanceof Error ? e.message : "Sign in failed."));
      setGitHubBusy(false);
    }
  }

  return (
    <AuthScreen>
      <Show
        when={!sentEmail()}
        fallback={
          <>
            <AuthTitle>Check your email</AuthTitle>
            <AuthCopy>
              We sent a sign-in link to <strong>{sentEmail()}</strong>. Open it in this browser
              to continue.
            </AuthCopy>
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
          </>
        }
      >
        <AuthTitle>Sign in</AuthTitle>
        <AuthCopy>Choose a sign-in method to access runs, waitpoints, and credentials.</AuthCopy>
        <button
          class={ui.button}
          type="button"
          onClick={signInWithGitHub}
          disabled={githubBusy() || busy()}
        >
          {githubButtonText()}
        </button>
        <AuthDivider>OR</AuthDivider>
        <form onSubmit={signIn}>
          <label class={ui.field}>
            <span>Email</span>
            <input
              class={ui.input}
              type="email"
              autocomplete="email"
              value={email()}
              onInput={(event) => setEmail(event.currentTarget.value)}
              required
              disabled={busy()}
              placeholder="you@example.com"
            />
          </label>
          <button class={ui.button} type="submit" disabled={busy()}>
            {buttonText()}
          </button>
        </form>
      </Show>
      {error() && <p class={ui.error}>{error()}</p>}
    </AuthScreen>
  );
}
