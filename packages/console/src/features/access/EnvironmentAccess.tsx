import { usePermission } from "../../lib/me";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, Show } from "solid-js";
import { loadError } from "../../lib/error";
import { listAccessTargets, listGrants, listRoles } from "../../lib/permissions";
import { useScope } from "../../lib/scope";
import { Dialog } from "../../ui/Dialog";
import { Toolbar } from "../../ui/Page";
import { QueryState } from "../../ui/QueryState";
import { TableSkeleton } from "../../ui/Skeleton";
import { ui } from "../../ui/styles";
import { AssignmentsTable } from "./AssignmentsTable";
import { GrantAccessDialog } from "./GrantAccessDialog";

const COLUMNS = ["Recipient", "Role", "Agents", "Permissions"];

// Every assignment in one environment of the tab's project.
export function EnvironmentAccess(props: { environment: string; onClose: () => void }) {
  const scope = useScope();
  const permission = usePermission();
  const project = () => scope.selectedProjectID();
  const targets = createQuery(() => ({ queryKey: ["access", "targets", project()], queryFn: () => listAccessTargets(project()) }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants }));
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles }));
  const environment = () => targets.data?.targets.find(t => t.environment === props.environment);
  const assignments = createMemo(() => (grants.data?.grants ?? []).filter(g => g.environment_id === environment()?.environment_id));
  const [granting, setGranting] = createSignal(false);
  return <>
    <Dialog title={`Access to ${environment()?.name ?? props.environment}`} onClose={props.onClose} class="max-w-220">
      <Toolbar end={<button type="button" class={ui.button} disabled={!environment() || !permission.has("assignments.create")} onClick={() => setGranting(true)}>Grant access</button>} />
      <QueryState query={targets} pending={<TableSkeleton label="Loading access" columns={COLUMNS} />} error={error => loadError(error, "Could not load this environment.")}>
        {() => (
          <QueryState query={grants} pending={<TableSkeleton label="Loading access" columns={COLUMNS} />} error={error => loadError(error, "Could not load access.")}>
            {() => <AssignmentsTable grants={assignments()} catalog={roles.data?.catalog ?? []} recipient empty="Nobody has access to this environment" emptyHint="Creating an environment grants no access. Grant a role to start deploying and running agents." />}
          </QueryState>
        )}
      </QueryState>
    </Dialog>
    <Show when={granting()}>
      <GrantAccessDialog environment={{ project: project(), environment: props.environment }} roleID={roles.data?.roles.find(r => r.builtin_key === "environment_resource_administrator")?.id} onClose={() => setGranting(false)} />
    </Show>
  </>;
}
