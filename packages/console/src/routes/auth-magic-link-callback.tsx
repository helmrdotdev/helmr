import { useNavigate, useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { finishMagicLink } from "../lib/auth";
import { errorMessage } from "../lib/error";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

function readParam(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

export function AuthMagicLinkCallback() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [error, setError] = createSignal<string | null>(null);

  onMount(async () => {
    const token = readParam(params["token"]);
    history.replaceState({}, "", "/auth/magic-link/callback");

    if (!token) {
      setError(errorMessage("magic_link_token_missing", "This sign-in link is missing its token."));
      return;
    }

    try {
      const { redirect_after } = await finishMagicLink(token);
      navigate(redirect_after, { replace: true });
    } catch (e) {
      const kind = e instanceof ApiError ? e.errorKind : null;
      setError(errorMessage(kind, e instanceof Error ? e.message : "Sign in failed."));
    }
  });

  return (
    <AuthScreen>
      <Show when={error()} fallback={<AuthCopy>Signing you in...</AuthCopy>}>
        <AuthTitle>Sign in failed</AuthTitle>
        <p class={ui.error}>{error()}</p>
        <a href="/login">Try again</a>
      </Show>
    </AuthScreen>
  );
}
