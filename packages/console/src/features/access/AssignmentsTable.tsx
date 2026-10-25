import { usePermission } from "../../lib/me";
import { A } from "@solidjs/router";
import { useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { ALL_MEMBERS, grantAgents, grantRecipientName, grantWhere, groupHref, memberHref, permissionNames, removeGrant, roleHref, type PermissionDefinition, type RoleGrant } from "../../lib/permissions";
import { DataTable, EmptyCell, LoadMore, RowActions } from "../../ui/DataTable";
import { ConfirmDialog } from "../../ui/Dialog";
import { EmptyState } from "../../ui/QueryState";
import { ui } from "../../ui/styles";
import { AccessLabel } from "./AccessIcon";
import { GrantAccessDialog } from "./GrantAccessDialog";
import { GrantRemovalImpact } from "./GrantRemovalImpact";

function recipientHref(grant: RoleGrant): string | undefined {
  if (grant.subject_kind === "member" && grant.member_id) return memberHref(grant.member_id);
  if (grant.subject_kind === "group" && grant.group_id) return groupHref(grant.group_id);
  if (grant.subject_kind === "all_members") return groupHref(ALL_MEMBERS);
  return undefined;
}

// Role assignments, one per row, with their targets. Each page leaves out the
// column it is about (a group's page has no recipient column, a role's page no
// role column). Rows edit their agent targets or remove the assignment.
export function AssignmentsTable(props: {
  grants: readonly RoleGrant[];
  catalog: readonly PermissionDefinition[];
  recipient?: boolean;
  role?: boolean;
  empty: string;
  emptyHint?: string;
  editable?: boolean;
}) {
  const cache = useQueryClient();
  const permission = usePermission();
  const [editing, setEditing] = createSignal<RoleGrant>();
  const [removing, setRemoving] = createSignal<RoleGrant>();
  const columns = () => [
    ...(props.recipient ? ["Recipient"] : []),
    ...(props.role !== false ? ["Role"] : []),
    "Where", "Agents", "Permissions",
    { label: "Actions", srOnly: true },
  ];
  return (
    <>
      <Show when={props.grants.length > 0} fallback={<EmptyState title={props.empty} hint={props.emptyHint} />}>
        <DataTable columns={columns()} minWidth="min-w-180" footer={<LoadMore count={props.grants.length} noun={{ one: "assignment", other: "assignments" }} hasMore={false} loading={false} onLoadMore={() => undefined} />}>
          <For each={props.grants}>
            {grant => (
              <tr>
                <Show when={props.recipient}>
                  <td>
                    <Show when={recipientHref(grant)} fallback={<AccessLabel kind={grant.subject_kind}>{grantRecipientName(grant)}</AccessLabel>}>
                      {href => <A href={href()} class="text-console-accent hover:text-console-accent-hover"><AccessLabel kind={grant.subject_kind}>{grantRecipientName(grant)}</AccessLabel></A>}
                    </Show>
                  </td>
                </Show>
                <Show when={props.role !== false}>
                  <td><A href={roleHref(grant.role_id)} class={ui.monoLink}>{grant.role_name}</A></td>
                </Show>
                <td class={grant.target_kind === "organization" ? "text-console-muted!" : "font-mono text-chrome!"}>{grantWhere(grant)}</td>
                <td><Show when={grantAgents(grant)} fallback={<EmptyCell />}>{agents => <span class="font-mono text-chrome">{agents()}</span>}</Show></td>
                <td>{permissionNames(grant.permissions, props.catalog)}</td>
                <RowActions
                  label={`Actions for ${props.recipient ? `${grantRecipientName(grant)}, ` : ""}${grant.role_name} in ${grantWhere(grant)}`}
                  items={[
                    ...(props.editable !== false && permission.has("assignments.update") && grant.agents_mode !== "none" ? [{ label: "Edit agents", onSelect: () => setEditing(grant) }] : []),
                    ...(permission.has("assignments.delete") ? [{ label: "Remove", tone: "danger" as const, onSelect: () => setRemoving(grant) }] : []),
                  ]}
                />
              </tr>
            )}
          </For>
        </DataTable>
      </Show>
      <Show when={editing()} keyed>{grant => <GrantAccessDialog grant={grant} onClose={() => setEditing(undefined)} />}</Show>
      <Show when={removing()}>
        {grant => (
          <ConfirmDialog
            title="Remove access"
            confirmLabel="Remove"
            busyLabel="Removing…"
            tone="danger"
            target={<>{grantRecipientName(grant())} · {grant().role_name} · {grantWhere(grant())}</>}
            onClose={() => setRemoving(undefined)}
            onConfirm={async () => {
              await removeGrant(grant().id, grant().revision);
              await Promise.all([cache.invalidateQueries({ queryKey: ["access"] }), cache.invalidateQueries({ queryKey: ["me"] }), cache.invalidateQueries({ queryKey: ["api-keys"] })]);
            }}
          >
            Access from other assignments stays. Work already running is not stopped.
            <GrantRemovalImpact grant={grant()} />
          </ConfirmDialog>
        )}
      </Show>
    </>
  );
}
