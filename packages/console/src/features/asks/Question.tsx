import { For, Show } from "solid-js";
import type { AskState } from "../../../../../sdk/typescript/src/contract";
import type { Content } from "../../../../../sdk/typescript/src/content";
import { questionCLICommands, type AskAddress } from "../../lib/sessions";
import { ui } from "../../ui/styles";

export function ContentView(props: { content: Content }) {
  return <For each={props.content}>{part => <pre class="whitespace-pre-wrap break-words">{part.type === "text" ? part.text : JSON.stringify(part.value, null, 2)}</pre>}</For>;
}

export function Question(props: { ask: AskState; address: AskAddress }) {
  const commands = () => questionCLICommands(props.address, window.location.origin);
  return <article class="space-y-3" aria-label="Question">
    <p>Status: {props.ask.status}</p>
    <Show when={props.ask.payloadExpired}><p>The question and answer payloads have expired.</p></Show>
    <Show when={props.ask.prompt}>{prompt => <ContentView content={prompt()} />}</Show>
    <Show when={props.ask.answerControl}>
      {control => <div>
        <p>Answer format: {control().type}</p>
        <pre aria-label="Answer control" class="whitespace-pre-wrap break-words">{JSON.stringify(control(), null, 2)}</pre>
      </div>}
    </Show>
    <Show when={props.ask.status === "responded"}>
      <p>Answered by {props.ask.respondedByUserId ? "user" : "API key"}: {props.ask.respondedByUserId ?? props.ask.respondedByApiKeyId}</p>
      <Show when={!props.ask.payloadExpired}><pre aria-label="Recorded answer" class="whitespace-pre-wrap break-words">{JSON.stringify(props.ask.answer, null, 2)}</pre></Show>
    </Show>
    <Show when={props.ask.status === "pending" && props.ask.answerControl && !props.ask.payloadExpired}>
      <section aria-label="Answer through CLI" class="space-y-3">
        <p>Answer in the connected Slack conversation or use the CLI below. These commands use your Helmr login; unset HELMR_API_KEY first.</p>
        <pre class={ui.codeBlock}>{commands().login}</pre>
        <p>Read the complete question and its current status:</p>
        <pre class={ui.codeBlock}>{commands().get}</pre>
        <p>Save your answer in answer.json: a JSON string for text, or a choice object with a selected array of &#123;id, value&#125; entries in declaration order. Include custom text only when allowText is enabled.</p>
        <p>Replace RESPONSE_ID with a unique ID for your answer. Reuse the same ID and unchanged answer when retrying an uncertain response.</p>
        <pre class={ui.codeBlock}>{commands().respond}</pre>
      </section>
    </Show>
  </article>;
}
