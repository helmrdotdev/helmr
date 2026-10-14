import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import {
  createAdminWorkerGroup,
  listAdminRegions,
  listAdminWorkerGroups,
  rotateAdminWorkerGroupToken,
  transitionAdminWorkerGroup,
  updateAdminWorkerGroup,
  type AdminWorkerGroup,
  type CreateAdminWorkerGroupInput,
} from "../lib/admin";
import { errorMessage } from "../lib/error";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { Dialog, RevealDialog } from "../ui/Dialog";
import { Field } from "../ui/Field";
import { Select } from "../ui/Select";
import { PageHeader } from "../ui/PageHeader";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";


export function AdminWorkerGroups() {
  const queryClient = useQueryClient();
  const [regionFilter, setRegionFilter] = createSignal("");
  const regions = createQuery(() => ({ queryKey: ["admin", "regions"], queryFn: listAdminRegions, retry: false }));
  const groups = createQuery(() => ({
    queryKey: ["admin", "worker-groups", regionFilter()],
    queryFn: () => listAdminWorkerGroups(regionFilter()),
    retry: false,
  }));
  const [creating, setCreating] = createSignal(false);
  const [editing, setEditing] = createSignal<AdminWorkerGroup | null>(null);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);
  const [enrollmentToken, setEnrollmentToken] = createSignal<string | null>(null);
  const [newRegionID, setNewRegionID] = createSignal("");

  const refresh = () => queryClient.invalidateQueries({ queryKey: ["admin", "worker-groups"] });

  async function submitCreate(event: SubmitEvent) {
    event.preventDefault();
    const form = new FormData(event.currentTarget as HTMLFormElement);
    const input: CreateAdminWorkerGroupInput = {
      region_id: String(form.get("region_id") ?? ""),
      name: String(form.get("name") ?? "").trim(),
      description: String(form.get("description") ?? "").trim(),
    };
    setSubmitting(true);
    setError(null);
    try {
      const created = await createAdminWorkerGroup(input);
      await refresh();
      setCreating(false);
      setEnrollmentToken(created.enrollment_token);
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setSubmitting(false);
    }
  }

  async function submitEdit(event: SubmitEvent) {
    event.preventDefault();
    const current = editing();
    if (!current) return;
    const form = new FormData(event.currentTarget as HTMLFormElement);
    setSubmitting(true);
    setError(null);
    try {
      await updateAdminWorkerGroup(current.id, String(form.get("description") ?? "").trim());
      await refresh();
      setEditing(null);
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setSubmitting(false);
    }
  }

  async function transition(group: AdminWorkerGroup, action: "pause" | "activate" | "drain" | "disable") {
    setSubmitting(true);
    setError(null);
    try {
      await transitionAdminWorkerGroup(group, action);
      await refresh();
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setSubmitting(false);
    }
  }

  async function rotateToken(group: AdminWorkerGroup) {
    setSubmitting(true);
    setError(null);
    try {
      const rotated = await rotateAdminWorkerGroupToken(group.id);
      setEnrollmentToken(rotated.enrollment_token);
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div class={ui.page}>
      <PageHeader
        title="Worker Groups"
        subtitle="Execution fleets, routing, and Worker enrollment."
        actions={<button type="button" class={ui.button} disabled={(regions.data?.regions.length ?? 0) === 0} onClick={() => { setError(null); setCreating(true); }}>New Worker Group</button>}
      />
      <div class={ui.toolbar}>
        <span class={ui.filterField}>
          Region
          <Select
            value={regionFilter()}
            options={[{ value: "", label: "All" }, ...(regions.data?.regions ?? []).map((region) => ({ value: region.id, label: region.display_name }))]}
            onChange={setRegionFilter}
            ariaLabel="Region"
            minWidth="160px"
          />
        </span>
      </div>
      <Show when={error()}>{(message) => <StatePanel error={message()} />}</Show>
      <Show when={!groups.isPending} fallback={<StatePanel loading="Loading Worker Groups..." />}>
        <Show when={!groups.isError} fallback={<StatePanel error="Could not load Worker Groups." />}>
          <Show
            when={(groups.data?.worker_groups.length ?? 0) > 0}
            fallback={
              <StatePanel empty="No Worker Groups configured.">
                <Show when={(regions.data?.regions.length ?? 0) > 0} fallback={<a href="/admin/regions" class="text-console-accent">Create a Region first</a>}>
                  <button type="button" class={ui.button} onClick={() => setCreating(true)}>Create Worker Group</button>
                </Show>
              </StatePanel>
            }
          >
            <DataTable columns={["Worker Group", "ID", "Region", "Status", "Version", { label: "Actions", srOnly: true }]} minWidth="min-w-220">
              <For each={groups.data?.worker_groups ?? []}>
                {(group) => (
                  <tr>
                    <td><strong class="font-medium text-console-text">{group.name}</strong></td>
                    <td><IDText value={group.id} /></td>
                    <td><code>{group.region_id}</code></td>
                    <td><StatusBadge resource="worker_group" status={group.status} /></td>
                    <td>{group.claim_version}</td>
                    <td class={ui.actionsCell}><div class="flex items-center justify-end gap-1.5">
                      <button type="button" class={ui.secondaryButton} onClick={() => { setError(null); setEditing(group); }}>Edit</button>
                      <Show when={group.status === "active"}><button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => void transition(group, "pause")}>Pause</button></Show>
                      <Show when={group.status === "paused"}><button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => void transition(group, "activate")}>Activate</button></Show>
                      <Show when={group.status === "active" || group.status === "paused"}><button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => void transition(group, "drain")}>Drain</button></Show>
                      <Show when={group.status === "draining"}><button type="button" class={ui.dangerOutlineButton} disabled={submitting()} onClick={() => void transition(group, "disable")}>Disable</button></Show>
                      <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => void rotateToken(group)}>Rotate token</button>
                    </div></td>
                  </tr>
                )}
              </For>
            </DataTable>
          </Show>
        </Show>
      </Show>

      <Show when={creating()}>
        <Dialog title="New Worker Group" onClose={() => setCreating(false)} closeDisabled={submitting()}>
          <form onSubmit={submitCreate}>
            <Field label="Region">
              <Select
                name="region_id"
                value={newRegionID() || (regions.data?.regions[0]?.id ?? "")}
                options={(regions.data?.regions ?? []).map((region) => ({ value: region.id, label: `${region.display_name} (${region.id})` }))}
                onChange={setNewRegionID}
                minWidth="100%"
              />
            </Field>
            <TextField name="name" label="Name" placeholder="default" required />
            <label class={ui.field}><span>Description</span><textarea class={ui.textarea} name="description" /></label>
            <Show when={error()}><p class={ui.fieldError} role="alert">{error()}</p></Show>
            <div class={ui.modalActions}><button type="button" class={ui.secondaryButton} onClick={() => setCreating(false)}>Cancel</button><button class={ui.button} disabled={submitting()}>{submitting() ? "Creating..." : "Create"}</button></div>
          </form>
        </Dialog>
      </Show>

      <Show when={editing()}>{(group) => <Dialog title={`Edit ${group().name}`} onClose={() => setEditing(null)} closeDisabled={submitting()}><form onSubmit={submitEdit}><label class={ui.field}><span>Description</span><textarea class={ui.textarea} name="description" autofocus>{group().description}</textarea></label><Show when={error()}><p class={ui.fieldError}>{error()}</p></Show><div class={ui.modalActions}><button type="button" class={ui.secondaryButton} onClick={() => setEditing(null)}>Cancel</button><button class={ui.button} disabled={submitting()}>Save</button></div></form></Dialog>}</Show>

      <Show when={enrollmentToken()}>{(token) => <RevealDialog title="Worker enrollment token" value={token()} warning="Copy this token now. It will not be shown again." onClose={() => setEnrollmentToken(null)} />}</Show>
    </div>
  );
}

function TextField(props: { name: string; label: string; placeholder?: string; required?: boolean }) {
  return <label class={ui.field}><span>{props.label}</span><input class={ui.input} name={props.name} placeholder={props.placeholder} required={props.required} /></label>;
}
