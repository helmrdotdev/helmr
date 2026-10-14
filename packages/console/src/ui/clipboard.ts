export type CopyState = "idle" | "copied" | "failed";

export const COPY_FEEDBACK_MS: Record<Exclude<CopyState, "idle">, number> = { copied: 1200, failed: 3000 };

// One copy action with a single feedback timer: each result replaces the previous
// one and returns to idle after its own duration.
export function createCopyFeedback(deps: {
  write: (text: string) => Promise<void>;
  onState: (state: CopyState) => void;
  schedule: (run: () => void, ms: number) => unknown;
  cancel: (timer: unknown) => void;
}): { copy: (text: string) => Promise<CopyState>; dispose: () => void } {
  let timer: unknown;
  const show = (state: Exclude<CopyState, "idle">) => {
    if (timer !== undefined) deps.cancel(timer);
    deps.onState(state);
    timer = deps.schedule(() => {
      timer = undefined;
      deps.onState("idle");
    }, COPY_FEEDBACK_MS[state]);
    return state;
  };
  return {
    async copy(text) {
      try {
        await deps.write(text);
        return show("copied");
      } catch {
        return show("failed");
      }
    },
    dispose() {
      if (timer !== undefined) deps.cancel(timer);
      timer = undefined;
    },
  };
}
