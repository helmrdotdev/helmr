import { useNavigate } from "@solidjs/router";

const INTERACTIVE_TARGETS = "a,button,input,textarea,select";

export function runHref(runID: string, sessionID: string, projectID: string, environmentID: string): string {
  const params = new URLSearchParams({ project_id: projectID, environment_id: environmentID });
  return `/sessions/${sessionID}/runs/${runID}?${params.toString()}`;
}

export function useRunRowNavigation(run: () => { id: string; session_id: string; project_id: string; environment_id: string }) {
  const navigate = useNavigate();
  const openRun = (event: MouseEvent) => {
    if (event.target instanceof Element && event.target.closest(INTERACTIVE_TARGETS)) return;
    const current = run();
    navigate(runHref(current.id, current.session_id, current.project_id, current.environment_id));
  };
  const openRunFromKeyboard = (event: KeyboardEvent) => {
    if (event.target instanceof Element && event.target.closest(INTERACTIVE_TARGETS)) return;
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    const current = run();
    navigate(runHref(current.id, current.session_id, current.project_id, current.environment_id));
  };

  return {
    role: "link" as const,
    tabIndex: 0,
    onClick: openRun,
    onKeyDown: openRunFromKeyboard,
  };
}
