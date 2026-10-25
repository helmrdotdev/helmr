import { usePermission } from "../../lib/me";
import { createQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, Show } from "solid-js";
import { type ApiKeySummary } from "../../lib/api-keys";
import { loadError } from "../../lib/error";
import { listGrants, listRoles } from "../../lib/permissions";
import { Dialog } from "../../ui/Dialog";
import { Notice } from "../../ui/Notice";
import { Toolbar } from "../../ui/Page";
import { QueryState } from "../../ui/QueryState";
import { TableSkeleton } from "../../ui/Skeleton";
import { ui } from "../../ui/styles";
import { AssignmentsTable } from "./AssignmentsTable";
import { GrantAccessDialog } from "./GrantAccessDialog";

// An API key's role assignments, all in its own environment. Keys never get
// access from groups or all members.
export function APIKeyAccess(props: { keyItem: ApiKeySummary; project: string; environment: string; onClose: () => void }) {
  const permission = usePermission();
  const roles = createQuery(() => ({ queryKey: ["access", "roles"], queryFn: listRoles }));
  const grants = createQuery(() => ({ queryKey: ["access", "grants"], queryFn: listGrants }));
  const assignments = createMemo(() => grants.data?.grants.filter(g => g.api_key_id === props.keyItem.id) ?? []);
  const [granting, setGranting] = createSignal(false);
  const active = () => props.keyItem.status === "active";
  return <>
    <Dialog title={`Access for ${props.keyItem.name}`} onClose={props.onClose} class="max-w-200">
      <Show when={!active()}><Notice tone="info" class="mb-3">This key is {props.keyItem.status} and can't be used. You can still remove its roles.</Notice></Show>
      <Toolbar end={<Show when={active() && permission.has("assignments.create")}><button type="button" class={ui.button} onClick={() => setGranting(true)}>Grant access</button></Show>} />
      <QueryState query={grants} pending={<TableSkeleton label="Loading access" columns={["Role", "Where", "Agents", "Permissions"]} />} error={error => loadError(error, "Could not load access.")}>
        {() => <AssignmentsTable grants={assignments()} catalog={roles.data?.catalog ?? []} editable={active()} empty="No roles yet" emptyHint="This key can't do anything until it has a role. Everyone holding it acts as the same identity." />}
      </QueryState>
    </Dialog>
    <Show when={granting()}>
      <GrantAccessDialog recipient={{ kind: "api_key", id: props.keyItem.id, name: props.keyItem.name }} environment={{ project: props.project, environment: props.environment }} onClose={() => setGranting(false)} />
    </Show>
  </>;
}
