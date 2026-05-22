import { useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { errorMessage } from "../lib/error";
import { startGitHubSetup } from "../lib/github";
import { readPendingGitHubSetup, rememberGitHubSetup } from "../lib/github-setup";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

function readParam(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

export function GitHubSetup() {
  const [params] = useSearchParams();
  const [error, setError] = createSignal<string | null>(null);

  onMount(async () => {
    const installationID = readParam(params["installation_id"]);
    history.replaceState({}, "", "/github/setup");

    if (!installationID) {
      setError("Missing GitHub installation.");
      return;
    }

    try {
      const pending = readPendingGitHubSetup();
      const setupAction = pending?.kind ?? "settings";
      rememberGitHubSetup({ kind: setupAction, installation_id: installationID });
      const { redirect_url } = await startGitHubSetup({
        installation_id: installationID,
        setup_action: setupAction,
      });
      window.location.href = redirect_url;
    } catch (e) {
      const kind = e instanceof ApiError ? e.errorKind : null;
      setError(errorMessage(kind, e instanceof Error ? e.message : "GitHub setup failed."));
    }
  });

  return (
    <AuthScreen>
      <Show when={error()} fallback={<AuthCopy>Connecting GitHub...</AuthCopy>}>
        <AuthTitle>GitHub setup failed</AuthTitle>
        <p class={ui.error}>{error()}</p>
        <a href="/settings/github">Back to GitHub settings</a>
      </Show>
    </AuthScreen>
  );
}
