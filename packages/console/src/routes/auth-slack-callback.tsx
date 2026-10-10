import { A, useNavigate, useSearchParams } from "@solidjs/router";
import { createSignal, onCleanup, onMount, Show } from "solid-js";
import { finishSlackAuthorization } from "../lib/slack";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

export function AuthSlackCallback() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [error, setError] = createSignal<string | null>(null);
  let mounted = true;
  onCleanup(() => { mounted = false; });
  onMount(async () => {
    const param = (name: string) => {
      const value = params[name];
      return Array.isArray(value) ? value[0] ?? "" : value ?? "";
    };
    const input = { state: param("state"), code: param("code"), error: param("error") };
    history.replaceState({}, "", "/auth/slack/callback");
    try {
      const result = await finishSlackAuthorization(input);
      if (mounted) navigate(result.return_url || "/deployments", { replace: true });
    } catch (e) {
      if (mounted) setError(e instanceof Error ? e.message : "Slack authorization could not be completed.");
    }
  });
  return <AuthScreen>
    <Show when={error()} fallback={<AuthCopy>Connecting Slack...</AuthCopy>}>
      <AuthTitle>Slack connection needs attention</AuthTitle>
      <p class={ui.error}>{error()}</p>
      <AuthCopy>Sign in with the account and organization that started this connection, then start authorization again.</AuthCopy>
      <A href="/deployments">Return to Agents</A>
    </Show>
  </AuthScreen>;
}
