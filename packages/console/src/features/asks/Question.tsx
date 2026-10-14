import { For, Show } from "solid-js";
import type { AskState } from "../../../../../sdk/typescript/src/contract";
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
      <KeyValue label="Answer format">{props.ask.answerControl?.type}</KeyValue>
      <Show when={props.ask.status === "responded"}>
        <KeyValue label="Answered by">
          <span class="inline-flex flex-wrap items-baseline gap-1.5">
            {props.ask.respondedByUserId ? "User" : "API key"}
            <IDText value={props.ask.respondedByUserId ?? props.ask.respondedByApiKeyId ?? ""} />
          </span>
        </KeyValue>
      </Show>
      <KeyValue label="Asked"><RelativeTime value={props.ask.createdAt} /></KeyValue>
    </KeyValueList>
  }>
    <Show when={props.ask.payloadExpired}><Notice tone="info">The question and answer payloads have expired.</Notice></Show>
    <Show when={props.ask.prompt}>{prompt => <Panel title="Question"><ContentView content={prompt()} /></Panel>}</Show>
    <Show when={props.ask.answerControl}>
      {control => <Panel title="Answer control"><CodeBlock label="Answer control" code={JSON.stringify(control(), null, 2)} /></Panel>}
    </Show>
    <Show when={props.ask.status === "responded" && !props.ask.payloadExpired}>
      <Panel title="Recorded answer"><CodeBlock label="Recorded answer" code={JSON.stringify(props.ask.answer, null, 2)} /></Panel>
    </Show>
    <Show when={props.ask.status === "pending" && props.ask.answerControl && !props.ask.payloadExpired}>
      <section aria-label="Answer through CLI" class={ui.windowStack}>
        <p class={ui.muted}>Answer in the connected Slack conversation or use the CLI below. These commands use your Helmr login; unset HELMR_API_KEY first.</p>
        <CodeBlock code={commands().login} copy />
        <p class={ui.muted}>Read the complete question and its current status:</p>
        <CodeBlock code={commands().get} copy />
        <p class={ui.muted}>Save your answer in answer.json: a JSON string for text, or a choice object with a selected array of &#123;id, value&#125; entries in declaration order. Include custom text only when allowText is enabled.</p>
        <p class={ui.muted}>Replace RESPONSE_ID with a unique ID for your answer. Reuse the same ID and unchanged answer when retrying an uncertain response.</p>
        <CodeBlock code={commands().respond} copy />
      </section>
    </Show>
  </DetailLayout>;
}
