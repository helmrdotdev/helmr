import { createMemo, createSignal, For } from "solid-js";
import type { SessionTurn } from "../../lib/sessions";
import { cx } from "../../ui/styles";

export const turnStatusColor: Record<SessionTurn["status"], string> = {
  completed: "bg-console-success-border",
  failed: "bg-console-danger-text",
  running: "bg-console-accent",
  queued: "bg-console-faint",
  interrupted: "bg-console-warning-border",
  cancelled: "bg-console-neutral-border",
};

function weight(turn: SessionTurn): number {
  if (!turn.started_at) return 1;
  const seconds = ((turn.terminal_at ? Date.parse(turn.terminal_at) : Date.now()) - Date.parse(turn.started_at)) / 1000;
  return 1 + Math.min(7, Math.log2(1 + Math.max(0, seconds) / 30));
}

// Every Turn of the Session as one bar: width by duration, colour by status.
// Bars shrink to fit, so many Turns never widen the page.
// One tab stop; arrow keys move between Turns.
export function TurnTimeline(props: { turns: readonly SessionTurn[]; onSelect: (turn: SessionTurn) => void }) {
  // Bars render by Turn id so a poll that re-reads a Turn keeps its button.
  const byID = createMemo(() => new Map(props.turns.map(turn => [turn.id, turn])));
  const ids = createMemo(() => props.turns.map(turn => turn.id));
  const [current, setCurrent] = createSignal(0);
  const active = () => Math.min(current(), props.turns.length - 1);
  const move = (event: KeyboardEvent) => {
    const step = { ArrowRight: 1, ArrowLeft: -1, Home: -Infinity, End: Infinity }[event.key];
    if (step === undefined) return;
    event.preventDefault();
    const next = Math.max(0, Math.min(props.turns.length - 1, active() + step));
    setCurrent(next);
    (event.currentTarget as HTMLElement).querySelectorAll<HTMLButtonElement>("button")[next]?.focus();
  };
  return (
    <div class="flex items-center gap-3 border-b border-console-border-soft px-3 py-2">
      <span class="w-12 shrink-0 font-mono text-micro text-console-subtle">Turns</span>
      <div class="flex h-3.5 min-w-0 flex-1 items-stretch gap-px" role="list" aria-label="Turn timeline" onKeyDown={move}>
        <For each={ids()}>
          {(id, index) => {
            const turn = () => byID().get(id)!;
            return (
            <div role="listitem" class="flex min-w-0" style={{ flex: `${weight(turn())} 1 0` }}>
              <button
                type="button"
                tabindex={index() === active() ? 0 : -1}
                title={`Turn #${turn().sequence}, ${turn().status}`}
                aria-label={`Turn #${turn().sequence}, ${turn().status}`}
                class={cx("w-full cursor-pointer border-0 p-0 hover:opacity-70", turnStatusColor[turn().status])}
                onFocus={() => setCurrent(index())}
                onClick={() => props.onSelect(turn())}
              />
            </div>
            );
          }}
        </For>
      </div>
      <span class="shrink-0 font-mono text-micro text-console-subtle">{props.turns.length}</span>
    </div>
  );
}
