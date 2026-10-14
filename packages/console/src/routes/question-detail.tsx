import { useParams } from "@solidjs/router";
import { createQuery } from "@tanstack/solid-query";
import { Show } from "solid-js";
import { Question } from "../features/asks/Question";
import { useScope } from "../lib/scope";
import { getAsk, sessionConsolePath, type AskAddress } from "../lib/sessions";
import { PageHeader } from "../ui/PageHeader";
import { StatePanel } from "../ui/StatePanel";
import { ui } from "../ui/styles";

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
  const ask = createQuery(() => ({ queryKey: ["ask", props.address], queryFn: () => getAsk(props.address), enabled: enabled(), retry: false, refetchInterval: 5_000 }));
  return <section class={ui.page}>
    <PageHeader title="Question" breadcrumb={[{ href: sessionConsolePath(props.address.sessionID), label: "Session" }]} />
    <Show when={!enabled()}><StatePanel error="Question and environment scope are required." /></Show>
    <Show when={ask.isError}><StatePanel error={ask.error instanceof Error ? ask.error.message : "The question could not be loaded."} /></Show>
    <Show when={enabled() && ask.isPending}><StatePanel loading="Loading question..." /></Show>
    <Show when={ask.data}>{value => <Question ask={value()} address={props.address} />}</Show>
  </section>;
}
