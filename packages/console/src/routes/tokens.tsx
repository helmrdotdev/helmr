import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, createSignal, For, Show } from "solid-js";
import { formatRelative } from "../features/runs/display";
import { TokenStatusBadge } from "../features/tokens/display";
import { ApiError } from "../lib/api";
import { useScope } from "../lib/scope";
import { listTokens, type TokenStatus } from "../lib/tokens";
import { Select, type SelectOption } from "../ui/Select";
import { ui } from "../ui/styles";

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
  const projectID = () => scope.selectedProjectID();
  const environmentID = () => scope.selectedEnvironmentID();
  const [filter, setFilter] = createSignal<TokenFilter>("all");
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

  return (
    <section class={ui.page}>
      <div class={ui.pageHeader}>
        <div>
          <h1 class={ui.h1}>Tokens</h1>
          <p class={ui.pageSubtitle}>
            Approvals and callbacks that Runs wait on. Pending Tokens can be completed or cancelled from the Overview.
          </p>
        </div>
        <div class="w-44">
          <Select<TokenFilter>
            value={filter()}
            options={FILTERS}
            onChange={setFilter}
            ariaLabel="Filter tokens"
          />
        </div>
      </div>

      <Show when={tokens.isError}>
        <p class={ui.error} role="alert">{tokensErrorMessage(tokens.error)}</p>
      </Show>
      <Show when={!tokens.isPending} fallback={<p class={ui.muted}>Loading Tokens...</p>}>
        <Show
          when={items().length > 0}
          fallback={
            <div class={ui.emptyState}>
              <strong>No Tokens match this filter.</strong>
              <span>Tokens are created by Runs that wait for an approval or callback.</span>
            </div>
          }
        >
          <div class={ui.tableWrap}>
            <table class="min-w-200">
              <thead>
                <tr>
                  <th>Token</th>
                  <th>Status</th>
                  <th>Tags</th>
                  <th>Timeout</th>
                  <th>Created</th>
                </tr>
              </thead>
              <tbody>
                <For each={items()}>
                  {(token) => (
                    <tr>
                      <td><code>{token.id}</code></td>
                      <td><TokenStatusBadge status={token.status} /></td>
                      <td>
                        <Show when={token.tags.length > 0} fallback={<span class="text-console-faint">—</span>}>
                          <div class="flex flex-wrap gap-1">
                            <For each={token.tags}>
                              {(tag) => <span class="rounded-xs border border-console-border bg-console-bg-panel px-1.5 font-mono text-[10.5px] text-console-muted">{tag}</span>}
                            </For>
                          </div>
                        </Show>
                      </td>
                      <td><span class={ui.muted} title={token.timeout_at}>{formatRelative(token.timeout_at)}</span></td>
                      <td><span class={ui.muted}>{formatRelative(token.created_at)}</span></td>
                    </tr>
                  )}
                </For>
              </tbody>
            </table>
          </div>
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
    </section>
  );
}
