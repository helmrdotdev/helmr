import { useParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { Show } from "solid-js";
import { Question } from "../features/asks/Question";
import { errorMessage } from "../lib/error";
import { useScope } from "../lib/scope";
import { agentHref } from "../lib/definitions";
import { getAsk, getSession, sessionConsolePath, type AskAddress } from "../lib/sessions";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { EmptyState, QueryState } from "../ui/QueryState";
import { PageSkeleton } from "../ui/Skeleton";

export function QuestionDetail() {
  const params = useParams(), scope = useScope();
  const address = (): AskAddress => ({
    sessionID: params["session_id"] ?? "", turnID: params["turn_id"] ?? "", askID: params["ask_id"] ?? "",
    projectID: scope.selectedProjectID(), environmentID: scope.selectedEnvironmentID(),
  });
  return <Show when={JSON.stringify(address())} keyed>{(_identity) => <QuestionDetailContent address={address()} />}</Show>;
}
function QuestionDetailContent(props: { address: AskAddress }) {
  const enabled = () => Object.values(props.address).every(Boolean);
  // The Session names the Agent for the breadcrumb; the Session page shares this query.
  const session = createQuery(() => ({ queryKey: ["session", props.address.sessionID, props.address.projectID, props.address.environmentID], queryFn: () => getSession(props.address), enabled: enabled(), retry: false }));
  const ask = createQuery(() => ({ queryKey: ["ask", props.address], queryFn: () => getAsk(props.address), enabled: enabled(), retry: false, refetchInterval: 5_000 }));
  return <Page>
    <PageHeader title="Question" breadcrumb={[
      { href: "/agents", label: "Agents" },
      ...(session.data ? [{ href: agentHref(session.data.agent), label: session.data.agent }] : []),
      { href: sessionConsolePath(props.address.sessionID), label: "Session" },
    ]} />
    <Show when={enabled()} fallback={<EmptyState title="Select a project and environment." />}>
      <QueryState query={ask} pending={<PageSkeleton label="Loading question" sections={["Question"]} rail="Question details" />} error={(error) => errorMessage(error, "The question could not be loaded.")}>
        {value => <Question ask={value()} address={props.address} />}
      </QueryState>
    </Show>
  </Page>;
}
