import { returnPath } from "../lib/continuation";
import { useNavigate, useLocation } from "@solidjs/router";
import { useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Show } from "solid-js";
import { createProjectDraft } from "../features/projects/forms";
import { createSignOut } from "../features/projects/onboarding";
import { errorDetails, errorMessage } from "../lib/error";
import { ME_QUERY_KEY, useMe } from "../lib/me";
import { rememberProjectScope } from "../lib/scope";
import { setupStep } from "../lib/setup";
import { AuthAccount, AuthCopy, AuthError, AuthLink, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { ui } from "../ui/styles";

export function ProjectNew() {
  const navigate = useNavigate();
  const location = useLocation();
  const queryClient = useQueryClient();
  const me = useMe();
  const draft = createProjectDraft();
  const signOut = createSignOut();
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<{ message: string; details: string | undefined } | null>(null);
  const firstProject = createMemo(() => me.data?.project_required === true);
  const destination = () => returnPath(new URLSearchParams(location.search).get("next"));
  const setup = () => new URLSearchParams(location.search).get("setup") === "1" && firstProject();

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    setSubmitting(true);
    setError(null);
    try {
      const wasFirstProject = firstProject();
      const project = await draft.submit();
      rememberProjectScope(project);
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      await queryClient.invalidateQueries({ queryKey: ME_QUERY_KEY });
      navigate(destination(), { replace: wasFirstProject });
    } catch (e) {
      setError({ message: errorMessage(e, "The project could not be created. Try again."), details: errorDetails(e) });
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <Show when={!me.isPending}>
      <AuthWindow
        step={setup() ? setupStep(4) : firstProject() ? "Step 2 of 2" : undefined}
        account={firstProject() ? <AuthAccount name={me.data?.display_name} signOutLabel={signOut.label()} signingOut={signOut.busy()} onSignOut={() => void signOut.run()} /> : undefined}
      >
        <AuthTitle>{firstProject() ? "Create your first project" : "New project"}</AuthTitle>
        <AuthCopy>Projects hold the environments your agents deploy to.</AuthCopy>
        <form onSubmit={submit}>
          <draft.Fields />
          <Show when={signOut.error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
          <Show when={error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
          <button type="submit" class={ui.button} disabled={submitting() || signOut.busy() || !draft.valid()}>
            {submitting() ? "Creating…" : "Create project"}
          </button>
          <Show when={!firstProject()}>
            <AuthLink onClick={() => navigate(destination())}>Cancel</AuthLink>
          </Show>
        </form>
      </AuthWindow>
    </Show>
  );
}
