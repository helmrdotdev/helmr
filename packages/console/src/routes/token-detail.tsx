import { useParams } from "@solidjs/router";
import { createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, Show } from "solid-js";
import { CancelTokenModal, CompleteTokenModal } from "../features/tokens/TokenActions";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { useScope } from "../lib/scope";
import { getToken, isTerminalTokenStatus } from "../lib/tokens";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { DetailItem, DetailList, Panel } from "../ui/Panel";
import { RelativeTime } from "../ui/RelativeTime";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { TagList } from "../ui/TagList";

function tokenErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.status === 404) return "Token not found.";
  if (error instanceof ApiError && error.code === "forbidden") return "You do not have permission to view this Token.";
  if (error instanceof ApiError) return error.message;
  return "Could not load this Token.";
}

function JSONPanel(props: { title: string; value: unknown }) {
  return (
    <Panel title={props.title}>
      <pre class={ui.codeBlock}>{JSON.stringify(props.value, null, 2)}</pre>
    </Panel>
  );
}

export function TokenDetail() {
  const params = useParams();
  const scope = useScope();
  const queryClient = useQueryClient();
  const tokenID = createMemo(() => params["token_id"]?.trim() ?? "");
  const projectID = createMemo(() => scope.selectedProjectID());
  const environmentID = createMemo(() => scope.selectedEnvironmentID());
  const hasScope = createMemo(() => projectID() !== "" && environmentID() !== "");
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const token = createQuery(() => ({
    queryKey: ["tokens", "detail", tokenID(), projectID(), environmentID()],
    queryFn: () => getToken(tokenID(), { projectID: projectID(), environmentID: environmentID() }),
    enabled: tokenID() !== "" && hasScope(),
    retry: false,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status && !isTerminalTokenStatus(status) ? 5_000 : false;
    },
  }));
  const [completing, setCompleting] = createSignal(false);
  const [cancelling, setCancelling] = createSignal(false);
  const pending = () => token.data?.status === "pending";
  const refreshTokens = async () => {
    await queryClient.invalidateQueries({ queryKey: ["tokens"] });
  };

  return (
    <section class={ui.page}>
      <PageHeader
        title="Token"
        back={{ href: "/tokens", label: "Tokens" }}
        badge={<Show when={token.data}>{(current) => <StatusBadge resource="token" status={current().status} />}</Show>}
        subtitle={<Show when={token.data}>{(current) => <IDText value={current().id} mode="full" />}</Show>}
        actions={
          <Show when={pending()}>
            <Show when={hasPermission(me.data, "tokens.complete")}>
              <button type="button" class={ui.button} onClick={() => setCompleting(true)}>Complete</button>
            </Show>
            <Show when={hasPermission(me.data, "tokens.cancel")}>
              <button type="button" class={ui.dangerOutlineButton} onClick={() => setCancelling(true)}>Cancel</button>
            </Show>
          </Show>
        }
      />

      <Show when={tokenID() !== ""} fallback={<StatePanel error="Token ID is required." />}>
        <Show when={hasScope()} fallback={<StatePanel empty="Select a project and environment." />}>
          <Show when={!token.isPending} fallback={<StatePanel loading="Loading Token..." />}>
            <Show
              when={token.data}
              fallback={
                <StatePanel empty={tokenErrorMessage(token.error)}>
                  <button class={ui.secondaryButton} type="button" onClick={() => void token.refetch()}>
                    Retry
                  </button>
                </StatePanel>
              }
            >
              {(current) => (
                <div class="grid grid-cols-[minmax(0,1fr)_310px] items-start gap-3.5 max-[960px]:grid-cols-1">
                  <div class="grid gap-3.5">
                    <Show when={current().status === "completed"}>
                      <JSONPanel title="Result" value={current().result ?? null} />
                    </Show>
                    <JSONPanel title="Metadata" value={current().metadata ?? null} />
                  </div>

                  <DetailList title="Token details">
                    <DetailItem label="Status"><StatusBadge resource="token" status={current().status} /></DetailItem>
                    <DetailItem label="Tags"><TagList tags={current().tags} /></DetailItem>
                    <DetailItem label="Timeout"><RelativeTime value={current().timeout_at} /></DetailItem>
                    <DetailItem label="Completed"><RelativeTime value={current().completed_at} /></DetailItem>
                    <DetailItem label="Created"><RelativeTime value={current().created_at} /></DetailItem>
                    <DetailItem label="Updated"><RelativeTime value={current().updated_at} /></DetailItem>
                  </DetailList>
                </div>
              )}
            </Show>
          </Show>
        </Show>
      </Show>

      <Show when={completing() && token.data}>
        {(current) => (
          <CompleteTokenModal
            token={current()}
            projectID={projectID()}
            environmentID={environmentID()}
            onClose={() => setCompleting(false)}
            onCompleted={refreshTokens}
          />
        )}
      </Show>
      <Show when={cancelling() && token.data}>
        {(current) => (
          <CancelTokenModal
            token={current()}
            projectID={projectID()}
            environmentID={environmentID()}
            onClose={() => setCancelling(false)}
            onCancelled={refreshTokens}
          />
        )}
      </Show>
    </section>
  );
}
