import { useNavigate } from "@solidjs/router";

const INTERACTIVE_TARGETS = "a,button,input,textarea,select";

export function runHref(runID: string): string {
  return `/runs/${runID}`;
}

export function useRunRowNavigation(runID: () => string) {
  const navigate = useNavigate();
  const openRun = (event: MouseEvent) => {
    if (event.target instanceof Element && event.target.closest(INTERACTIVE_TARGETS)) return;
    navigate(runHref(runID()));
  };
  const openRunFromKeyboard = (event: KeyboardEvent) => {
    if (event.target instanceof Element && event.target.closest(INTERACTIVE_TARGETS)) return;
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    navigate(runHref(runID()));
  };

  return {
    role: "link" as const,
    tabIndex: 0,
    onClick: openRun,
    onKeyDown: openRunFromKeyboard,
  };
}
