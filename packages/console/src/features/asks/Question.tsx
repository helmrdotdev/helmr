import { For, Show } from "solid-js";
import type { AskState } from "../../../../../sdk/typescript/src/contract";
import type { Content } from "../../../../../sdk/typescript/src/content";
import type { ChoiceQuestion } from "../../../../../sdk/typescript/src/question";
import { questionCLICommands, type AskAddress } from "../../lib/sessions";
import { ui } from "../../ui/styles";

export function ContentView(props: { content: Content }) {
  return <For each={props.content}>{part => <pre class="whitespace-pre-wrap break-words">{part.type === "text" ? part.text : JSON.stringify(part.value, null, 2)}</pre>}</For>;
}

export function Question(props: { ask: AskState; address: AskAddress }) {
  const commands = () => questionCLICommands(props.address, window.location.origin);
  return <article class="space-y-4 min-w-0" aria-label="Question">
    <p>Status: {props.ask.status}</p>
    <Show when={props.ask.payloadExpired}><p>The question and answer payloads have expired.</p></Show>
    <Show when={props.ask.question}>{question => <>
      <Show when={question().title}>{title => <h2 class={ui.h2}>{title()}</h2>}</Show>
      <ContentView content={question().prompt} />
      <details><summary class="cursor-pointer">Inspect question</summary><pre aria-label="Question definition" class={`${ui.codeBlock} whitespace-pre-wrap break-all`}>{JSON.stringify(question(), null, 2)}</pre></details>
      <Show when={question().type === "choice"}>
        <ul class="space-y-2"><For each={(question() as ChoiceQuestion).options}>{option => <li><strong>{option.label}</strong><Show when={option.description}><p class="whitespace-pre-wrap break-words">{option.description}</p></Show></li>}</For></ul>
      </Show>
    </>}</Show>
    <Show when={props.ask.status === "answered"}>
      <p>Answered by {props.ask.answeredByUserId ? "user" : "API key"}: {props.ask.answeredByUserId ?? props.ask.answeredByApiKeyId}</p>
      <Show when={!props.ask.payloadExpired}><pre aria-label="Recorded answer" class="whitespace-pre-wrap break-words">{JSON.stringify(props.ask.answer, null, 2)}</pre></Show>
    </Show>
    <Show when={props.ask.status === "pending"}>
      <details class="space-y-3 min-w-0" aria-label="Answer through CLI">
        <summary class="cursor-pointer">Answer with the CLI</summary>
        <p>Use your Helmr login; unset HELMR_API_KEY first.</p>
        <pre class={`${ui.codeBlock} whitespace-pre-wrap break-all`}>{commands().login}</pre>
        <pre class={`${ui.codeBlock} whitespace-pre-wrap break-all`}>{commands().get}</pre>
        <p>Save answer.json as a JSON string for text, or an object with selectedIds and any enabled custom or notes fields. Use option IDs from the question.</p>
        <p>Replace ANSWER_ID with a unique answer ID. Reuse it with the same answer if delivery is uncertain.</p>
        <pre class={`${ui.codeBlock} whitespace-pre-wrap break-all`}>{commands().answer}</pre>
      </details>
    </Show>
  </article>;
}
