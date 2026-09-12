import { createInfiniteQuery, createQuery, useQueryClient } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { CancelTokenModal, CompleteTokenModal } from "../features/tokens/TokenActions";
import { tokenHref } from "../features/tokens/navigation";
import { ApiError } from "../lib/api";
import { getMe, hasPermission } from "../lib/auth";
import { useScope } from "../lib/scope";
import { listTokens, type TokenListItem, type TokenStatus } from "../lib/tokens";
import { DataTable } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { PageHeader } from "../ui/PageHeader";
import { RelativeTime } from "../ui/RelativeTime";
import { Select, type SelectOption } from "../ui/Select";
import { StatePanel } from "../ui/StatePanel";
import { StatusBadge } from "../ui/StatusBadge";
import { ui } from "../ui/styles";
import { TagList } from "../ui/TagList";

type TokenFilter = TokenStatus | "all";

const FILTERS: SelectOption<TokenFilter>[] = [
  { value: "all", label: "All tokens" },
  { value: "pending", label: "Pending" },
  { value: "completed", label: "Completed" },
  { value: "expired", label: "Expired" },
  { value: "cancelled", label: "Cancelled" },
];

function tokensErrorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "forbidden") {
    return "You do not have permission to view Tokens.";
  }
  if (error instanceof ApiError) return error.message;
  return "Could not load Tokens.";
}

export function Tokens() {
  const scope = useScope();
  const queryClient = useQueryClient();
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const me = createQuery(() => ({ queryKey: ["me"], queryFn: getMe, retry: false, staleTime: 60_000 }));
  const canComplete = () => hasPermission(me.data, "tokens.complete");
  const canCancel = () => hasPermission(me.data, "tokens.cancel");
  const [filter, setFilter] = createSignal<TokenFilter>("all");
  const [completing, setCompleting] = createSignal<TokenListItem | null>(null);
  const [cancelling, setCancelling] = createSignal<TokenListItem | null>(null);
  const tokens = createInfiniteQuery(() => ({
    queryKey: ["tokens", "list", filter(), projectID(), environmentID()],
    queryFn: ({ pageParam }) => {
      const status = filter();
      return listTokens(
        { projectID: projectID(), environmentID: environmentID() },
        { status: status === "all" ? undefined : status, cursor: pageParam || undefined, limit: 100 },
      );
    },
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor,
    enabled: !!projectID() && !!environmentID(),
    retry: false,
  }));
  const items = createMemo(() => tokens.data?.pages.flatMap((page) => page.tokens) ?? []);
  const refreshTokens = async () => {
    await queryClient.invalidateQueries({ queryKey: ["tokens"] });
  };

  return (
    <section class={ui.page}>
      <PageHeader
        title="Tokens"
        subtitle="Approvals and callbacks that Runs wait on. Pending Tokens can be completed with a JSON result or cancelled."
        actions={
          <div class="w-44">
            <Select<TokenFilter> value={filter()} options={FILTERS} onChange={setFilter} ariaLabel="Filter tokens" />
          </div>
        }
      />

      <Show when={tokens.isError}>
        <StatePanel error={tokensErrorMessage(tokens.error)} />
      </Show>
      <Show when={!tokens.isPending} fallback={<StatePanel loading="Loading Tokens..." />}>
        <Show
          when={items().length > 0}
          fallback={<StatePanel empty="No Tokens match this filter." hint="Tokens are created by Runs that wait for an approval or callback." />}
        >
          <DataTable columns={["Token", "Status", "Tags", "Timeout", "Created", { label: "Actions", srOnly: true }]} minWidth="min-w-200">
            <For each={items()}>
              {(token) => (
                <tr>
                  <td><IDText value={token.id} href={tokenHref(token.id)} /></td>
                  <td><StatusBadge resource="token" status={token.status} /></td>
                  <td><TagList tags={token.tags} /></td>
                  <td><RelativeTime value={token.timeout_at} /></td>
                  <td><RelativeTime value={token.created_at} /></td>
                  <td class={ui.actionsCell}>
                    <Show when={token.status === "pending"}>
                      <div class="flex justify-end gap-1.5">
                        <Show when={canComplete()}>
                          <button type="button" class={ui.button} onClick={() => setCompleting(token)}>Complete</button>
                        </Show>
                        <Show when={canCancel()}>
                          <button type="button" class={ui.dangerOutlineButton} onClick={() => setCancelling(token)}>Cancel</button>
                        </Show>
                      </div>
                    </Show>
                  </td>
                </tr>
              )}
            </For>
          </DataTable>
          <Show when={tokens.hasNextPage}>
            <div class={ui.actionRow}>
              <button
                type="button"
                class={ui.secondaryButton}
                disabled={tokens.isFetchingNextPage}
                onClick={() => void tokens.fetchNextPage()}
              >
                {tokens.isFetchingNextPage ? "Loading..." : "Load more"}
              </button>
            </div>
          </Show>
        </Show>
      </Show>

      <Show when={completing()}>
        {(token) => (
          <CompleteTokenModal
            token={token()}
            projectID={projectID()}
            environmentID={environmentID()}
            onClose={() => setCompleting(null)}
            onCompleted={refreshTokens}
          />
        )}
      </Show>
      <Show when={cancelling()}>
        {(token) => (
          <CancelTokenModal
            token={token()}
            projectID={projectID()}
            environmentID={environmentID()}
            onClose={() => setCancelling(null)}
            onCancelled={refreshTokens}
          />
        )}
      </Show>
    </section>
  );
}
