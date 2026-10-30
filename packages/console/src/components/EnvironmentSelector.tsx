import { createEffect, createMemo, createSignal, on, Show, type JSX } from "solid-js";
import type { Environment } from "../lib/projects";
import { useScope } from "../lib/scope";
import { environmentChip } from "../features/projects/display";
import { EnvDot } from "../ui/EnvChip";
import { Tag } from "../ui/Tag";
import { Toolbar } from "../ui/Page";
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
export function WorkingTag() {
  return <Tag title="The environment you are working in">working</Tag>;
}

// An Environment-specific settings page: a toolbar choosing the Environment being
// edited, then its content. The choice is page state; when it differs from the
// tab's Environment the toolbar says so and offers the way back.
export function EnvironmentSelectorLayout(props: { edited: EditedEnvironment; tools?: JSX.Element; children: JSX.Element }) {
  const scope = useScope();
  const options = createMemo(() =>
    props.edited.environments().map((environment) => ({
      value: environment.environment,
      label: environment.name,
      leading: () => <EnvDot colorHex={environmentChip(environment)?.colorHex} />,
      trailing: () => <Show when={environment.environment === scope.selectedEnvironmentID()}><WorkingTag /></Show>,
    })),
  );
  const working = () => scope.selectedEnvironment();
  const elsewhere = () => !!props.edited.environmentID() && props.edited.environmentID() !== scope.selectedEnvironmentID();
  let toolbar: HTMLDivElement | undefined;
  // The way back disappears once used, so focus returns to the Environment choice.
  const back = (environmentID: string) => {
    props.edited.select(environmentID);
    toolbar?.querySelector<HTMLElement>("[role=combobox]")?.focus();
  };

  return (
    <div class="min-w-0" ref={toolbar}>
      <Toolbar
        start={<>
          <Select
            value={props.edited.environmentID()}
            options={options()}
            onChange={props.edited.select}
            ariaLabel="Environment"
            minWidth="220px"
            disabled={options().length === 0}
          />
          {props.tools}
          <Show when={elsewhere() && working()}>
            {(current) => (
              <span class="text-chrome text-console-muted">
                You are working in {current().name}.{" "}
                <button type="button" class={cx(ui.monoLink, "border-0 bg-transparent p-0")} onClick={() => back(current().environment)}>Back to {current().name}</button>
              </span>
            )}
          </Show>
        </>}
      />
      {props.children}
    </div>
  );
}
