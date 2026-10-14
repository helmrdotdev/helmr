import { createEffect, createMemo, createSignal, createUniqueId, For, on, Show, type JSX } from "solid-js";
import { environmentChip } from "../features/projects/display";
import type { Environment } from "../lib/projects";
import { useScope } from "../lib/scope";
import { EnvChip, ProtectedBadge } from "../ui/EnvChip";
import { Panel } from "../ui/Panel";
import { Select } from "../ui/Select";
import { cx, ui } from "../ui/styles";

// The Project and Environment an action writes to, captured when its dialog opens
// so the dialog keeps naming and writing to it whatever the page selects later.
export type EditTarget = { projectID: string; projectName: string; environment: Environment };

export type EditedEnvironment = {
  environments: () => Environment[];
  environment: () => Environment | undefined;
  projectID: () => string;
  environmentID: () => string;
  select: (environmentID: string) => void;
  target: () => EditTarget | undefined;
};

// The Environment an Environment-specific settings page edits. It opens on the
// tab's Environment and is page state only: choosing another one here never
// changes the tab's scope, and a change of the tab's scope starts over from it.
export function useEditedEnvironment(): EditedEnvironment {
  const scope = useScope();
  const [chosenID, setChosenID] = createSignal<string>();
  createEffect(on([scope.selectedProjectID, scope.selectedEnvironmentID], () => setChosenID(undefined), { defer: true }));
  const environments = () => scope.selectedProject()?.environments ?? [];
  const environment = createMemo(() =>
    environments().find((candidate) => candidate.environment === chosenID()) ?? scope.selectedEnvironment(),
  );
  return {
    environments,
    environment,
    projectID: scope.selectedProjectID,
    environmentID: () => environment()?.environment ?? "",
    select: setChosenID,
    target: () => {
      const project = scope.selectedProject();
      const current = environment();
      return project && current ? { projectID: project.project, projectName: project.name, environment: current } : undefined;
    },
  };
}

// Marks the Environment the tab is working in.
function WorkingTag() {
  return <span class="font-mono text-micro text-console-subtle">working</span>;
}

// An Environment-specific settings page: its content, and a rail listing the
// Project's Environments to choose the one being edited. On narrow screens the
// rail becomes a Select above the content.
export function EnvironmentSelectorLayout(props: { edited: EditedEnvironment; children: JSX.Element }) {
  const scope = useScope();
  const name = createUniqueId();
  const options = createMemo(() =>
    props.edited.environments().map((environment) => ({
      value: environment.environment,
      label: environment.name,
      trailing: () => <>
        <Show when={environmentChip(environment)?.protected}><ProtectedBadge /></Show>
        <Show when={environment.environment === scope.selectedEnvironmentID()}><WorkingTag /></Show>
      </>,
    })),
  );

  return (
    <div class="grid grid-cols-[minmax(0,1fr)_280px] items-start gap-6 max-[960px]:grid-cols-1">
      <div class="min-w-0">
        <div class="mb-3 hidden max-[960px]:block">
          <Select
            value={props.edited.environmentID()}
            options={options()}
            onChange={props.edited.select}
            ariaLabel="Environment"
            minWidth="200px"
            disabled={options().length === 0}
          />
        </div>
        {props.children}
      </div>
      <Panel title="Environment" class="sticky top-13.5 max-[960px]:hidden">
        <fieldset class="m-0 grid gap-0.5 border-0 p-0">
          <legend class="sr-only">Environment</legend>
          <For each={props.edited.environments()}>
            {(environment) => {
              const chip = () => environmentChip(environment)!;
              const selected = () => environment.environment === props.edited.environmentID();
              return (
                <label class={cx(ui.scopeItem, selected() && ui.scopeItemSelected)}>
                  <input
                    type="radio"
                    class="m-0 size-3 accent-console-accent"
                    name={name}
                    value={environment.environment}
                    checked={selected()}
                    onChange={(event) => { if (event.currentTarget.checked) props.edited.select(environment.environment); }}
                  />
                  <EnvChip name={chip().name} colorHex={chip().colorHex} protected={chip().protected} />
                  <Show when={environment.environment === scope.selectedEnvironmentID()} fallback={<span />}>
                    <WorkingTag />
                  </Show>
                </label>
              );
            }}
          </For>
        </fieldset>
        <p class="m-0 text-badge leading-normal text-console-muted">Opens on the environment you are working in. Switching here does not change it.</p>
      </Panel>
    </div>
  );
}
