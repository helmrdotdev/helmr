import { A } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import {
  createAdminWorkerGroup,
  listAdminRegions,
  listAdminWorkerGroups,
  rotateAdminWorkerGroupToken,
  transitionAdminWorkerGroup,
  updateAdminWorkerGroup,
  type AdminRegion,
  type AdminWorkerGroup,
} from "../lib/admin";
import { errorMessage } from "../lib/error";
import { DataTable, RowActions, RowError } from "../ui/DataTable";
import type { ActionMenuItem } from "../ui/ActionMenu";
import { FormDialog, RevealDialog } from "../ui/Dialog";
import { Field, TextArea, TextInput } from "../ui/Field";
import { IDText } from "../ui/IDText";
import { Page, Toolbar } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { createRowActions } from "../ui/rowActions";
import { Select } from "../ui/Select";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";

type Transition = "pause" | "activate" | "drain" | "disable";

function CreateWorkerGroupDialog(props: { regions: AdminRegion[]; onClose: () => void; onCreated: (enrollmentToken: string) => Promise<void> }) {
  const [regionID, setRegionID] = createSignal(props.regions[0]?.id ?? "");
  const [name, setName] = createSignal("");
  const [description, setDescription] = createSignal("");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const created = await createAdminWorkerGroup({ region_id: regionID(), name: name().trim(), description: description().trim() });
      await props.onCreated(created.enrollment_token);
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <FormDialog
      title="New Worker Group"
      submitLabel="Create"
      busyLabel="Creating..."
      busy={submitting()}
      disabled={!regionID() || !name().trim()}
      error={error()}
      onSubmit={() => void submit()}
      onClose={props.onClose}
    >
      <Field label="Region">
        <Select
          value={regionID()}
          options={props.regions.map((region) => ({ value: region.id, label: `${region.display_name} (${region.id})` }))}
          onChange={setRegionID}
          minWidth="100%"
        />
      </Field>
      <Field label="Name">
        <TextInput value={name()} onInput={(event) => setName(event.currentTarget.value)} placeholder="default" required autofocus />
      </Field>
      <Field label="Description">
        <TextArea value={description()} onInput={(event) => setDescription(event.currentTarget.value)} />
      </Field>
    </FormDialog>
  );
}

function EditWorkerGroupDialog(props: { group: AdminWorkerGroup; onClose: () => void; onSaved: () => Promise<void> }) {
  const [description, setDescription] = createSignal(props.group.description);
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async () => {
    setSubmitting(true);
    setError(null);
    try {
      await updateAdminWorkerGroup(props.group.id, description().trim());
      await props.onSaved();
      props.onClose();
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <FormDialog
      title={`Edit ${props.group.name}`}
      submitLabel="Save"
      busyLabel="Saving..."
      busy={submitting()}
      error={error()}
      onSubmit={() => void submit()}
      onClose={props.onClose}
    >
      <Field label="Description">
        <TextArea value={description()} onInput={(event) => setDescription(event.currentTarget.value)} autofocus />
      </Field>
    </FormDialog>
  );
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
  const regionList = createMemo(() => regions.data?.regions ?? []);
  const [creating, setCreating] = createSignal(false);
  const [editing, setEditing] = createSignal<AdminWorkerGroup | null>(null);
  const [enrollmentToken, setEnrollmentToken] = createSignal<string | null>(null);
  const rows = createRowActions<Transition | "rotate">();

  const refresh = () => queryClient.invalidateQueries({ queryKey: ["admin", "worker-groups"] });

  const transition = (group: AdminWorkerGroup, action: Transition) =>
    void rows.run(group.id, action, async () => {
      await transitionAdminWorkerGroup(group, action);
      await refresh();
    }, (cause) => errorMessage(cause));

  const rotateToken = (group: AdminWorkerGroup) =>
    void rows.run(group.id, "rotate", async () => {
      const rotated = await rotateAdminWorkerGroupToken(group.id);
      setEnrollmentToken(rotated.enrollment_token);
    }, (cause) => errorMessage(cause));

  const actions = (group: AdminWorkerGroup): ActionMenuItem[] => {
    const busy = rows.busy(group.id);
    const items: ActionMenuItem[] = [{ label: "Edit", disabled: busy, onSelect: () => setEditing(group) }];
    if (group.status === "active") items.push({ label: "Pause", disabled: busy, onSelect: () => transition(group, "pause") });
    if (group.status === "paused") items.push({ label: "Activate", disabled: busy, onSelect: () => transition(group, "activate") });
    if (group.status === "active" || group.status === "paused") items.push({ label: "Drain", disabled: busy, onSelect: () => transition(group, "drain") });
    items.push({ label: "Rotate token", disabled: busy, onSelect: () => rotateToken(group) });
    if (group.status === "draining") items.push({ label: "Disable", disabled: busy, tone: "danger", onSelect: () => transition(group, "disable") });
    return items;
  };

  return (
    <Page>
      <PageHeader
        title="Worker Groups"
        subtitle="Execution fleets, routing, and Worker enrollment."
        actions={<button type="button" class={ui.button} disabled={regionList().length === 0} onClick={() => setCreating(true)}>New Worker Group</button>}
      />
      <Toolbar
        start={
          <Select
            value={regionFilter()}
            options={[{ value: "", label: "All Regions" }, ...regionList().map((region) => ({ value: region.id, label: region.display_name }))]}
            onChange={setRegionFilter}
            ariaLabel="Filter by Region"
            minWidth="160px"
          />
        }
      />
      <QueryState
        query={groups}
        pending="Loading Worker Groups..."
        error={(error) => errorMessage(error, "Could not load Worker Groups.")}
        empty={{
          when: (data) => data.worker_groups.length === 0,
          title: "No Worker Groups configured.",
          action: (
            <Show when={regionList().length > 0} fallback={<A href="/admin/regions" class={ui.secondaryButton}>Create a Region first</A>}>
              <button type="button" class={ui.button} onClick={() => setCreating(true)}>Create Worker Group</button>
            </Show>
          ),
        }}
      >
        {(data) => (
          <DataTable columns={["Worker Group", "ID", "Region", "Status", "Version", { label: "Actions", srOnly: true }]} minWidth="min-w-220">
            <For each={data().worker_groups}>
              {(group) => (
                <>
                  <tr>
                    <td><strong class="font-medium text-console-text">{group.name}</strong></td>
                    <td><IDText value={group.id} /></td>
                    <td><code>{group.region_id}</code></td>
                    <td><StatusBadge resource="worker_group" status={group.status} /></td>
                    <td>{group.claim_version}</td>
                    <RowActions label={`Actions for ${group.name}`} items={actions(group)} />
                  </tr>
                  <RowError columns={6} message={rows.error(group.id)} />
                </>
              )}
            </For>
          </DataTable>
        )}
      </QueryState>

      <Show when={creating()}>
        <CreateWorkerGroupDialog
          regions={regionList()}
          onClose={() => setCreating(false)}
          onCreated={async (token) => {
            setCreating(false);
            setEnrollmentToken(token);
            await refresh();
          }}
        />
      </Show>

      <Show when={editing()}>{(group) => <EditWorkerGroupDialog group={group()} onClose={() => setEditing(null)} onSaved={refresh} />}</Show>

      <Show when={enrollmentToken()}>{(token) => <RevealDialog title="Worker enrollment token" value={token()} warning="Copy this token now. It will not be shown again." onClose={() => setEnrollmentToken(null)} />}</Show>
    </Page>
  );
}
