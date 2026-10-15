import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createSignal, For, Show } from "solid-js";
import { errorMessage } from "../lib/error";
import { createAdminRegion, listAdminRegions, updateAdminRegion, type AdminRegion } from "../lib/admin";
import { DataTable, EmptyCell, RowActions } from "../ui/DataTable";
import { FormDialog } from "../ui/Dialog";
import { Field, TextInput } from "../ui/Field";
import { IDText } from "../ui/IDText";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { ui } from "../ui/styles";

const COLUMNS = ["Region", "ID", "Location", { label: "Actions", srOnly: true }] as const;

// Create (no region) or edit one Region. The ID is fixed once created.
function RegionDialog(props: { region: AdminRegion | null; onClose: () => void; onSaved: () => Promise<void> }) {
  const [id, setID] = createSignal(props.region?.id ?? "");
  const [displayName, setDisplayName] = createSignal(props.region?.display_name ?? "");
  const [location, setLocation] = createSignal(props.region?.location ?? "");
  const [submitting, setSubmitting] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const submit = async () => {
    setSubmitting(true);
    setError(null);
    try {
      const input = { display_name: displayName().trim(), location: location().trim() };
      if (props.region) await updateAdminRegion(props.region.id, input);
      else await createAdminRegion({ id: id().trim(), ...input });
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
      title={props.region ? `Edit ${props.region.id}` : "New region"}
      submitLabel={props.region ? "Save" : "Create"}
      busyLabel={props.region ? "Saving..." : "Creating..."}
      busy={submitting()}
      disabled={!id().trim() || !displayName().trim()}
      error={error()}
      onSubmit={() => void submit()}
      onClose={props.onClose}
    >
      <Show when={!props.region}>
        <Field label="ID">
          <TextInput class="font-mono" value={id()} onInput={(event) => setID(event.currentTarget.value)} placeholder="us-east-1" autocomplete="off" spellcheck={false} required autofocus />
        </Field>
      </Show>
      <Field label="Display name">
        <TextInput value={displayName()} onInput={(event) => setDisplayName(event.currentTarget.value)} placeholder="US East" required autofocus={!!props.region} />
      </Field>
      <Field label="Location">
        <TextInput value={location()} onInput={(event) => setLocation(event.currentTarget.value)} placeholder="Virginia, USA" />
      </Field>
    </FormDialog>
  );
}

export function AdminRegions() {
  const queryClient = useQueryClient();
  const regions = createQuery(() => ({ queryKey: ["admin", "regions"], queryFn: listAdminRegions, retry: false }));
  const [dialog, setDialog] = createSignal<{ region: AdminRegion | null } | null>(null);

  const refresh = () => queryClient.invalidateQueries({ queryKey: ["admin", "regions"] });

  return (
    <Page>
      <PageHeader
        title="Regions"
        subtitle="Logical placement domains available to projects and worker groups."
        actions={<button type="button" class={ui.button} onClick={() => setDialog({ region: null })}>New region</button>}
      />
      <QueryState
        query={regions}
        pending={<TableSkeleton label="Loading regions" columns={COLUMNS} />}
        error={(error) => errorMessage(error, "Could not load regions.")}
        empty={{
          when: (data) => data.regions.length === 0,
          title: "No regions configured.",
          action: <button type="button" class={ui.button} onClick={() => setDialog({ region: null })}>Create region</button>,
        }}
      >
        {(data) => (
          <DataTable columns={COLUMNS}>
            <For each={data().regions}>
              {(region) => (
                <tr>
                  <td><strong class="font-medium text-console-text">{region.display_name}</strong></td>
                  <td><IDText value={region.id} /></td>
                  <td><Show when={region.location} fallback={<EmptyCell />}>{region.location}</Show></td>
                  <RowActions label={`Actions for ${region.display_name}`} items={[{ label: "Edit", onSelect: () => setDialog({ region }) }]} />
                </tr>
              )}
            </For>
          </DataTable>
        )}
      </QueryState>

      <Show when={dialog()}>
        {(current) => <RegionDialog region={current().region} onClose={() => setDialog(null)} onSaved={refresh} />}
      </Show>
    </Page>
  );
}
