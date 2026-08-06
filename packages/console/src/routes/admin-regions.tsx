import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { ApiError } from "../lib/api";
import { createAdminRegion, listAdminRegions, updateAdminRegion, type AdminRegion } from "../lib/admin";
import { Modal } from "../ui/Modal";
import { ui } from "../ui/styles";

function errorMessage(error: unknown): string {
  return error instanceof ApiError ? error.message : "Something went wrong.";
}

export function AdminRegions() {
  const queryClient = useQueryClient();
  const regions = createQuery(() => ({ queryKey: ["admin", "regions"], queryFn: listAdminRegions, retry: false }));
  const [creating, setCreating] = createSignal(false);
  const [editing, setEditing] = createSignal<AdminRegion | null>(null);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const refresh = () => queryClient.invalidateQueries({ queryKey: ["admin", "regions"] });

  async function submitCreate(event: SubmitEvent) {
    event.preventDefault();
    const form = new FormData(event.currentTarget as HTMLFormElement);
    setSubmitting(true);
    setError(null);
    try {
      await createAdminRegion({
        id: String(form.get("id") ?? "").trim(),
        provider: String(form.get("provider") ?? "").trim(),
        provider_region: String(form.get("provider_region") ?? "").trim(),
        display_name: String(form.get("display_name") ?? "").trim(),
        location: String(form.get("location") ?? "").trim(),
      });
      await refresh();
      setCreating(false);
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
      await updateAdminRegion(current.id, {
        display_name: String(form.get("display_name") ?? "").trim(),
        location: String(form.get("location") ?? "").trim(),
      });
      await refresh();
      setEditing(null);
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div class={ui.page}>
      <div class={ui.pageHeader}>
        <div><h1 class={ui.h1}>Regions</h1><p class={ui.pageSubtitle}>Placement and provider metadata available to projects and Worker Groups.</p></div>
        <button type="button" class={ui.button} onClick={() => { setError(null); setCreating(true); }}>New Region</button>
      </div>
      <Show when={!regions.isPending} fallback={<p class={ui.muted}>Loading Regions...</p>}>
        <Show when={!regions.isError} fallback={<p class={ui.error}>Could not load Regions.</p>}>
          <Show when={(regions.data?.regions.length ?? 0) > 0} fallback={<div class={ui.emptyState}><strong class="text-console-text">No Regions configured.</strong><button type="button" class={ui.button} onClick={() => setCreating(true)}>Create Region</button></div>}>
            <div class={ui.tableWrap}>
              <table class={ui.dataTable}>
                <thead><tr><th>Region</th><th>Provider</th><th>Location</th><th>State</th><th></th></tr></thead>
                <tbody><For each={regions.data?.regions ?? []}>{(region) => (
                  <tr>
                    <td><div class={ui.tableCellStack}><strong>{region.display_name}</strong><div><code>{region.id}</code></div></div></td>
                    <td>{region.provider} / <code>{region.provider_region}</code></td>
                    <td>{region.location || "—"}</td><td>{region.state}</td>
                    <td class={ui.actionsCell}><button type="button" class={ui.secondaryButton} onClick={() => { setError(null); setEditing(region); }}>Edit</button></td>
                  </tr>
                )}</For></tbody>
              </table>
            </div>
          </Show>
        </Show>
      </Show>

      <Show when={creating()}>
        <Modal title="New Region" onClose={() => setCreating(false)} closeDisabled={submitting()}>
          <form onSubmit={submitCreate}>
            <RegionField name="id" label="ID" placeholder="us-east-1" autofocus />
            <RegionField name="display_name" label="Display name" placeholder="US East" />
            <RegionField name="provider" label="Provider" placeholder="aws" />
            <RegionField name="provider_region" label="Provider Region" placeholder="us-east-1" />
            <RegionField name="location" label="Location" placeholder="Virginia, USA" />
            <Show when={error()}><p class={ui.fieldError} role="alert">{error()}</p></Show>
            <div class={ui.modalActions}><button type="button" class={ui.secondaryButton} onClick={() => setCreating(false)}>Cancel</button><button class={ui.button} disabled={submitting()}>{submitting() ? "Creating..." : "Create"}</button></div>
          </form>
        </Modal>
      </Show>

      <Show when={editing()}>{(region) => (
        <Modal title={`Edit ${region().id}`} onClose={() => setEditing(null)} closeDisabled={submitting()}>
          <form onSubmit={submitEdit}>
            <RegionField name="display_name" label="Display name" value={region().display_name} autofocus />
            <RegionField name="location" label="Location" value={region().location} />
            <Show when={error()}><p class={ui.fieldError} role="alert">{error()}</p></Show>
            <div class={ui.modalActions}><button type="button" class={ui.secondaryButton} onClick={() => setEditing(null)}>Cancel</button><button class={ui.button} disabled={submitting()}>{submitting() ? "Saving..." : "Save"}</button></div>
          </form>
        </Modal>
      )}</Show>
    </div>
  );
}

function RegionField(props: { name: string; label: string; value?: string; placeholder?: string; autofocus?: boolean }) {
  return <label class={ui.field}><span>{props.label}</span><input class={ui.input} name={props.name} value={props.value ?? ""} placeholder={props.placeholder} autofocus={props.autofocus} required={props.name !== "location"} /></label>;
}
