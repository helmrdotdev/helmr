import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { getCurrentDeployment } from "../lib/deployments";
import {
  activateSchedule,
  createSchedule,
  deactivateSchedule,
  deleteSchedule,
  listSchedules,
  type Schedule,
} from "../lib/schedules";
import { useScope } from "../lib/scope";
import { ActionMenu } from "../ui/ActionMenu";
import { Modal } from "../ui/Modal";
import { statusBadgeClass, ui } from "../ui/styles";

const DEFAULT_PAYLOAD = "{}";

function scheduleErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load schedules.";
}

function parsePayload(value: string): { ok: true; value: unknown } | { ok: false; message: string } {
  const trimmed = value.trim();
  if (trimmed === "") return { ok: true, value: {} };
  try {
    return { ok: true, value: JSON.parse(trimmed) as unknown };
  } catch {
    return { ok: false, message: "Payload must be valid JSON." };
  }
}

function shortID(id: string): string {
  return id.slice(0, 8);
}

function scheduleStatusTone(schedule: Schedule): "active" | "expired" {
  return schedule.active ? "active" : "expired";
}

function scheduleStatusLabel(schedule: Schedule): string {
  return schedule.active ? "Active" : "Inactive";
}

function workspaceLabel(schedule: Schedule): string {
  const workspace = schedule.workspace;
  if (!workspace?.repository) return "-";
  const ref = workspace.sha || workspace.ref;
  const subpath = workspace.subpath ? `:${workspace.subpath}` : "";
  return ref ? `${workspace.repository}@${ref}${subpath}` : `${workspace.repository}${subpath}`;
}

function dateCell(value: string | undefined) {
  return value ? formatRelative(value) : <span class={"text-console-faint"}>—</span>;
}

function ScheduleModal(props: {
  projectID: string;
  environmentID: string;
  taskIDs: string[];
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const [taskID, setTaskID] = createSignal(props.taskIDs[0] ?? "");
  const [cron, setCron] = createSignal("0 * * * *");
  const [timezone, setTimezone] = createSignal("UTC");
  const [dedupKey, setDedupKey] = createSignal("");
  const [repository, setRepository] = createSignal("");
  const [ref, setRef] = createSignal("main");
  const [subpath, setSubpath] = createSignal("");
  const [payload, setPayload] = createSignal(DEFAULT_PAYLOAD);
  const [active, setActive] = createSignal(true);
  const [saving, setSaving] = createSignal(false);
  const [error, setError] = createSignal<string | null>(null);

  const save = async (event: Event) => {
    event.preventDefault();
    setError(null);

    const payloadResult = parsePayload(payload());
    if (!payloadResult.ok) {
      setError(payloadResult.message);
      return;
    }

    const trimmedTaskID = taskID().trim();
    const trimmedRepository = repository().trim();
    const trimmedCron = cron().trim();
    const trimmedRef = ref().trim();
    if (!trimmedTaskID || !trimmedRepository || !trimmedCron || !trimmedRef) {
      setError("Task, repository, ref, and cron are required.");
      return;
    }

    setSaving(true);
    try {
      await createSchedule({
        project_id: props.projectID,
        environment_id: props.environmentID,
        ...(dedupKey().trim() === "" ? {} : { dedup_key: dedupKey().trim() }),
        task_id: trimmedTaskID,
        cron: trimmedCron,
        timezone: timezone().trim() || "UTC",
        payload: payloadResult.value,
        workspace: {
          repository: trimmedRepository,
          ref: trimmedRef,
          subpath: subpath().trim(),
        },
        active: active(),
      });
      await props.onSaved();
      props.onClose();
    } catch (saveError) {
      setError(scheduleErrorMessage(saveError));
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal title="Create schedule" onClose={props.onClose} closeDisabled={saving()}>
      <form onSubmit={save}>
        <label class={ui.field}>
          <span>Task</span>
          <input
            class={ui.input}
            value={taskID()}
            list="schedule-task-options"
            autocomplete="off"
            autofocus
            onInput={(event) => setTaskID(event.currentTarget.value)}
          />
          <datalist id="schedule-task-options">
            <For each={props.taskIDs}>
              {(id) => <option value={id} />}
            </For>
          </datalist>
        </label>

        <div class={"grid grid-cols-2 gap-2 max-sm:grid-cols-1"}>
          <label class={ui.field}>
            <span>Cron</span>
            <input
              class={ui.input}
              value={cron()}
              autocomplete="off"
              placeholder="0 * * * *"
              onInput={(event) => setCron(event.currentTarget.value)}
            />
          </label>
          <label class={ui.field}>
            <span>Timezone</span>
            <input
              class={ui.input}
              value={timezone()}
              autocomplete="off"
              placeholder="UTC"
              onInput={(event) => setTimezone(event.currentTarget.value)}
            />
          </label>
        </div>

        <label class={ui.field}>
          <span>Dedup key</span>
          <input
            class={ui.input}
            value={dedupKey()}
            autocomplete="off"
            placeholder="optional"
            onInput={(event) => setDedupKey(event.currentTarget.value)}
          />
        </label>

        <label class={ui.field}>
          <span>Repository</span>
          <input
            class={ui.input}
            value={repository()}
            autocomplete="off"
            placeholder="owner/repo"
            onInput={(event) => setRepository(event.currentTarget.value)}
          />
        </label>

        <div class={"grid grid-cols-2 gap-2 max-sm:grid-cols-1"}>
          <label class={ui.field}>
            <span>Ref</span>
            <input
              class={ui.input}
              value={ref()}
              autocomplete="off"
              placeholder="main"
              onInput={(event) => setRef(event.currentTarget.value)}
            />
          </label>
          <label class={ui.field}>
            <span>Subpath</span>
            <input
              class={ui.input}
              value={subpath()}
              autocomplete="off"
              placeholder="optional"
              onInput={(event) => setSubpath(event.currentTarget.value)}
            />
          </label>
        </div>

        <label class={ui.field}>
          <span>Payload</span>
          <textarea
            class={`${ui.textarea} font-mono`}
            value={payload()}
            spellcheck={false}
            onInput={(event) => setPayload(event.currentTarget.value)}
          />
        </label>

        <label class={"mb-3 grid cursor-pointer grid-cols-[15px_1fr] gap-2 text-[12px] text-console-text"}>
          <input
            class={"mt-0.5 size-[15px] accent-console-accent"}
            type="checkbox"
            checked={active()}
            onChange={(event) => setActive(event.currentTarget.checked)}
          />
          <span>Create active</span>
        </label>

        <Show when={error()}>
          <p class={ui.error} role="alert">{error()}</p>
        </Show>
        <div class={ui.modalActions}>
          <button type="button" class={ui.secondaryButton} disabled={saving()} onClick={props.onClose}>
            Cancel
          </button>
          <button
            class={ui.button}
            type="submit"
            disabled={saving() || taskID().trim() === "" || repository().trim() === "" || ref().trim() === "" || cron().trim() === ""}
          >
            {saving() ? "Creating..." : "Create"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function ScheduleRow(props: {
  schedule: Schedule;
  action: { id: string; kind: "activate" | "deactivate" | "delete" } | null;
  error: string | null;
  onActivate: (schedule: Schedule) => void;
  onDeactivate: (schedule: Schedule) => void;
  onDelete: (schedule: Schedule) => void;
}) {
  const busy = (kind: "activate" | "deactivate" | "delete") =>
    props.action?.id === props.schedule.id && props.action.kind === kind;
  return (
    <tr class={ui.detailTableRow}>
      <td>
        <div class={ui.tableCellStack}>
          <strong>{props.schedule.task_id}</strong>
          <div><code>{props.schedule.dedup_key}</code></div>
        </div>
      </td>
      <td><span class={statusBadgeClass(scheduleStatusTone(props.schedule))}>{scheduleStatusLabel(props.schedule)}</span></td>
      <td><code>{props.schedule.cron}</code></td>
      <td><span class={ui.muted}>{props.schedule.timezone}</span></td>
      <td><span class={ui.muted}>{workspaceLabel(props.schedule)}</span></td>
      <td>{dateCell(props.schedule.next_scheduled_at)}</td>
      <td>{dateCell(props.schedule.next_due_at)}</td>
      <td>{dateCell(props.schedule.last_scheduled_at)}</td>
      <td><code>{shortID(props.schedule.id)}</code></td>
      <td class={ui.actionsCell}>
        <ActionMenu
          label={`Actions for ${props.schedule.task_id}`}
          items={[
            {
              label: "Activate",
              busyLabel: busy("activate") ? "Activating..." : undefined,
              disabled: props.schedule.active || !!props.action,
              onSelect: () => props.onActivate(props.schedule),
            },
            {
              label: "Deactivate",
              busyLabel: busy("deactivate") ? "Deactivating..." : undefined,
              disabled: !props.schedule.active || !!props.action,
              onSelect: () => props.onDeactivate(props.schedule),
            },
            {
              label: "Delete",
              busyLabel: busy("delete") ? "Deleting..." : undefined,
              disabled: !!props.action,
              tone: "danger",
              onSelect: () => props.onDelete(props.schedule),
            },
          ]}
        />
        <Show when={props.error}>
          <p class={ui.rowError} role="alert">{props.error}</p>
        </Show>
      </td>
    </tr>
  );
}

export function Schedules() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const [modalOpen, setModalOpen] = createSignal(false);
  const [action, setAction] = createSignal<{ id: string; kind: "activate" | "deactivate" | "delete" } | null>(null);
  const [actionError, setActionError] = createSignal<{ id: string; message: string } | null>(null);

  const schedules = createQuery(() => ({
    queryKey: ["schedules", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () =>
      listSchedules({
        projectID: scope.selectedProjectID(),
        environmentID: scope.selectedEnvironmentID(),
      }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));

  const deployment = createQuery(() => ({
    queryKey: ["deployments", "current", scope.selectedProjectID(), scope.selectedEnvironmentID()],
    queryFn: () =>
      getCurrentDeployment({
        projectID: scope.selectedProjectID(),
        environmentID: scope.selectedEnvironmentID(),
      }),
    enabled: !!scope.selectedProjectID() && !!scope.selectedEnvironmentID(),
    retry: false,
  }));

  const items = createMemo(() => schedules.data?.schedules ?? []);
  const activeCount = createMemo(() => items().filter((schedule) => schedule.active).length);
  const inactiveCount = createMemo(() => items().length - activeCount());
  const taskIDs = createMemo(() => deployment.data?.deployment?.tasks.map((task) => task.task_id) ?? []);
  const scopeIDs = () => ({
    projectID: scope.selectedProjectID(),
    environmentID: scope.selectedEnvironmentID(),
  });
  const invalidateSchedules = () => queryClient.invalidateQueries({ queryKey: ["schedules"] });

  const runAction = async (schedule: Schedule, kind: "activate" | "deactivate" | "delete") => {
    if (kind === "delete" && !window.confirm(`Delete schedule for "${schedule.task_id}"?`)) return;
    setActionError(null);
    setAction({ id: schedule.id, kind });
    try {
      if (kind === "activate") {
        await activateSchedule(schedule.id, scopeIDs());
      } else if (kind === "deactivate") {
        await deactivateSchedule(schedule.id, scopeIDs());
      } else {
        await deleteSchedule(schedule.id, scopeIDs());
      }
      await invalidateSchedules();
    } catch (error) {
      setActionError({ id: schedule.id, message: scheduleErrorMessage(error) });
    } finally {
      setAction(null);
    }
  };

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Schedules</h1>
          <p class={ui.pageSubtitle}>
            Cron schedules that create runs for deployed tasks in the selected environment.
          </p>
        </div>
        <button
          class={ui.button}
          type="button"
          disabled={!scope.selectedProjectID() || !scope.selectedEnvironmentID()}
          onClick={() => setModalOpen(true)}
        >
          Create schedule
        </button>
      </div>

      <div class={ui.metricStrip} aria-label="Schedule summary">
        <div class={ui.metricCard}>
          <span>Total</span>
          <strong>{items().length}</strong>
        </div>
        <div class={ui.metricCard}>
          <span>Active</span>
          <strong class="text-console-info">{activeCount()}</strong>
        </div>
        <div class={ui.metricCard}>
          <span>Inactive</span>
          <strong class="text-console-muted">{inactiveCount()}</strong>
        </div>
        <div class={ui.metricCard}>
          <span>Tasks</span>
          <strong>{taskIDs().length}</strong>
        </div>
      </div>

      <Show when={schedules.isError}>
        <p class={ui.error} role="alert">{scheduleErrorMessage(schedules.error)}</p>
      </Show>

      <Show when={!schedules.isError}>
        <Show when={!schedules.isPending} fallback={<p class={ui.muted}>Loading schedules...</p>}>
          <Show when={items().length > 0} fallback={<p class={ui.emptyState}>No schedules found.</p>}>
            <div class={ui.tableWrap}>
              <table class={"min-w-270"}>
                <thead>
                  <tr>
                    <th>Task</th>
                    <th>Status</th>
                    <th>Cron</th>
                    <th>Timezone</th>
                    <th>Workspace</th>
                    <th>Next</th>
                    <th>Due</th>
                    <th>Last</th>
                    <th>ID</th>
                    <th><span class="sr-only">Actions</span></th>
                  </tr>
                </thead>
                <tbody>
                  <For each={items()}>
                    {(schedule) => (
                      <ScheduleRow
                        schedule={schedule}
                        action={action()}
                        error={actionError()?.id === schedule.id ? actionError()?.message ?? null : null}
                        onActivate={(selected) => void runAction(selected, "activate")}
                        onDeactivate={(selected) => void runAction(selected, "deactivate")}
                        onDelete={(selected) => void runAction(selected, "delete")}
                      />
                    )}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
        </Show>
      </Show>

      <Show when={modalOpen()}>
        <ScheduleModal
          projectID={scope.selectedProjectID()}
          environmentID={scope.selectedEnvironmentID()}
          taskIDs={taskIDs()}
          onClose={() => setModalOpen(false)}
          onSaved={invalidateSchedules}
        />
      </Show>
    </section>
  );
}
