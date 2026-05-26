import type { RunStatus } from "../../lib/runs";
import { statusBadgeClass } from "../../ui/styles";

const STATUS_LABELS: Record<RunStatus, string> = {
  queued: "Queued",
  running: "Running",
  checkpointing: "Checkpointing",
  waiting: "Waiting",
  succeeded: "Succeeded",
  failed: "Failed",
  cancelled: "Cancelled",
};

export function formatRelative(iso: string | null | undefined): string {
  if (!iso) return "—";
  const time = new Date(iso).getTime();
  if (Number.isNaN(time)) return "—";
  const diff = Date.now() - time;
  if (Math.abs(diff) < 45_000) return "just now";

  const minute = 60_000;
  const hour = 60 * minute;
  const day = 24 * hour;
  const units = [
    { name: "day", value: day },
    { name: "hour", value: hour },
    { name: "minute", value: minute },
  ];
  const unit = units.find((candidate) => Math.abs(diff) >= candidate.value) ?? units[2];
  if (!unit) return "—";
  const count = Math.max(1, Math.round(Math.abs(diff) / unit.value));
  const suffix = count === 1 ? unit.name : `${unit.name}s`;
  return diff >= 0 ? `${count} ${suffix} ago` : `in ${count} ${suffix}`;
}

export function StatusBadge(props: { status: RunStatus }) {
  const tone = (): "active" | "waiting" | "succeeded" | "revoked" => {
    if (props.status === "queued" || props.status === "running" || props.status === "checkpointing") return "active";
    if (props.status === "waiting") return "waiting";
    if (props.status === "succeeded") return "succeeded";
    return "revoked";
  };
  return (
    <span class={statusBadgeClass(tone())}>
      {STATUS_LABELS[props.status]}
    </span>
  );
}
