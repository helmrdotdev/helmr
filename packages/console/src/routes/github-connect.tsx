import { useNavigate } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { createMemo } from "solid-js";
import { listGitHubInstallations } from "../lib/github";
import { rememberGitHubSetup } from "../lib/github-setup";
import { useScope } from "../lib/scope";
import { AuthCopy, AuthScreen, AuthTitle } from "../ui/AuthScreen";
import { ui } from "../ui/styles";

export function GitHubConnect() {
  const navigate = useNavigate();
  const scope = useScope();
  const installations = createQuery(() => ({
    queryKey: ["github-installations"],
    queryFn: listGitHubInstallations,
    retry: false,
  }));
  const hasActiveInstallation = createMemo(() =>
    (installations.data?.installations ?? []).some((installation) => installation.status === "active"),
  );

  const connect = () => {
    const installURL = installations.data?.install_url;
    const projectID = scope.selectedProjectID();
    if (!installURL || !projectID) return;
    rememberGitHubSetup({ kind: "onboarding", project_id: projectID });
    window.location.href = installURL;
  };

  return (
    <AuthScreen>
      <AuthTitle>Connect GitHub</AuthTitle>
      <AuthCopy>
        Helmr uses a GitHub App installation to mount repository code for runs.
      </AuthCopy>
      {installations.isPending ? (
        <p class={ui.muted}>Loading...</p>
      ) : (
        <div class={ui.authActions}>
          <button
            type="button"
            class={ui.button}
            disabled={installations.isError || !installations.data?.install_url || !scope.selectedProjectID()}
            onClick={connect}
          >
            Install GitHub App
          </button>
          <button
            type="button"
            class={ui.secondaryButton}
            disabled={!hasActiveInstallation()}
            onClick={() => navigate("/github/connect/repositories")}
          >
            Choose repositories
          </button>
        </div>
      )}
    </AuthScreen>
  );
}
