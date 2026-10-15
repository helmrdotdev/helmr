import { createInfiniteQuery } from "@tanstack/solid-query";
import { createMemo, For, Show } from "solid-js";
import { deploymentHref } from "../features/deployments/navigation";
import { agentHref } from "../lib/definitions";
import { loadError } from "../lib/error";
import { useScopeIDs } from "../lib/scope";
import { listTriggers } from "../lib/triggers";
import { DataTable, LoadMore, PrimaryLink } from "../ui/DataTable";
import { IDText } from "../ui/IDText";
import { QueryState } from "../ui/QueryState";
import { TableSkeleton } from "../ui/Skeleton";
import { RelativeTime } from "../ui/RelativeTime";

const failure = (error: unknown) => loadError(error, "Could not load triggers.", { forbidden: "You do not have permission to view triggers." });

// Trigger activations across Deployments: every Agent's in the tab's Environment,
// or one Agent's.
export function TriggerList(props: { agent?: string }) {
  const scope = useScopeIDs();
  const agent = () => props.agent ?? "";
  const triggers = createInfiniteQuery(() => ({
    queryKey: ["triggers", scope().projectID, scope().environmentID, agent()],
    queryFn: ({ pageParam }) => listTriggers(scope(), pageParam || undefined, agent() || undefined),
    initialPageParam: "",
    getNextPageParam: page => page.next_cursor,
    enabled: !!scope().projectID && !!scope().environmentID,
    retry: false,
  }));
  const items = createMemo(() => triggers.data?.pages.flatMap(page => page.triggers) ?? []);
  const columns = () => [
    "Trigger",
    ...(agent() ? [] : ["Agent"]),
    "Cron",
    "Timezone",
    "Next",
    "Active from",
    "Active until",
    "Deployment",
  ];
  return (
    <QueryState
      query={triggers}
      pending={<TableSkeleton label="Loading triggers" columns={columns()} minWidth="min-w-230" />}
      error={failure}
      empty={{
        when: (data) => data.pages.every(page => page.triggers.length === 0),
        title: agent() ? "No trigger activations for this agent." : "No trigger activations in this environment.",
      }}
    >
      {() => (
        <DataTable
          columns={columns()}
          minWidth="min-w-230"
          footer={<LoadMore count={items().length} noun={{ one: "trigger", other: "triggers" }} hasMore={triggers.hasNextPage} loading={triggers.isFetchingNextPage} failed={triggers.isFetchNextPageError} onLoadMore={() => void triggers.fetchNextPage()} />}
        >
          <For each={items()}>{trigger => (
            <tr>
              <td><code>{trigger.trigger_key}</code></td>
              <Show when={!agent()}>
                <td><PrimaryLink href={agentHref(trigger.agent, "triggers")}>{trigger.agent}</PrimaryLink></td>
              </Show>
              <td><code>{trigger.cron.pattern}</code></td>
              <td>{trigger.cron.timezone}</td>
              <td><RelativeTime value={trigger.next_fire_at} /></td>
              <td><RelativeTime value={trigger.active_from} /></td>
              <td><RelativeTime value={trigger.active_until} /></td>
              <td><IDText value={trigger.deployment_id} mode="link" href={deploymentHref(trigger.deployment_id)} /></td>
            </tr>
          )}</For>
        </DataTable>
      )}
    </QueryState>
  );
}
