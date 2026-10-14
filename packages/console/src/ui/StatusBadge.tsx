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
  computer: { available: "succeeded", deleted: "expired", deleting: "expired" },
  computer_command: { pending: "waiting", starting: "waiting", running: "active", stopping: "waiting", exited: "succeeded", failed: "revoked", cancelled: "revoked", timed_out: "revoked", lost: "revoked" },
  trigger: { active: "active", errored: "revoked", archived: "expired" },
  deployment: { current: "active" },
  environment: { current: "active", protected: "expired" },
  api_key: { active: "succeeded", expired: "expired", revoked: "revoked" },
  secret: { active: "succeeded", revoked: "revoked" },
  member: { active: "succeeded", disabled: "revoked" },
  invitation: { pending: "waiting", accepted: "succeeded", revoked: "revoked", expired: "expired" },
  role: { owner: "active", admin: "expired", developer: "expired", viewer: "expired" },
  worker_group: { active: "active", paused: "waiting", draining: "waiting", disabled: "revoked" },
} as const satisfies Record<string, Record<string, Tone>>;

type BadgeResource = keyof typeof TONES;
type BadgeStatus<R extends BadgeResource> = keyof (typeof TONES)[R] & string;

export function StatusBadge<R extends BadgeResource>(props: { resource: R; status: BadgeStatus<R> }) {
  const tone = () => (TONES[props.resource] as Record<string, Tone>)[props.status] ?? "expired";
  return (
    <span
      class={cx(
        "inline-flex items-center gap-1.5 whitespace-nowrap rounded-xs border px-2 py-0.5 font-mono text-badge font-medium leading-normal before:size-1.5 before:bg-current before:content-['']",
        TONE_CLASSES[tone()],
      )}
    >
      {statusLabel(props.status)}
    </span>
  );
}
