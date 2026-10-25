import { useParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { Show } from "solid-js";
import { Question } from "../features/asks/Question";
import { hasPermission } from "../lib/auth";
import { loadError } from "../lib/error";
import { useMe } from "../lib/me";
import { useScope } from "../lib/scope";
import { getAsk, sessionConsolePath, type AskAddress } from "../lib/sessions";
import { Page } from "../ui/Page";
import { PageHeader } from "../ui/PageHeader";
import { isMissingSessionRead, NotFoundState, QueryState, NoScopeState } from "../ui/QueryState";
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
  const scope = useScope();
  const enabled = () => Object.values(props.address).every(Boolean);
  const ask = createQuery(() => ({ queryKey: ["ask", props.address], queryFn: () => getAsk(props.address), enabled: enabled(), retry: false, refetchInterval: 5_000 }));
  const me = useMe();
  const missing = (error: unknown) => isMissingSessionRead(error, hasPermission(me.data, "sessions.read", props.address));
  const notFound = () => !ask.data && ask.isError && missing(ask.error);
  return <Page>
    <PageHeader title={notFound() ? "Question not found" : "Question"} breadcrumb={[
      { href: "/sessions", label: "Sessions" },
      { href: sessionConsolePath(props.address.sessionID), label: "Session" },
    ]} />
    <Show when={enabled()} fallback={<NoScopeState />}>
      <QueryState query={ask} pending={<PageSkeleton label="Loading question" sections={["Question"]} rail="Question details" />} error={(error) => loadError(error, "The question could not be loaded.", { forbidden: "You do not have permission to view this question." })}
        missing={missing}
        notFound={<NotFoundState noun="question" environment={scope.selectedEnvironment()?.name} back={{ href: sessionConsolePath(props.address.sessionID), label: "Go to session" }} />}
      >
        {value => <Question ask={value()} address={props.address} />}
      </QueryState>
    </Show>
  </Page>;
}
