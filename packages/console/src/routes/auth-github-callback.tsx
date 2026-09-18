import { returnPath } from "../lib/continuation";
import { useNavigate, useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { finishGitHubAuth } from "../lib/auth";
import { errorMessage } from "../lib/error";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

function readParam(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

export function AuthGitHubCallback() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [error, setError] = createSignal<string | null>(null);

  onMount(async () => {
    const code = readParam(params["code"]) ?? "";
    const state = readParam(params["state"]) ?? "";
    const oauthError = readParam(params["error"]);
    const errorDescription = readParam(params["error_description"]);
    history.replaceState({}, "", "/auth/github/callback");

    try {
      const result = await finishGitHubAuth({
        code,
        state,
        ...(oauthError !== undefined ? { error: oauthError } : {}),
        ...(errorDescription !== undefined ? { error_description: errorDescription } : {}),
      });
      navigate(returnPath(result.redirect_after), { replace: true });
    } catch (e) {
      const kind = e instanceof ApiError ? e.code : null;
      setError(errorMessage(kind, e instanceof Error ? e.message : "Sign in failed."));
    }
  });

  return (
    <AuthScreen>
      <Show when={error()} fallback={<AuthCopy>Signing you in...</AuthCopy>}>
        <AuthTitle>Sign in failed</AuthTitle>
        <p class={ui.error}>{error()}</p>
        <AuthCopy>If you started from the CLI, run helmr login again in your terminal.</AuthCopy>
        <a href="/login">Sign in to the console</a>
      </Show>
    </AuthScreen>
  );
}
