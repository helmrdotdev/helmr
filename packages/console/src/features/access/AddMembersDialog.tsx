import { useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { errorMessage } from "../../lib/error";
import type { OrganizationMember } from "../../lib/members";
import { setGroupMember } from "../../lib/permissions";
import { FormDialog } from "../../ui/Dialog";
import { ui } from "../../ui/styles";

const memberName = (m: OrganizationMember) => m.display_name || m.email || m.membership_id;

// Adds chosen active members to a group.
export function AddMembersDialog(props: { groupID: string; candidates: OrganizationMember[]; onClose: () => void }) {
  const cache = useQueryClient();
  const [chosen, setChosen] = createSignal<string[]>([]);
  const [busy, setBusy] = createSignal(false); const [error, setError] = createSignal<string | null>(null);
  return (
    <FormDialog
      title="Add members"
      submitLabel="Add members"
      busyLabel="Adding…"
      busy={busy()}
      disabled={chosen().length === 0}
      error={error()}
      onClose={props.onClose}
      onSubmit={async () => {
        setBusy(true); setError(null);
        try {
          for (const member of chosen()) await setGroupMember(props.groupID, member, true);
          await Promise.all([cache.invalidateQueries({ queryKey: ["access"] }), cache.invalidateQueries({ queryKey: ["me"] })]);
          props.onClose();
        } catch (cause) { setError(errorMessage(cause)); await cache.invalidateQueries({ queryKey: ["access"] }); } finally { setBusy(false); }
      }}
    >
      <Show when={props.candidates.length > 0} fallback={<p class={ui.muted}>Every active member is already in this group.</p>}>
        <div class="grid max-h-72 gap-1.5 overflow-y-auto">
          <For each={props.candidates}>
            {member => (
              <label class="flex cursor-pointer items-center gap-2 text-control text-console-text">
                <input type="checkbox" class="accent-console-accent" checked={chosen().includes(member.membership_id)} disabled={busy()} onChange={e => setChosen(ids => e.currentTarget.checked ? [...ids, member.membership_id] : ids.filter(v => v !== member.membership_id))} />
                <span>{memberName(member)}</span>
                <Show when={member.email && member.email !== memberName(member)}><span class="text-console-muted">{member.email}</span></Show>
              </label>
            )}
          </For>
        </div>
      </Show>
    </FormDialog>
  );
}
