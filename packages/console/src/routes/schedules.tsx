import { createQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { ApiError } from "../lib/api";
import { listSchedules, type Schedule } from "../lib/schedules";
import { useScope } from "../lib/scope";
import { statusBadgeClass, ui } from "../ui/styles";

function scheduleErrorMessage(error: unknown): string {
  if (error instanceof ApiError) return error.message;
  return "Could not load schedules.";
}

function statusTone(schedule: Schedule): "active" | "expired" | "revoked" {
  if (schedule.status === "active") return "active";
  if (schedule.status === "errored") return "revoked";
  return "expired";
}

function statusLabel(status: Schedule["status"]): string {
  switch (status) {
    case "active":
      return "Active";
    case "errored":
      return "Errored";
    case "archived":
      return "Archived";
  }
}

function dateCell(value: string | undefined) {
  return value ? formatRelative(value) : <span class="text-console-faint">—</span>;
}

export function Schedules() {
  const scope = useScope();
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
  const items = createMemo(() => schedules.data?.schedules ?? []);
  const activeCount = createMemo(() =>
    items().filter((schedule) => schedule.status === "active").length
  );
  const archivedCount = createMemo(() =>
    items().filter((schedule) => schedule.status === "archived").length
  );
  const issueCount = createMemo(() =>
    items().filter((schedule) => schedule.status === "errored").length
  );

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Schedules</h1>
          <p class={ui.pageSubtitle}>
            Source-declared Task schedules in the selected environment.
          </p>
        </div>
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
          <span>Archived</span>
          <strong class="text-console-muted">{archivedCount()}</strong>
        </div>
        <div class={ui.metricCard}>
          <span>Errored</span>
          <strong class="text-console-danger">{issueCount()}</strong>
        </div>
      </div>

      <Show when={schedules.isError}>
        <p class={ui.error} role="alert">{scheduleErrorMessage(schedules.error)}</p>
      </Show>

      <Show when={!schedules.isError}>
        <Show when={!schedules.isPending} fallback={<p class={ui.muted}>Loading schedules...</p>}>
          <Show when={items().length > 0} fallback={<p class={ui.emptyState}>No schedules found.</p>}>
            <div class={ui.tableWrap}>
              <table class="min-w-250">
                <thead>
                  <tr>
                    <th>Task</th>
                    <th>Status</th>
                    <th>Cron</th>
                    <th>Timezone</th>
                    <th>Next</th>
                    <th>Last</th>
                    <th>Generation</th>
                    <th>ID</th>
                  </tr>
                </thead>
                <tbody>
                  <For each={items()}>
                    {(schedule) => (
                      <tr class={ui.detailTableRow}>
                        <td><strong>{schedule.task_id}</strong></td>
                        <td>
                          <div class={ui.tableCellStack}>
                            <span class={statusBadgeClass(statusTone(schedule))}>
                              {statusLabel(schedule.status)}
                            </span>
                            <Show when={schedule.last_failure}>
                              {(failure) => <span class={ui.muted}>{failure().message}</span>}
                            </Show>
                          </div>
                        </td>
                        <td><code>{schedule.cron.pattern}</code></td>
                        <td><span class={ui.muted}>{schedule.cron.timezone}</span></td>
                        <td>{dateCell(schedule.next_fire_at)}</td>
                        <td>{dateCell(schedule.last_fire_at)}</td>
                        <td>{schedule.generation}</td>
                        <td><code>{schedule.id}</code></td>
                      </tr>
                    )}
                  </For>
                </tbody>
              </table>
            </div>
          </Show>
        </Show>
      </Show>
    </section>
  );
}
