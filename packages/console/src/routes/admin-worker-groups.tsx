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
import { ApiError } from "../lib/api";
import { Modal } from "../ui/Modal";
import { ui } from "../ui/styles";

function errorMessage(error: unknown): string {
  return error instanceof ApiError ? error.message : "Something went wrong.";
}

function numberValue(form: FormData, name: string): number {
  return Number(String(form.get(name) ?? "0"));
}

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

  const refresh = () => queryClient.invalidateQueries({ queryKey: ["admin", "worker-groups"] });

  async function submitCreate(event: SubmitEvent) {
    event.preventDefault();
    const form = new FormData(event.currentTarget as HTMLFormElement);
    const input: CreateAdminWorkerGroupInput = {
      region_id: String(form.get("region_id") ?? ""),
      name: String(form.get("name") ?? "").trim(),
      description: String(form.get("description") ?? "").trim(),
      allows_run: form.has("allows_run"),
      allows_build: form.has("allows_build"),
      required_cpu_millis: numberValue(form, "required_cpu_millis"),
      required_memory_bytes: numberValue(form, "required_memory_bytes"),
      required_guest_ephemeral_disk_bytes: numberValue(form, "required_guest_ephemeral_disk_bytes"),
      required_build_cache_bytes: numberValue(form, "required_build_cache_bytes"),
      required_artifact_cache_bytes: numberValue(form, "required_artifact_cache_bytes"),
      required_vm_slots: numberValue(form, "required_vm_slots"),
      observation_ttl_seconds: numberValue(form, "observation_ttl_seconds"),
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
      <div class={ui.pageHeader}>
        <div><h1 class={ui.h1}>Worker Groups</h1><p class={ui.pageSubtitle}>Execution fleets, capacity contracts, and Worker enrollment.</p></div>
        <button type="button" class={ui.button} disabled={(regions.data?.regions.length ?? 0) === 0} onClick={() => { setError(null); setCreating(true); }}>New Worker Group</button>
      </div>
      <div class={ui.toolbar}>
        <label class={ui.filterField}>Region <select class={ui.input} value={regionFilter()} onChange={(event) => setRegionFilter(event.currentTarget.value)}><option value="">All</option><For each={regions.data?.regions ?? []}>{(region) => <option value={region.id}>{region.display_name}</option>}</For></select></label>
      </div>
      <Show when={error()}><p class={ui.error} role="alert">{error()}</p></Show>
      <Show when={!groups.isPending} fallback={<p class={ui.muted}>Loading Worker Groups...</p>}>
        <Show when={!groups.isError} fallback={<p class={ui.error}>Could not load Worker Groups.</p>}>
          <Show when={(groups.data?.worker_groups.length ?? 0) > 0} fallback={<div class={ui.emptyState}><strong class="text-console-text">No Worker Groups configured.</strong><Show when={(regions.data?.regions.length ?? 0) > 0} fallback={<a href="/admin/regions" class="text-console-accent">Create a Region first</a>}><button type="button" class={ui.button} onClick={() => setCreating(true)}>Create Worker Group</button></Show></div>}>
            <div class={ui.tableWrap}>
              <table class="min-w-270">
                <thead><tr><th>Worker Group</th><th>Region</th><th>Roles</th><th>State</th><th>Version</th><th>Capacity contract</th><th></th></tr></thead>
                <tbody><For each={groups.data?.worker_groups ?? []}>{(group) => (
                  <tr>
                    <td><div class={ui.tableCellStack}><strong>{group.name}</strong><div><code>{group.id}</code></div></div></td>
                    <td><code>{group.region_id}</code></td>
                    <td>{[group.allows_run && "run", group.allows_build && "build"].filter(Boolean).join(", ")}</td>
                    <td>{group.state}</td><td>{group.claim_version}</td>
                    <td><code>{group.required_cpu_millis}m / {Math.round(group.required_memory_bytes / 1048576)} MiB</code></td>
                    <td class={ui.actionsCell}><div class="flex items-center justify-end gap-1.5">
                      <button type="button" class={ui.secondaryButton} onClick={() => { setError(null); setEditing(group); }}>Edit</button>
                      <Show when={group.state === "active"}><button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => void transition(group, "pause")}>Pause</button></Show>
                      <Show when={group.state === "paused"}><button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => void transition(group, "activate")}>Activate</button></Show>
                      <Show when={group.state === "active" || group.state === "paused"}><button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => void transition(group, "drain")}>Drain</button></Show>
                      <Show when={group.state === "draining"}><button type="button" class={ui.dangerOutlineButton} disabled={submitting()} onClick={() => void transition(group, "disable")}>Disable</button></Show>
                      <button type="button" class={ui.secondaryButton} disabled={submitting()} onClick={() => void rotateToken(group)}>Rotate token</button>
                    </div></td>
                  </tr>
                )}</For></tbody>
              </table>
            </div>
          </Show>
        </Show>
      </Show>

      <Show when={creating()}>
        <Modal title="New Worker Group" onClose={() => setCreating(false)} closeDisabled={submitting()}>
          <form onSubmit={submitCreate}>
            <label class={ui.field}><span>Region</span><select name="region_id" class={ui.input} required autofocus><For each={regions.data?.regions ?? []}>{(region) => <option value={region.id}>{region.display_name} ({region.id})</option>}</For></select></label>
            <TextField name="name" label="Name" placeholder="default" required />
            <label class={ui.field}><span>Description</span><textarea class={ui.textarea} name="description" /></label>
            <fieldset class={ui.fieldSet}><legend class={ui.fieldLegend}>Roles</legend><label class="mr-4"><input type="checkbox" name="allows_run" checked /> Run</label><label><input type="checkbox" name="allows_build" checked /> Build</label></fieldset>
            <div class="grid grid-cols-2 gap-x-3 max-sm:grid-cols-1">
              <NumberField name="required_cpu_millis" label="CPU (millicores)" value={1000} min={1} />
              <NumberField name="required_memory_bytes" label="Memory (bytes)" value={1073741824} min={1} />
              <NumberField name="required_guest_ephemeral_disk_bytes" label="Guest disk (bytes)" value={34359738368} min={1} />
              <NumberField name="required_build_cache_bytes" label="Build cache (bytes)" value={0} min={0} />
              <NumberField name="required_artifact_cache_bytes" label="Artifact cache (bytes)" value={0} min={0} />
              <NumberField name="required_vm_slots" label="VM slots" value={1} min={0} />
              <NumberField name="observation_ttl_seconds" label="Observation TTL (seconds)" value={120} min={1} />
            </div>
            <Show when={error()}><p class={ui.fieldError} role="alert">{error()}</p></Show>
            <div class={ui.modalActions}><button type="button" class={ui.secondaryButton} onClick={() => setCreating(false)}>Cancel</button><button class={ui.button} disabled={submitting()}>{submitting() ? "Creating..." : "Create"}</button></div>
          </form>
        </Modal>
      </Show>

      <Show when={editing()}>{(group) => <Modal title={`Edit ${group().name}`} onClose={() => setEditing(null)} closeDisabled={submitting()}><form onSubmit={submitEdit}><label class={ui.field}><span>Description</span><textarea class={ui.textarea} name="description" autofocus>{group().description}</textarea></label><Show when={error()}><p class={ui.fieldError}>{error()}</p></Show><div class={ui.modalActions}><button type="button" class={ui.secondaryButton} onClick={() => setEditing(null)}>Cancel</button><button class={ui.button} disabled={submitting()}>Save</button></div></form></Modal>}</Show>

      <Show when={enrollmentToken()}>{(token) => <Modal title="Worker enrollment token" onClose={() => setEnrollmentToken(null)}><p class={ui.warning}>Copy this token now. It will not be shown again.</p><code class={ui.rawKey}>{token()}</code><div class={ui.modalActions}><button type="button" class={ui.button} onClick={() => setEnrollmentToken(null)}>Done</button></div></Modal>}</Show>
    </div>
  );
}

function TextField(props: { name: string; label: string; placeholder?: string; required?: boolean }) {
  return <label class={ui.field}><span>{props.label}</span><input class={ui.input} name={props.name} placeholder={props.placeholder} required={props.required} /></label>;
}

function NumberField(props: { name: string; label: string; value: number; min: number }) {
  return <label class={ui.field}><span>{props.label}</span><input class={ui.input} type="number" name={props.name} value={props.value} min={props.min} step="1" required /></label>;
}
