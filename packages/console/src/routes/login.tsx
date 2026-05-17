import { useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { getBootstrapStatus, startGitHubLogin, startMagicLinkLogin } from "../lib/auth";
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
  const [bootstrapRequired, setBootstrapRequired] = createSignal(false);
  const [bootstrapOwnerEmailConfigured, setBootstrapOwnerEmailConfigured] = createSignal(true);

  const nextPath = () => readParam(params["next"]);
  const initialOwnerSetup = () => bootstrapRequired() && bootstrapOwnerEmailConfigured();
  const setupUnavailable = () => bootstrapRequired() && !bootstrapOwnerEmailConfigured();
  const title = () => (initialOwnerSetup() ? "Set up initial owner" : "Sign in");
  const copy = () =>
    initialOwnerSetup()
      ? "Use the configured owner identity to create the first owner account."
      : "Choose a sign-in method to access runs, approvals, and credentials.";
  const buttonText = () => {
    if (busy()) return "Sending...";
    return initialOwnerSetup() ? "Send setup link" : "Send sign-in link";
  };
  const githubButtonText = () => {
    if (githubBusy()) return "Redirecting...";
    return initialOwnerSetup() ? "Set up with GitHub" : "Continue with GitHub";
  };

  onMount(async () => {
    try {
      const status = await getBootstrapStatus();
      setBootstrapRequired(status.bootstrap_required);
      setBootstrapOwnerEmailConfigured(status.bootstrap_owner_email_configured === true);
    } catch {
      // Keep the login page usable when setup status is temporarily unavailable.
    }
  });

  async function signIn(event: SubmitEvent) {
    event.preventDefault();
    if (setupUnavailable()) return;
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
    if (setupUnavailable()) return;
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
        <AuthTitle>{title()}</AuthTitle>
        <Show
          when={!setupUnavailable()}
          fallback={
            <p class={ui.error}>
              Initial owner setup is unavailable. Ask the operator to configure
              {" "}HELMR_BOOTSTRAP_OWNER_EMAIL, then return to this page.
            </p>
          }
        >
          <AuthCopy>{copy()}</AuthCopy>
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
      </Show>
      {error() && <p class={ui.error}>{error()}</p>}
    </AuthScreen>
  );
}
