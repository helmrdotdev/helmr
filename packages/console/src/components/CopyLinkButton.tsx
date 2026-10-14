import { createSignal, onCleanup } from "solid-js";
import { useScopeIDs } from "../lib/scope";
import { scopeShareURL } from "../lib/scope-link";
import { ui } from "../ui/styles";

type CopyState = "idle" | "copied" | "failed";

const LABEL: Record<CopyState, string> = { idle: "Copy link", copied: "Copied", failed: "Could not copy" };
const ANNOUNCEMENT: Record<CopyState, string> = { idle: "", copied: "Link copied", failed: "Could not copy link" };

// Copies a link to the current page that opens it in this tab's Project and Environment.
export function CopyLinkButton() {
  const scope = useScopeIDs();
  const [state, setState] = createSignal<CopyState>("idle");
  let timer: number | undefined;
  onCleanup(() => window.clearTimeout(timer));

  const show = (next: CopyState, duration: number) => {
    setState(next);
    window.clearTimeout(timer);
    timer = window.setTimeout(() => setState("idle"), duration);
  };

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(scopeShareURL(window.location.href, scope()));
      show("copied", 1200);
    } catch {
      show("failed", 3000);
    }
  };

  return (
    <>
      <button
        type="button"
        class={ui.secondaryButton}
        disabled={!scope().projectID || !scope().environmentID}
        onClick={() => void copy()}
      >
        {LABEL[state()]}
      </button>
      <span class="sr-only" aria-live="polite">{ANNOUNCEMENT[state()]}</span>
    </>
  );
}
