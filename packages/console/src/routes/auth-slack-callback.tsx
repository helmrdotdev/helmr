import { A, useNavigate, useSearchParams } from "@solidjs/router";
import { createSignal, onCleanup, onMount, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { describeFailure, type Failure } from "../lib/error";
import { finishSlackAuthorization } from "../lib/slack";
import { AuthCopy, AuthError, AuthWindow, AuthWorking } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

const MESSAGES = {
  bad_request: "The Slack authorization expired, was cancelled, or was started by a different account.",
  forbidden: "This Slack connection is no longer available, or your account can no longer manage it.",
  conflict: "This Slack connection changed while you were authorizing it.",
  service_unavailable: "Slack is not configured on this server.",
  unauthorized: "Your Helmr sign-in expired during Slack authorization.",
};

// What to do next, by what went wrong.
const NEXT_STEPS: Record<string, string> = {
  forbidden: "Ask a manager of this agent to check the agent’s Slack connection.",
  service_unavailable: "Ask an administrator to configure the Slack integration, then start authorization again.",
};
const DEFAULT_NEXT_STEP = "Sign in with the account and organization that started this connection, then start authorization again from the agent’s Slack connection.";

export function AuthSlackCallback() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const [failure, setFailure] = createSignal<(Failure & { nextStep: string }) | null>(null);
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
      if (!mounted) return;
      const code = e instanceof ApiError ? e.code : "";
      setFailure({ ...describeFailure(e, MESSAGES, "Slack authorization could not be completed."), nextStep: NEXT_STEPS[code] ?? DEFAULT_NEXT_STEP });
    }
  });
  return (
    <Show when={failure()} fallback={<AuthWorking>Connecting Slack…</AuthWorking>}>
      {(current) => (
        <AuthWindow>
          <AuthError
            title="Slack connection needs attention"
            message={current().message}
            details={current().details}
            action={<>
              <AuthCopy>{current().nextStep}</AuthCopy>
              <A class={ui.secondaryButton} href="/agents">Return to agents</A>
            </>}
          />
        </AuthWindow>
      )}
    </Show>
  );
}
