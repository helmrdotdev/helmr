import { For, Show } from "solid-js";
import { Tag } from "../../ui/Tag";
import { Icon } from "../../ui/Icon";
import type { AskState } from "../../../../../sdk/typescript/src/contract";
import type { ChoiceQuestion } from "../../../../../sdk/typescript/src/question";
import type { Content } from "../../../../../sdk/typescript/src/content";
import type { JsonValue } from "../../../../../sdk/typescript/src/contract";
import { questionCLICommands, type AskAddress } from "../../lib/sessions";
import { CodeBlock } from "../../ui/CodeBlock";
import { JsonView, TextView } from "../../ui/PayloadView";
import { IDText } from "../../ui/IDText";
import { Notice } from "../../ui/Notice";
import { DetailLayout, KeyValue, KeyValueList, Panel } from "../../ui/Panel";
import { RelativeTime } from "../../ui/RelativeTime";
import { StatusBadge } from "../../ui/StatusBadge";
import { ui } from "../../ui/styles";

export function ContentView(props: { content: Content; label: string; inline?: boolean }) {
  return <For each={props.content}>{(part, index) => part.type === "text"
    ? <TextView text={part.text} label={`${props.label} part ${index() + 1}`} inline={props.inline} />
    : <JsonView value={part.value} label={`${props.label} part ${index() + 1}`} inline={props.inline} />}</For>;
}

// What the answer says, in words: the chosen options' labels, or the text.
function answerText(question: AskState["question"], answer: unknown): { choice: string | undefined; custom: string | undefined; notes: string | undefined; text: string | undefined } {
  if (typeof answer === "string") return { text: answer, choice: undefined, custom: undefined, notes: undefined };
  const record = (answer ?? {}) as { selected?: readonly { id: string }[]; custom?: string; notes?: string };
  const options = question?.type === "choice" ? question.options : [];
  const labels = (record.selected ?? []).map((selection) => options.find((option) => option.id === selection.id)?.label ?? selection.id);
  return { text: undefined, choice: labels.length > 0 ? labels.join(", ") : undefined, custom: record.custom, notes: record.notes };
}

function formatLabel(question: AskState["question"]): string | undefined {
  if (!question) return undefined;
  if (question.type === "text") return "Text";
  return question.multiple ? "Multiple choice" : "Single choice";
}

export function Question(props: { ask: AskState; address: AskAddress }) {
  const commands = () => questionCLICommands(props.address, window.location.origin);
  const answered = () => props.ask.status === "answered" && !props.ask.payloadExpired;
  const answer = () => answerText(props.ask.question, props.ask.answer);
  const selected = (id: string) => answered() && ((props.ask.answer ?? {}) as { selected?: readonly { id: string }[] }).selected?.some((selection) => selection.id === id) === true;
  return <DetailLayout rail={
    <KeyValueList title="Question details">
      <KeyValue label="Status"><StatusBadge resource="question" status={props.ask.status} /></KeyValue>
      <Show when={props.ask.status === "answered"}>
        <KeyValue label="Answered by">
          <span class="inline-flex flex-wrap items-baseline gap-1.5">
            {props.ask.answeredByUserId ? "A user" : "An API key"}
            <span class="font-mono text-chrome text-console-faint"><IDText value={props.ask.answeredByUserId ?? props.ask.answeredByApiKeyId ?? ""} /></span>
          </span>
        </KeyValue>
      </Show>
      <KeyValue label="Format">{formatLabel(props.ask.question)}</KeyValue>
      <KeyValue label="Asked"><RelativeTime value={props.ask.createdAt} /></KeyValue>
      <Show when={props.ask.answeredAt}>{(at) => <KeyValue label="Answered"><RelativeTime value={at()} /></KeyValue>}</Show>
    </KeyValueList>
  }>
    <Show when={props.ask.payloadExpired}><Notice tone="info">The question and answer payloads have expired.</Notice></Show>
    <Show when={props.ask.question}>{question => <Panel title={question().title ?? "Question"}>
      <ContentView content={question().prompt} label="Question prompt" />
      <Show when={question().type === "choice"}>
        <ul class="m-0 flex list-none flex-col gap-2.5 p-0" aria-label="Options">
          <For each={(question() as ChoiceQuestion).options}>{option => (
            <li class="grid grid-cols-[16px_minmax(0,1fr)] gap-x-2" data-selected={selected(option.id) ? "" : undefined}>
              <span class="pt-0.5 text-console-accent">{selected(option.id) ? <Icon name="check" /> : null}</span>
              <div class="grid gap-0.5">
                <span class="flex flex-wrap items-center gap-2">
                  <span class={answered() && !selected(option.id) ? "text-console-muted" : "font-medium text-console-text"}>{option.label}</span>
                  <Show when={selected(option.id)}><Tag>selected</Tag></Show>
                </span>
                <Show when={option.description}><p class="m-0 whitespace-pre-wrap break-words text-console-muted">{option.description}</p></Show>
              </div>
            </li>
          )}</For>
        </ul>
      </Show>
      <Show when={answered()}>
        <dl class="m-0 grid grid-cols-[96px_minmax(0,1fr)] gap-x-3 gap-y-1.5 border-t border-console-border-soft pt-3 text-body" role="group" aria-label="Recorded answer">
          <Show when={answer().text !== undefined || answer().choice !== undefined}>
            <dt class="text-console-muted">Answer</dt>
            <dd class="m-0 whitespace-pre-wrap break-words">{answer().text ?? answer().choice}<Show when={answer().text === ""}><span class="text-console-muted">Empty text</span></Show></dd>
          </Show>
          <Show when={answer().custom}>{(value) => <><dt class="text-console-muted">Other</dt><dd class="m-0 whitespace-pre-wrap break-words">{value()}</dd></>}</Show>
          <Show when={answer().notes}>{(value) => <><dt class="text-console-muted">Notes</dt><dd class="m-0 whitespace-pre-wrap break-words">{value()}</dd></>}</Show>
        </dl>
      </Show>
      <details><summary class="cursor-pointer font-mono text-chrome">Inspect question and answer</summary><JsonView value={{ question: question(), ...(answered() ? { answer: props.ask.answer } : {}) } as unknown as JsonValue} label="Question and answer" class="mt-2" /></details>
    </Panel>}</Show>
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
