import { createQuery } from "@tanstack/solid-query";
import { request } from "./api";
import { useScopeIDs } from "./scope";
import type { ScopeIDs } from "./scope-link";

export type AttentionKind = "question" | "hold" | "failed_turn" | "slack_reauthorization";

// One thing in the Environment waiting on a person. `id` names the Question,
// hold, Turn or Slack connection; `at` is when it started waiting.
export type AttentionItem = {
  kind: AttentionKind;
  id: string;
  at: string;
  agent: string;
  session_id?: string;
  turn_id?: string;
  prompt?: string;
  hold_kind?: "interrupt" | "process_failed" | "recovery_gap" | "storage_pressure";
  turn_sequence?: number;
  error_code?: string;
  workspace_name?: string | null;
};

// Every waiting item of each kind. `slack_reauthorization` is present only for
// people who manage Slack connections.
export type AttentionCounts = {
  question?: number;
  hold?: number;
  failed_turn?: number;
  slack_reauthorization?: number;
};

// What is waiting on a person in one Environment, oldest first. Each kind
// returns at most 100 items; the counts are complete.
export type Attention = { items: AttentionItem[]; counts: AttentionCounts };

export function getAttention(scope: ScopeIDs): Promise<Attention> {
  return request<Attention>(
    `/api/projects/${encodeURIComponent(scope.projectID)}/environments/${encodeURIComponent(scope.environmentID)}/attention`,
  );
}

// The number of waiting items across the kinds this person sees.
export function attentionTotal(counts: AttentionCounts): number {
  return (counts.question ?? 0) + (counts.hold ?? 0) + (counts.failed_turn ?? 0) + (counts.slack_reauthorization ?? 0);
}

// Content is fetched only by surfaces that display the waiting items.
export function useAttention() {
  const scope = useScopeIDs();
  return createQuery(() => ({
    queryKey: ["attention", scope().projectID, scope().environmentID],
    queryFn: () => getAttention(scope()),
    enabled: !!scope().projectID && !!scope().environmentID,
    retry: false,
    refetchInterval: 10_000,
  }));
}

export function useAttentionCounts() {
  const scope = useScopeIDs();
  return createQuery(() => ({
    queryKey: ["attention-counts", scope().projectID, scope().environmentID],
    queryFn: () => request<{ counts: AttentionCounts }>(
      `/api/projects/${encodeURIComponent(scope().projectID)}/environments/${encodeURIComponent(scope().environmentID)}/attention/counts`,
    ),
    enabled: !!scope().projectID && !!scope().environmentID,
    retry: false,
    refetchInterval: 10_000,
  }));
}
