import { useSearchParams } from "@solidjs/router";
import { createSignal, onMount, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { errorMessage } from "../lib/error";
import { startGitHubSetup } from "../lib/github";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

const GITHUB_SETUP_STORAGE_KEY = "helmr.github_setup";

function readParam(value: string | string[] | undefined): string | undefined {
  return Array.isArray(value) ? value[0] : value;
}

function rememberGitHubSetup(installationID: string) {
  try {
    const existing: unknown = JSON.parse(sessionStorage.getItem(GITHUB_SETUP_STORAGE_KEY) ?? "{}");
    const previous = existing && typeof existing === "object" && !Array.isArray(existing) ? existing : {};
    sessionStorage.setItem(GITHUB_SETUP_STORAGE_KEY, JSON.stringify({
      ...previous,
      installation_id: installationID,
      created_at: Date.now(),
    }));
  } catch {
    // Setup can still finish without the post-install prompt.
  }
}

export function GitHubSetup() {
  const [params] = useSearchParams();
  const [error, setError] = createSignal<string | null>(null);

  onMount(async () => {
    const installationID = readParam(params["installation_id"]);
    const setupAction = readParam(params["setup_action"]);
    history.replaceState({}, "", "/github/setup");

    if (!installationID) {
      setError("Missing GitHub installation.");
      return;
    }

    try {
      rememberGitHubSetup(installationID);
      const { redirect_url } = await startGitHubSetup({
        installation_id: installationID,
        ...(setupAction ? { setup_action: setupAction } : {}),
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
