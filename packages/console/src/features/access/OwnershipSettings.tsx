import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { request } from "../../lib/api";
import { errorMessage } from "../../lib/error";
import { updateMemberOwnership } from "../../lib/members";
import { ConfirmDialog } from "../../ui/Dialog";
import { List, ListItem } from "../../ui/List";
import { Notice } from "../../ui/Notice";
import { Panel } from "../../ui/Panel";
import { ui } from "../../ui/styles";
import { Tag } from "../../ui/Tag";

type OwnerMember = { membership_id: string; display_name: string; is_owner: boolean };

// Owners appoint or remove owners and can recover their own access administration.
export function OwnershipSettings() {
  const cache = useQueryClient();
  const members = createQuery(() => ({ queryKey: ["ownership", "members"], queryFn: () => request<{ members: OwnerMember[] }>("/api/ownership/members") }));
  const [changing, setChanging] = createSignal<OwnerMember>();
  const [recovering, setRecovering] = createSignal(false);
  const refresh = async () => {
    await Promise.all(["ownership", "me", "projects", "members", "access"].map(key => cache.invalidateQueries({ queryKey: [key] })));
  };
  return (
    <Panel title="Organization ownership" actions={<button type="button" class={ui.compactButton} onClick={() => setRecovering(true)}>Recover my access administration</button>}>
      <p class={ui.muted}>Owners appoint or remove owners and can recover their own access administration. At least one owner must remain.</p>
      <Show when={members.error}><Notice tone="danger">{errorMessage(members.error)}</Notice></Show>
      <List label="Members">
        <For each={members.data?.members}>
          {member => (
            <ListItem line>
              <span class="flex min-w-0 flex-1 items-center gap-1.5">
                <span class="truncate">{member.display_name}</span>
                <Show when={member.is_owner}><Tag>owner</Tag></Show>
              </span>
              <button type="button" class={ui.compactButton} onClick={() => setChanging(member)}>{member.is_owner ? "Remove ownership" : "Make owner"}</button>
            </ListItem>
          )}
        </For>
      </List>
      <Show when={changing()}>
        {member => (
          <ConfirmDialog
            title={member().is_owner ? "Remove ownership" : "Appoint owner"}
            confirmLabel={member().is_owner ? "Remove ownership" : "Make owner"}
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
    </Panel>
  );
}
