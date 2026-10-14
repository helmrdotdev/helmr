import { useLocation } from "@solidjs/router";
import { locationDestination } from "../lib/continuation";
import { createSignal, Show } from "solid-js";
import { startGitHubLogin, startMagicLinkLogin } from "../lib/auth";
import { AUTH_MESSAGES, describeFailure, type Failure } from "../lib/error";
import { AuthCopy, AuthDivider, AuthError, AuthWindow, AuthTitle } from "../ui/AuthWindow";
import { Field, TextInput } from "../ui/Field";
import { ui } from "../ui/styles";

const EMAIL_MESSAGES = {
  ...AUTH_MESSAGES,
  bad_request: "Enter a valid email address.",
  service_unavailable: "Email sign-in is not configured on this server. Continue with GitHub, or contact your administrator.",
};
const GITHUB_MESSAGES = {
  ...AUTH_MESSAGES,
  service_unavailable: "GitHub sign-in is not configured on this server. Use an email sign-in link, or contact your administrator.",
};
const START_FALLBACK = "Sign in could not start. Try again, or choose another sign-in method.";

export function Login() {
  const location = useLocation();
  const [busy, setBusy] = createSignal(false);
  const [githubBusy, setGitHubBusy] = createSignal(false);
  const [error, setError] = createSignal<Failure | null>(null);
  const [email, setEmail] = createSignal("");
  const [sentEmail, setSentEmail] = createSignal<string | null>(null);
  const [debugURL, setDebugURL] = createSignal<string | null>(null);

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
      setError({ message: "Enter your email address.", details: undefined });
      return;
    }

    setBusy(true);
    setError(null);
    setDebugURL(null);
    try {
      const result = await startMagicLinkLogin({ email: trimmedEmail, next: locationDestination(location) });
      setSentEmail(trimmedEmail);
      setDebugURL(result.debug_url ?? null);
    } catch (e) {
      setError(describeFailure(e, EMAIL_MESSAGES, START_FALLBACK));
    } finally {
      setBusy(false);
    }
  }

  async function signInWithGitHub() {
    setGitHubBusy(true);
    setError(null);
    try {
      const { redirect_url } = await startGitHubLogin(locationDestination(location));
      window.location.href = redirect_url;
    } catch (e) {
      setError(describeFailure(e, GITHUB_MESSAGES, START_FALLBACK));
      setGitHubBusy(false);
    }
  }

  return (
    <AuthWindow>
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
        <AuthCopy>Choose a sign-in method to access your Sessions, Computers, and credentials.</AuthCopy>
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
          <Field label="Email">
            <TextInput
              type="email"
              autocomplete="email"
              value={email()}
              onInput={(event) => setEmail(event.currentTarget.value)}
              required
              disabled={busy()}
              placeholder="you@example.com"
            />
          </Field>
          <button class={ui.button} type="submit" disabled={busy()}>
            {buttonText()}
          </button>
        </form>
      </Show>
      <Show when={error()}>{(current) => <AuthError message={current().message} details={current().details} />}</Show>
    </AuthWindow>
  );
}
