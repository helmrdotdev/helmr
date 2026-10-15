import { returnPath } from "../lib/continuation";
import { useNavigate, useLocation } from "@solidjs/router";
import { useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Show } from "solid-js";
import { createProjectDraft } from "../features/projects/forms";
import { createSignOut, onboardingSteps } from "../features/projects/onboarding";
import { errorDetails, errorMessage } from "../lib/error";
import { ME_QUERY_KEY, useMe } from "../lib/me";
import { rememberProjectScope } from "../lib/scope";
import { AuthError, AuthTitle, AuthWindow } from "../ui/AuthWindow";
import { FormActions } from "../ui/Field";
import { Steps } from "../ui/Steps";
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
    <Show when={!me.isPending && !draft.loading()}>
      <AuthWindow step={firstProject() ? "Step 2 of 2" : undefined}>
        <AuthTitle>{firstProject() ? "Create your first project" : "New project"}</AuthTitle>
        <Show when={firstProject()}>
          <div class={ui.blockStack}>
            <Steps label="Setup steps" steps={onboardingSteps("project")} />
          </div>
        </Show>
        <form class="mt-3.5" onSubmit={submit}>
          <draft.Fields />
          <Show when={error() ?? signOut.error()}>{(failure) => <AuthError message={failure().message} details={failure().details} />}</Show>
          <FormActions
            submitLabel="Create"
            busyLabel="Creating..."
            busy={submitting()}
            disabled={signOut.busy() || !draft.valid()}
            cancelLabel={firstProject() ? signOut.label() : "Cancel"}
            cancelDisabled={signOut.busy()}
            onCancel={() => (firstProject() ? void signOut.run() : navigate(destination()))}
          />
        </form>
      </AuthWindow>
    </Show>
  );
}
