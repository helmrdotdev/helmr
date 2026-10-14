import { statusLabel } from "./label";
import { cx, toneClass } from "./styles";

type Tone = "active" | "waiting" | "succeeded" | "revoked" | "expired";

const TONE_CLASSES: Record<Tone, string> = {
  active: toneClass.info,
  waiting: cx(toneClass.warning, "before:animate-pulse"),
  succeeded: toneClass.success,
  revoked: toneClass.danger,
  expired: toneClass.neutral,
};

const TONES = {
  turn: { queued: "waiting", running: "active", finalizing: "waiting", completed: "succeeded", failed: "revoked", interrupted: "expired", cancelled: "revoked" },
  session: { open: "active", closing: "waiting", closed: "succeeded", cancelled: "revoked" },
  // Only the lifecycle states the console shows; an available Computer shows its
  // residency (ComputerStatus).
  computer: { deleted: "expired", deleting: "expired" },
  computer_residency: { cold: "expired", starting: "waiting", running: "active", parking: "waiting", restoring: "waiting", unavailable: "revoked" },
  computer_command: { pending: "waiting", starting: "waiting", running: "active", stopping: "waiting", exited: "succeeded", failed: "revoked", cancelled: "revoked", timed_out: "revoked", lost: "revoked" },
  trigger: { active: "active", errored: "revoked", archived: "expired" },
  deployment: { current: "active", not_current: "expired" },
  environment: { working: "active", default: "expired" },
  api_key: { active: "succeeded", expired: "expired", revoked: "revoked" },
  secret: { active: "succeeded", revoked: "revoked" },
  member: { active: "succeeded", disabled: "revoked", invited: "waiting" },
  role: { owner: "active", admin: "expired", developer: "expired", viewer: "expired" },
  worker_group: { active: "active", paused: "waiting", draining: "waiting", disabled: "revoked" },
  agent: { defined: "active", not_defined: "expired" },
  slack_connection: { setup_incomplete: "waiting", connected: "succeeded", reauthorization_required: "waiting", disconnected: "revoked" },
  computer_usage: { admitted: "waiting", running: "active", waiting: "waiting", parked: "expired", draining: "waiting", unreconciled: "revoked" },
  process: { starting: "waiting", ready: "active", stopping: "waiting", stopped: "expired", lost: "revoked", recovery_required: "revoked" },
  process_stop: { requested: "waiting", stopping: "waiting", stopped: "succeeded" },
  question: { pending: "waiting", answered: "succeeded", cancelled: "revoked" },
  attention: { question: "waiting", hold: "waiting", failed_turn: "revoked", slack_reauthorization: "revoked" },
  device_code: { pending: "waiting", approved: "succeeded", denied: "revoked", consumed: "expired", expired: "expired" },
} as const satisfies Record<string, Record<string, Tone>>;

// Labels that read better than the status name.
const LABELS: Partial<Record<keyof typeof TONES, Record<string, string>>> = {
  agent: { defined: "In current deployment", not_defined: "Not in current deployment" },
  attention: { question: "Question", hold: "Held", failed_turn: "Failed", slack_reauthorization: "Connection" },
};

export type BadgeResource = keyof typeof TONES;
export type BadgeStatus<R extends BadgeResource> = keyof (typeof TONES)[R] & string;

export function StatusBadge<R extends BadgeResource>(props: { resource: R; status: BadgeStatus<R> }) {
  const tone = () => (TONES[props.resource] as Record<string, Tone>)[props.status] ?? "expired";
  return (
    <span
      class={cx(
        "inline-flex items-center gap-1.5 whitespace-nowrap rounded-xs border px-2 py-0.5 font-mono text-badge font-medium leading-normal before:size-1.5 before:bg-current before:content-['']",
        TONE_CLASSES[tone()],
      )}
    >
      {LABELS[props.resource]?.[props.status] ?? statusLabel(props.status)}
    </span>
  );
}
