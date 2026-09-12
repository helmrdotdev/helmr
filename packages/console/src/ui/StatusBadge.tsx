import { cx } from "./styles";

type Tone = "active" | "waiting" | "succeeded" | "revoked" | "expired";

const TONE_CLASSES: Record<Tone, string> = {
  active: "border-[#9bb9e8] bg-[#eef4ff] text-console-info",
  waiting: "border-[#e5c26e] bg-[#fff7df] text-console-warning before:animate-pulse",
  succeeded: "border-[#a8c3ad] bg-[#eef7f0] text-console-success",
  revoked: "border-[#e6aaa4] bg-[#fff1ef] text-console-danger",
  expired: "border-console-border bg-console-bg-panel text-console-muted",
};

const TONES = {
  run: {
    queued: "active",
    running: "active",
    waiting: "waiting",
    retry_delayed: "waiting",
    cancel_requested: "waiting",
    succeeded: "succeeded",
    failed: "revoked",
    system_failed: "revoked",
    cancelled: "revoked",
    expired: "expired",
  },
  session: { open: "active", closed: "succeeded", cancelled: "expired", failed: "revoked" },
  token: { pending: "waiting", completed: "succeeded", expired: "expired", cancelled: "revoked" },
  workspace: { available: "succeeded", recovery_required: "revoked", deleting: "expired" },
  schedule: { active: "active", errored: "revoked", archived: "expired" },
  deployment: { current: "active" },
  environment: { current: "active", protected: "expired" },
  api_key: { active: "succeeded", expired: "expired", revoked: "revoked" },
  secret: { active: "succeeded", revoked: "revoked" },
  member: { active: "succeeded", disabled: "revoked" },
  invitation: { pending: "waiting", accepted: "succeeded", revoked: "revoked", expired: "expired" },
  role: { owner: "waiting", admin: "expired", developer: "expired", viewer: "expired" },
  worker_group: { active: "active", paused: "waiting", draining: "waiting", disabled: "revoked" },
} as const satisfies Record<string, Record<string, Tone>>;

export type BadgeResource = keyof typeof TONES;
export type BadgeStatus<R extends BadgeResource> = keyof (typeof TONES)[R] & string;

export function statusLabel(status: string): string {
  const spaced = status.replace(/_/g, " ");
  return spaced.charAt(0).toUpperCase() + spaced.slice(1);
}

export function StatusBadge<R extends BadgeResource>(props: { resource: R; status: BadgeStatus<R> }) {
  const tone = () => (TONES[props.resource] as Record<string, Tone>)[props.status] ?? "expired";
  return (
    <span
      class={cx(
        "inline-flex items-center gap-1.5 whitespace-nowrap rounded-xs border px-2 py-0.5 font-mono text-[11px] font-medium leading-normal before:size-1.5 before:bg-current before:content-['']",
        TONE_CLASSES[tone()],
      )}
    >
      {statusLabel(props.status)}
    </span>
  );
}
