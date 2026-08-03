import { useNavigate } from "@solidjs/router";

const INTERACTIVE_TARGETS = "a,button,input,textarea,select";

export function runHref(runID: string, projectID: string, environmentID: string): string {
  const params = new URLSearchParams({
    project_id: projectID,
    environment_id: environmentID,
  });
  return `/runs/${runID}?${params.toString()}`;
}

export function useRunRowNavigation(run: () => {
  id: string;
  project_id: string;
  environment_id: string;
}) {
  const navigate = useNavigate();
  const href = () => {
    const current = run();
    return runHref(current.id, current.project_id, current.environment_id);
  };
  return {
    role: "link" as const,
    tabIndex: 0,
    onClick: (event: MouseEvent) => {
      if (event.target instanceof Element && event.target.closest(INTERACTIVE_TARGETS)) return;
      navigate(href());
    },
    onKeyDown: (event: KeyboardEvent) => {
      if (event.target instanceof Element && event.target.closest(INTERACTIVE_TARGETS)) return;
      if (event.key !== "Enter" && event.key !== " ") return;
      event.preventDefault();
      navigate(href());
    },
  };
}
