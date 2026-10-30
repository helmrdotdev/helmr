import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { request } from "../lib/api";
import { loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { updateMemberOwnership } from "../lib/members";
import { DataTable, RowActions, TableCount } from "../ui/DataTable";
import { ConfirmDialog } from "../ui/Dialog";
import { PageHeader } from "../ui/PageHeader";
import { Panel } from "../ui/Panel";
import { DeniedState, QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";
import { Tag } from "../ui/Tag";

type OwnerMember = { membership_id: string; display_name: string; is_owner: boolean };

const COLUMNS = ["Member", "Ownership", { label: "Actions", srOnly: true }] as const;

// Owners appoint or remove owners and can recover their own access
// administration. This page reads its own owner-only list, so an owner reaches it
// without any role, including after losing access administration.
export function Ownership() {
  const me = useMe();
  const cache = useQueryClient();
  const isOwner = () => me.data?.is_owner === true;
  const members = createQuery(() => ({ queryKey: ["ownership", "members"], queryFn: () => request<{ members: OwnerMember[] }>("/api/ownership/members"), enabled: isOwner() }));
  const [changing, setChanging] = createSignal<OwnerMember>();
  const [recovering, setRecovering] = createSignal(false);
  const refresh = async () => {
    await Promise.all(["ownership", "me", "projects", "members", "access"].map(key => cache.invalidateQueries({ queryKey: [key] })));
  };
  return <>
    <PageHeader title="Ownership" subtitle="Owners appoint or remove owners and can recover access administration. At least one owner remains." />
    <Show when={isOwner()} fallback={<Show when={!me.isPending}><DeniedState message="Only owners can manage ownership." /></Show>}>
      <div class={ui.sectionStack}>
        <QueryState query={members} pending={<TableSkeleton label="Loading members" columns={COLUMNS} />} error={cause => loadError(cause, "Could not load members.")}>
          {data => (
            <DataTable columns={COLUMNS} footer={<TableCount count={data().members.length} noun={{ one: "member", other: "members" }} />}>
              <For each={data().members}>
                {member => (
                  <tr>
                    <td><span class="inline-flex items-center gap-1.5">{member.display_name}<Show when={member.is_owner}><Tag title="Can appoint owners and recover access administration">owner</Tag></Show></span></td>
                    <td><Show when={member.is_owner} fallback={<span class={ui.muted}>Member</span>}>Owner</Show></td>
                    <RowActions label={`Actions for ${member.display_name}`} items={[member.is_owner ? { label: "Remove ownership", tone: "danger" as const, onSelect: () => setChanging(member) } : { label: "Make owner", onSelect: () => setChanging(member) }]} />
                  </tr>
                )}
              </For>
            </DataTable>
          )}
        </QueryState>
        <Panel title="Access administration" actions={<button type="button" class={ui.compactButton} onClick={() => setRecovering(true)}>Recover my access administration</button>}>
          <p class={ui.muted}>If nobody can manage roles any more, an owner can take the access administrator role back. It grants no workload access by itself and is recorded in audit events.</p>
        </Panel>
      </div>
    </Show>
    <Show when={changing()}>
      {member => (
        <ConfirmDialog
          title={member().is_owner ? "Remove ownership" : "Make owner"}
          confirmLabel={member().is_owner ? "Remove ownership" : "Make owner"}
          tone={member().is_owner ? "danger" : "default"}
          target={member().display_name}
          onClose={() => setChanging(undefined)}
          onConfirm={async () => { await updateMemberOwnership(member().membership_id, !member().is_owner, member().is_owner); await refresh(); }}
        >
          An owner can change other owners and recover access administration, including the ability to grant workload access. Roles stay as they are.
        </ConfirmDialog>
      )}
    </Show>
    <Show when={recovering()}>
      <ConfirmDialog title="Recover access administration" confirmLabel="Recover my access" onClose={() => setRecovering(false)} onConfirm={async () => { await request("/api/ownership/recover-access", { method: "POST" }); await refresh(); }}>
        Grants you the access administrator role across the organization, so you can manage roles and their assignments. It grants no workload access by itself, and it is audited.
      </ConfirmDialog>
    </Show>
  </>;
}
