import { useNavigate } from "@solidjs/router";

const INTERACTIVE_TARGETS = "a,button,input,textarea,select";

export function sessionHref(sessionID: string, projectID: string, environmentID: string): string {
  const params = new URLSearchParams({ project_id: projectID, environment_id: environmentID });
  return `/sessions/${sessionID}?${params.toString()}`;
}

export function useSessionRowNavigation(session: () => { id: string; project_id: string; environment_id: string }) {
  const navigate = useNavigate();
  const openSession = (event: MouseEvent) => {
    if (event.target instanceof Element && event.target.closest(INTERACTIVE_TARGETS)) return;
    const current = session();
    navigate(sessionHref(current.id, current.project_id, current.environment_id));
  };
  const openSessionFromKeyboard = (event: KeyboardEvent) => {
    if (event.target instanceof Element && event.target.closest(INTERACTIVE_TARGETS)) return;
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    const current = session();
    navigate(sessionHref(current.id, current.project_id, current.environment_id));
  };

  return {
    role: "link" as const,
    tabIndex: 0,
    onClick: openSession,
    onKeyDown: openSessionFromKeyboard,
  };
}
