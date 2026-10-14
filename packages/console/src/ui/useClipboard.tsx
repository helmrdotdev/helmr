import { createSignal, onCleanup, type Accessor, type JSX } from "solid-js";
import { createCopyFeedback, type CopyState } from "./clipboard";

export type { CopyState } from "./clipboard";

// Copies text and reports the result once: `state` drives the visible label and
// `LiveRegion` announces it to screen readers. Each copy control owns one.
export function useClipboard(announcements: { copied: string; failed: string } = { copied: "Copied", failed: "Could not copy" }): {
  state: Accessor<CopyState>;
  copy: (text: string) => Promise<CopyState>;
  LiveRegion: () => JSX.Element;
} {
  const [state, setState] = createSignal<CopyState>("idle");
  const feedback = createCopyFeedback({
    write: (text) => navigator.clipboard.writeText(text),
    onState: setState,
    schedule: (run, ms) => window.setTimeout(run, ms),
    cancel: (timer) => window.clearTimeout(timer as number),
  });
  onCleanup(feedback.dispose);
  const message = () => (state() === "idle" ? "" : announcements[state() as "copied" | "failed"]);
  return {
    state,
    copy: feedback.copy,
    LiveRegion: () => <span class="sr-only" aria-live="polite">{message()}</span>,
  };
}
