import { For, Show, type JSX } from "solid-js";
import { Icon } from "./Icon";
import { cx, ui } from "./styles";

export type Step = {
  title: string;
  state: "done" | "current" | "upcoming";
  description?: JSX.Element;
  // Shown only for the current step.
  content?: JSX.Element;
};

const MARKER: Record<Step["state"], string> = {
  done: "border-console-success-border bg-console-success-bg text-console-success",
  current: "border-console-accent bg-console-accent text-white",
  upcoming: "border-console-border bg-console-surface text-console-subtle",
};

// Numbered setup steps (first Deployment, Slack connection, project onboarding).
export function Steps(props: { steps: readonly Step[]; label: string }) {
  return (
    <ol class="m-0 grid list-none gap-3 p-0" aria-label={props.label}>
      <For each={props.steps}>
        {(step, index) => (
          <li class="grid grid-cols-[22px_minmax(0,1fr)] gap-x-2.5" aria-current={step.state === "current" ? "step" : undefined}>
            <span class={cx("grid size-5.5 place-items-center rounded-xs border font-mono text-badge font-medium", MARKER[step.state])} aria-hidden="true">
              {step.state === "done" ? <Icon name="check" /> : index() + 1}
            </span>
            <div class="grid min-w-0 gap-1 pt-0.5">
              <span class={cx("text-body font-medium", step.state === "upcoming" ? "text-console-muted" : "text-console-text")}>
                {step.title}
                <span class="sr-only">{step.state === "done" ? " (done)" : step.state === "current" ? " (current step)" : ""}</span>
              </span>
              <Show when={step.description}>
                <div class={cx(ui.muted, "grid gap-1.5")}>{step.description}</div>
              </Show>
              <Show when={step.state === "current" && step.content}>
                <div class="mt-1">{step.content}</div>
              </Show>
            </div>
          </li>
        )}
      </For>
    </ol>
  );
}
