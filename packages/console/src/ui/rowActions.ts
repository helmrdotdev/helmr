import { createSignal } from "solid-js";

// Busy and error state for actions on individual table rows: one action runs at a
// time (a second run while one is pending is refused), and its error stays with
// the row it came from until the next action.
export function createRowActions<A extends string = "default">() {
  const [pending, setPending] = createSignal<{ id: string; action: A } | null>(null);
  const [failure, setFailure] = createSignal<{ id: string; message: string } | null>(null);
  return {
    busy: (id: string, action?: A): boolean => {
      const current = pending();
      return !!current && current.id === id && (action === undefined || current.action === action);
    },
    error: (id: string): string | null => {
      const current = failure();
      return current && current.id === id ? current.message : null;
    },
    async run(id: string, action: A, task: () => Promise<unknown>, toMessage: (error: unknown) => string): Promise<boolean> {
      if (pending()) return false;
      setFailure(null);
      setPending(() => ({ id, action }));
      try {
        await task();
        return true;
      } catch (error) {
        setFailure({ id, message: toMessage(error) });
        return false;
      } finally {
        setPending(null);
      }
    },
  };
}
