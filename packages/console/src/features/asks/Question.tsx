import { For, Show } from "solid-js";
import type { AskState } from "../../../../../sdk/typescript/src/contract";
import type { ChoiceQuestion } from "../../../../../sdk/typescript/src/question";
import type { Content } from "../../../../../sdk/typescript/src/content";
import { questionCLICommands, type AskAddress } from "../../lib/sessions";
import { CodeBlock } from "../../ui/CodeBlock";
import { IDText } from "../../ui/IDText";
import { Notice } from "../../ui/Notice";
import { DetailLayout, KeyValue, KeyValueList, Panel } from "../../ui/Panel";
import { RelativeTime } from "../../ui/RelativeTime";
import { StatusBadge } from "../../ui/StatusBadge";
import { ui } from "../../ui/styles";

export function ContentView(props: { content: Content }) {
  return <For each={props.content}>{part => <CodeBlock code={part.type === "text" ? part.text : JSON.stringify(part.value, null, 2)} />}</For>;
}

export function Question(props: { ask: AskState; address: AskAddress }) {
  const commands = () => questionCLICommands(props.address, window.location.origin);
  return <DetailLayout rail={
    <KeyValueList title="Question details">
      <KeyValue label="Status"><StatusBadge resource="question" status={props.ask.status} /></KeyValue>
      <KeyValue label="Answer format">{props.ask.question?.type}</KeyValue>
      <Show when={props.ask.status === "answered"}>
        <KeyValue label="Answered by">
          <span class="inline-flex flex-wrap items-baseline gap-1.5">
            {props.ask.answeredByUserId ? "User" : "API key"}
            <IDText value={props.ask.answeredByUserId ?? props.ask.answeredByApiKeyId ?? ""} />
          </span>
        </KeyValue>
      </Show>
      <KeyValue label="Asked"><RelativeTime value={props.ask.createdAt} /></KeyValue>
    </KeyValueList>
  }>
    <Show when={props.ask.payloadExpired}><Notice tone="info">The question and answer payloads have expired.</Notice></Show>
    <Show when={props.ask.question}>{question => <Panel title={question().title ?? "Question"}>
      <ContentView content={question().prompt} />
      <Show when={question().type === "choice"}>
        <ul class={ui.blockStack}><For each={(question() as ChoiceQuestion).options}>{option => <li><strong>{option.label}</strong><Show when={option.description}><p class="whitespace-pre-wrap break-words">{option.description}</p></Show></li>}</For></ul>
      </Show>
      <details><summary class="cursor-pointer font-mono text-chrome">Inspect question</summary><CodeBlock code={JSON.stringify(question(), null, 2)} label="Question definition" class="mt-2" /></details>
    </Panel>}</Show>
    <Show when={props.ask.status === "answered" && !props.ask.payloadExpired}>
      <Panel title="Recorded answer"><CodeBlock code={JSON.stringify(props.ask.answer, null, 2)} /></Panel>
    </Show>
    <Show when={props.ask.status === "pending" && props.ask.question && !props.ask.payloadExpired}>
      <Panel title="Answer through CLI">
        <p class={ui.muted}>Answer in the connected Slack conversation or use the CLI below. These commands use your Helmr login; unset HELMR_API_KEY first.</p>
        <CodeBlock code={commands().login} copy />
        <p class={ui.muted}>Read the complete question and its current status:</p>
        <CodeBlock code={commands().get} copy />
        <p class={ui.muted}>Save answer.json as a JSON string for text, or an object with selectedIds and any enabled custom or notes fields. Use option IDs from the question.</p>
        <p class={ui.muted}>Replace ANSWER_ID with a unique answer ID. Reuse it with the same answer if delivery is uncertain.</p>
        <CodeBlock code={commands().answer} copy />
      </Panel>
    </Show>
  </DetailLayout>;
}
