import { createEffect, createMemo, createSignal, For, Match, on, onCleanup, onMount, Show, Switch, type JSX } from "solid-js";
import type { JsonValue } from "../../../../../sdk/typescript/src/contract";
import { askConsolePath, type SessionAddress, type SessionEvent, type SessionTurn } from "../../lib/sessions";
import { JsonView } from "../../ui/PayloadView";
import { Segmented } from "../../ui/Field";
import { Popover } from "../../ui/Popover";
import { Loading, SkeletonBar } from "../../ui/Skeleton";
import { byteLength, formatBytes, preview } from "../../ui/payload";
import { cx, toneClass, ui, type Tone } from "../../ui/styles";
import { useClipboard } from "../../ui/useClipboard";
import {
  buildBlocks, clockTime, contentSummary, duration, eventRecord, eventSummary, eventTone, recordView,
  inputSummary, isOutput, isProblem, parseEvent, payloadSize, searchText, type LogBlock, type LogView,
} from "./log";
import type { SessionLog } from "./sessionLog";
import { TurnTimeline, turnStatusColor } from "./TurnTimeline";

type TurnBlock = Extract<LogBlock, { kind: "turn" }>;
type Row =
  | { kind: "turn"; block: TurnBlock; events: SessionEvent[] }
  | { kind: "session"; event: SessionEvent };

const shortcuts: [string, string][] = [
  ["j / k", "next / previous line"],
  ["Enter", "open or close the line"],
  ["[ / ]", "previous / next turn"],
  ["e / E", "next / previous problem"],
  ["/", "search"],
  ["G", "latest events"],
];

// The Session's history as one log: Turns group their events, a timeline above
// shows every Turn, and filters and search narrow the events.
export function SessionLogView(props: {
  log: SessionLog;
  address: SessionAddress;
  live: boolean;
  // What the person can do with the open Session, beside the live line.
  liveHint?: string;
  canInterrupt: boolean;
  onInterruptTurn: (turnID: string) => void;
}) {
  const log = props.log;
  // What the reader is looking for: everything, what the agent produced or
  // asked (and each Turn's input, response and result), or what went wrong.
  const [view, setView] = createSignal<LogView>("all");
  const [query, setQuery] = createSignal("");
  const [overrides, setOverrides] = createSignal<ReadonlyMap<string, boolean>>(new Map());
  const [highlight, setHighlight] = createSignal<number>();
  const [follow, setFollow] = createSignal(false);
  const [help, setHelp] = createSignal(false);
  let section: HTMLElement | undefined;
  let search: HTMLInputElement | undefined;
  let helpButton: HTMLButtonElement | undefined;

  const filtering = () => view() !== "all" || query().trim() !== "";
  const turnRows = () => view() !== "problems" && query().trim() === "";
  const visible = (event: SessionEvent) => {
    if (view() === "output" && !isOutput(event.kind)) return false;
    if (view() === "problems" && !isProblem(event.kind)) return false;
    const needle = query().trim().toLowerCase();
    return !needle || searchText(event).includes(needle);
  };
  const turnRecord = (block: TurnBlock) => log.turnByID(block.turnID);
  // Turns start closed. A filter or search opens the Turns it matches and a
  // link opens its target's Turn.
  const newestOwner = createMemo(() => log.events().at(-1)?.turn_id);
  const defaultOpen = (block: TurnBlock) =>
    filtering() ||
    block.events.some(event => event.sequence === highlight());
  // Following opens the Turn the newest event belongs to and keeps it open, so
  // scrolling up to read it does not close it.
  createEffect(on([follow, newestOwner], ([following, owner]) => {
    if (following && owner && overrides().get(owner) === undefined) setOverrides(current => new Map(current).set(owner, true));
  }));
  const isOpen = (block: TurnBlock) => overrides().get(block.turnID) ?? defaultOpen(block);
  const toggleTurn = (block: TurnBlock) => setOverrides(current => new Map(current).set(block.turnID, !isOpen(block)));

  // Rows render by key so an update keeps mounted rows (and what is open or
  // focused in them) instead of rebuilding the list.
  const rowKey = (row: Row) => row.kind === "session" ? `session:${row.event.sequence}` : `turn:${row.block.turnID}`;
  const rows = createMemo((): Row[] => {
    const out: Row[] = [];
    for (const block of buildBlocks(log.events(), log.turnSequence)) {
      if (block.kind === "session") {
        if (visible(block.event)) out.push({ kind: "session", event: block.event });
        continue;
      }
      const events = block.events.filter(visible);
      if (filtering() && events.length === 0) continue;
      out.push({ kind: "turn", block, events });
    }
    return out;
  });
  const problemCount = createMemo(() => log.events().filter(event => isProblem(event.kind)).length);
  const rowMap = createMemo(() => new Map(rows().map(row => [rowKey(row), row])));
  const rowKeys = createMemo(() => rows().map(rowKey));

  // Follow: stay at the newest events while the reader is at the bottom.
  const atBottom = () => window.innerHeight + window.scrollY >= document.documentElement.scrollHeight - 4;
  const scrollToEnd = () => requestAnimationFrame(() => window.scrollTo({ top: document.documentElement.scrollHeight }));
  onMount(() => {
    let lastY = window.scrollY;
    const onScroll = () => {
      if (window.scrollY < lastY - 2) setFollow(false);
      else if (props.live && atBottom() && !follow()) setFollow(true);
      lastY = window.scrollY;
    };
    window.addEventListener("scroll", onScroll, { passive: true });
    onCleanup(() => window.removeEventListener("scroll", onScroll));
  });
  createEffect(on(() => log.events().length, () => {
    if (follow()) {
      log.clearAppended();
      scrollToEnd();
    }
  }, { defer: true }));
  const goLatest = () => {
    setHighlight(undefined);
    log.clearAppended();
    setFollow(props.live);
    scrollToEnd();
  };

  const focusLine = (selector: string, step: 1 | -1) => {
    const lines = Array.from(section?.querySelectorAll<HTMLElement>(selector) ?? []);
    if (lines.length === 0) return;
    const current = lines.findIndex(line => line.contains(document.activeElement));
    const anchor = current === -1 ? Array.from(section?.querySelectorAll<HTMLElement>("[data-log-line]") ?? []).findIndex(line => line.contains(document.activeElement)) : current;
    let next = current === -1 && anchor !== -1
      ? lines.findIndex(line => line.compareDocumentPosition(document.activeElement!) & Node.DOCUMENT_POSITION_PRECEDING)
      : current + step;
    if (step === -1 && current === -1 && anchor !== -1) next = Math.max(0, next - 1);
    const target = lines[(next + lines.length) % lines.length];
    (target?.querySelector<HTMLElement>(":scope > button, :scope > div > button") ?? target)?.focus();
    target?.scrollIntoView({ block: "nearest" });
    setFollow(false);
  };
  const onKeyDown = (event: KeyboardEvent) => {
    const typing = event.target instanceof HTMLInputElement || event.target instanceof HTMLTextAreaElement;
    if (typing) {
      if (event.key === "Escape") (event.target as HTMLElement).blur();
      return;
    }
    if (event.metaKey || event.ctrlKey || event.altKey) return;
    const actions: Record<string, () => void> = {
      j: () => focusLine("[data-log-line]", 1),
      k: () => focusLine("[data-log-line]", -1),
      "]": () => focusLine("[data-turn-header]", 1),
      "[": () => focusLine("[data-turn-header]", -1),
      e: () => focusLine("[data-problem]", 1),
      E: () => focusLine("[data-problem]", -1),
      "/": () => search?.focus(),
      G: () => void goLatest(),
      "?": () => setHelp(!help()),
    };
    const action = actions[event.key];
    if (action) {
      event.preventDefault();
      action();
    }
  };

  // Links: #event-<sequence> opens that event's Turn and highlights it;
  // #turn-<id> opens that Turn.
  const openFromHash = () => {
    const hash = window.location.hash.slice(1);
    const event = /^event-(\d+)$/.exec(hash);
    const turn = /^turn-([0-9a-f-]{36})$/i.exec(hash);
    if (event || turn) {
      // A link shows its target: filters clear and following stops.
      setView("all");
      setQuery("");
      setFollow(false);
    }
    if (event) {
      // A link past the newest event points at the newest one.
      const sequence = Math.min(Number(event[1]), log.events().at(-1)?.sequence ?? 0);
      setHighlight(sequence);
      const owner = log.events().find(item => item.sequence === sequence)?.turn_id;
      if (owner) setOverrides(current => new Map(current).set(owner, true));
      requestAnimationFrame(() => document.getElementById(`event-${sequence}`)?.scrollIntoView({ block: "center" }));
    } else if (turn) {
      const id = turn[1]!;
      setOverrides(current => new Map(current).set(id, true));
      requestAnimationFrame(() => document.getElementById(`turn-${id}`)?.scrollIntoView({ block: "start" }));
    }
  };
  onMount(() => {
    // A link applies once the history has loaded (a retried load included).
    createEffect(on(log.loaded, loaded => loaded && openFromHash(), { defer: true }));
    void log.load();
    window.addEventListener("hashchange", openFromHash);
    onCleanup(() => window.removeEventListener("hashchange", openFromHash));
  });
  const selectTurn = (turn: SessionTurn) => {
    setHighlight(undefined);
    setFollow(false);
    setOverrides(current => new Map(current).set(turn.id, true));
    requestAnimationFrame(() => {
      const element = section?.querySelector<HTMLElement>(`[data-turn-sequence="${turn.sequence}"]`);
      element?.scrollIntoView({ block: "start" });
      element?.querySelector<HTMLElement>("button")?.focus({ preventScroll: true });
    });
  };

  return (
    <section ref={section} aria-label="Log" class={ui.container} onKeyDown={onKeyDown}>
      <div class={ui.containerHeader}>
        <div class="flex min-w-0 items-center gap-2">
          <h2 class={ui.containerTitle}>Log</h2>
          <Show when={log.ready() && !log.expired()}>
            <span class={cx(ui.containerCount, "max-[720px]:hidden")}>
              {log.timeline().length} {log.timeline().length === 1 ? "turn" : "turns"} · {log.events().length} {log.events().length === 1 ? "event" : "events"}
            </span>
          </Show>
        </div>
        <Show when={props.live}>
          <button type="button" aria-pressed={follow()} class={cx(ui.compactButton, follow() && "border-console-border-strong")} onClick={() => (follow() ? setFollow(false) : void goLatest())}>
            {follow() ? "● Following" : "○ Follow"}
          </button>
        </Show>
      </div>
      <Show when={log.timeline().length > 0}>
        <TurnTimeline turns={log.timeline()} onSelect={selectTurn} />
      </Show>
      <Show when={log.expired()}>
        <ExpiredLog timeline={log.timeline()} />
      </Show>
      <Show when={!log.expired()}>
        <div class="flex flex-wrap items-center gap-1.5 border-b border-console-border-soft px-3 py-2">
          <input
            ref={search}
            type="search"
            aria-label="Search events"
            placeholder="Search events  /"
            class="h-6 w-60 rounded-xs border border-console-border bg-console-surface px-2 font-mono text-badge text-console-text outline-none hover:border-console-border-strong focus:border-console-accent max-[720px]:w-full"
            value={query()}
            onInput={event => setQuery(event.currentTarget.value)}
          />
          <Segmented
            label="Show"
            value={view()}
            onChange={setView}
            options={[
              { value: "all", label: "All" },
              { value: "output", label: "Output" },
              { value: "problems", label: "Problems", count: problemCount() },
            ]}
          />
          <button ref={helpButton} type="button" class={cx(ui.payloadAction, "ml-auto text-console-subtle")} aria-label="Keyboard shortcuts" aria-expanded={help()} onClick={() => setHelp(!help())}>? Keys</button>
          <Popover open={help()} anchor={helpButton} onDismiss={() => setHelp(false)} title="Keyboard shortcuts" role="dialog" label="Keyboard shortcuts" align="end" width={260}>
            <dl class="m-0 grid grid-cols-[64px_minmax(0,1fr)] gap-x-3 gap-y-1 text-body">
              <For each={shortcuts}>{([key, label]) => <><dt class="font-mono text-chrome">{key}</dt><dd class="m-0 text-console-muted">{label}</dd></>}</For>
            </dl>
          </Popover>
        </div>
        <Show when={log.ready()} fallback={<LoadingRows />}>
          <Show when={log.events().length > 0} fallback={<p class="m-0 px-3 py-3.5 text-body text-console-muted">No events yet. Events appear when the first turn is queued.</p>}>
            <Show when={rows().length > 0} fallback={<p class="m-0 px-3 py-3.5 text-body text-console-muted">No events match.</p>}>
              <div role="list" aria-label="Log entries">
                <For each={rowKeys()}>
                  {key => (
                    <Switch>
                      <Match when={(() => { const row = rowMap().get(key); return row?.kind === "session" ? row : undefined; })()}>
                        {current => <div role="listitem" class="border-t border-console-border"><EventLine event={current().event} address={props.address} highlight={highlight()} /></div>}
                      </Match>
                      <Match when={(() => { const row = rowMap().get(key); return row?.kind === "turn" ? row : undefined; })()}>
                        {current => (
                          <TurnGroup
                            block={current().block}
                            events={current().events}
                            record={turnRecord(current().block)}
                            open={isOpen(current().block)}
                            filtering={filtering()}
                            turnRows={turnRows()}
                            address={props.address}
                            highlight={highlight()}
                            canInterrupt={props.canInterrupt}
                            onToggle={() => toggleTurn(current().block)}
                            onInterrupt={props.onInterruptTurn}
                          />
                        )}
                      </Match>
                    </Switch>
                  )}
                </For>
              </div>
            </Show>
          </Show>
        </Show>
        <Show when={props.live && log.ready()}>
          <div class="flex min-h-7 flex-wrap items-center gap-x-2 gap-y-0.5 border-t border-console-border-soft py-1.5 pl-[34px] pr-3 font-mono text-micro text-console-subtle" role="status">
            <span aria-hidden="true" class="size-1.5 rounded-full bg-console-success" />
            <span class="whitespace-nowrap">{follow() ? "Live · following new events" : "Live · new events appear here"}</span>
            <Show when={props.liveHint}><span class="ml-auto font-sans text-chrome text-console-muted max-[720px]:ml-0 max-[720px]:basis-full">{props.liveHint}</span></Show>
          </div>
        </Show>
        <Show when={log.error()}>
          <p class="m-0 flex items-center gap-3 border-t border-console-border-soft px-3 py-2 text-body text-console-danger" role="alert">
            Events could not be loaded. They are retried automatically.
            <button type="button" class={ui.compactButton} disabled={log.busy()} onClick={() => log.retry()}>Retry now</button>
          </p>
        </Show>
      </Show>
      <Show when={!follow() && log.appended() > 0}>
        <div class="pointer-events-none sticky bottom-3 flex justify-center">
          <button type="button" class={cx(ui.button, "pointer-events-auto h-6 min-h-6")} onClick={() => void goLatest()}>
            ↓ {log.appended()} new {log.appended() === 1 ? "event" : "events"} · Latest
          </button>
        </div>
      </Show>
    </section>
  );
}

function LoadingRows() {
  return (
    <Loading label="Loading events">
      <div class="flex flex-col gap-2.5 py-3 pl-[34px]">
        <For each={[180, 120, 210]}>{width => <span class="flex gap-2"><SkeletonBar width="80px" /><SkeletonBar width="96px" /><SkeletonBar width={`${width}px`} /></span>}</For>
      </div>
    </Loading>
  );
}

function ExpiredLog(props: { timeline: readonly SessionTurn[] }) {
  return (
    <>
      <div class="flex flex-col gap-1 border-b border-console-border-soft bg-console-bg-panel px-3 py-2.5">
        <strong class="text-body font-medium">Content expired</strong>
        <span class="text-body text-console-muted">This session's events and turn content were removed under its retention policy. Turn status and times remain.</span>
      </div>
      <For each={props.timeline}>
        {turn => (
          <div class={ui.logTurnHeader}>
            <span class={ui.logTurnButton}>
              <span />
              <span class="font-mono text-chrome font-medium">#{turn.sequence}</span>
              <TurnStatus status={turn.status} />
              <span class="truncate text-console-muted">{turn.error?.code ?? "Content expired"}</span>
            </span>
            <span class="font-mono text-micro text-console-subtle">{duration(turn.started_at, turn.terminal_at)}</span>
          </div>
        )}
      </For>
    </>
  );
}

function TurnStatus(props: { status: SessionTurn["status"] | undefined }) {
  return (
    <span class="inline-flex items-center gap-1.5 font-mono text-badge text-console-muted">
      <span aria-hidden="true" class={cx("size-1.75 rounded-full", props.status ? turnStatusColor[props.status] : "bg-console-faint")} />
      {props.status ?? "unknown"}
    </span>
  );
}

function TurnGroup(props: {
  block: TurnBlock;
  events: SessionEvent[];
  record: SessionTurn | undefined;
  open: boolean;
  filtering: boolean;
  turnRows: boolean;
  address: SessionAddress;
  highlight: number | undefined;
  canInterrupt: boolean;
  onToggle: () => void;
  onInterrupt: (turnID: string) => void;
}) {
  const status = () => props.record?.status;
  const problems = () => props.block.events.filter(event => isProblem(event.kind)).length;
  const questions = () => props.block.events.filter(event => event.kind === "ask.created").length;
  const counts = () => [
    props.filtering ? `${props.events.length} of ${props.block.events.length} shown` : `${props.block.events.length} ${props.block.events.length === 1 ? "event" : "events"}`,
    problems() ? `${problems()} ${problems() === 1 ? "problem" : "problems"}` : "",
    questions() ? `${questions()} ${questions() === 1 ? "question" : "questions"}` : "",
  ].filter(Boolean).join(" · ");
  const tint = () => status() === "failed" ? "bg-console-danger-bg/50" : questions() && status() === "running" ? "bg-console-warning-bg/60" : status() === "running" ? "bg-console-accent-soft" : "";
  return (
    <div role="listitem" id={`turn-${props.block.turnID}`} data-turn-sequence={props.block.sequence}>
      {/* A Turn with problems is itself a problem stop for e/E while it is closed. */}
      <div data-log-line data-turn-header data-problem={problems() && !props.open ? "" : undefined} class={cx(ui.logTurnHeader, tint())}>
        <button type="button" aria-expanded={props.open} class={ui.logTurnButton} onClick={() => props.onToggle()}>
          <span aria-hidden="true" class="font-mono text-micro text-console-subtle">{props.open ? "▾" : "▸"}</span>
          <span class="font-mono text-chrome font-medium">{props.block.sequence === undefined ? "Turn" : `#${props.block.sequence}`}</span>
          <TurnStatus status={status()} />
          <span class="min-w-0 truncate">{inputSummary(props.record)}</span>
        </button>
        <span class="flex shrink-0 items-center gap-2 font-mono text-micro text-console-subtle">
          <span class="max-[720px]:hidden">{counts()}</span>
          <Show when={props.record?.started_at}>{started => <span>{duration(started(), props.record?.terminal_at)}</span>}</Show>
          <Show when={props.canInterrupt && status() === "running"}>
            <button type="button" class={ui.payloadAction} onClick={() => props.onInterrupt(props.block.turnID)}>Interrupt</button>
          </Show>
        </span>
      </div>
      <Show when={props.open}>
        <Show when={props.record && props.turnRows && props.block.events.some(event => event.kind === "turn.queued")}>
          <InputLine turn={props.record!} time={props.block.events.find(event => event.kind === "turn.queued")?.created_at} />
        </Show>
        <For each={props.events}>{event => <EventLine event={event} address={props.address} highlight={props.highlight} turn={props.record} />}</For>
        <Show when={props.turnRows && props.record?.status === "completed" && !props.record.payload_expired_at}>
          <OutcomeLines turn={props.record!} />
        </Show>
        <Show when={props.turnRows && props.record?.status === "finalizing"}>
          <p class="m-0 border-t border-console-border-soft py-1.5 pl-[34px] pr-3 text-body text-console-muted min-[721px]:pl-[296px]">
            The handler has returned. Helmr is saving the required disk state before completion.
          </p>
        </Show>
      </Show>
    </div>
  );
}

// A Turn row opens the matching part of the Turn record: its id, sequence and
// that value.
function recordOfTurn(turn: SessionTurn, field: "input" | "response" | "result"): JsonValue {
  return { turn_id: turn.id, sequence: turn.sequence, [field]: (turn[field] ?? null) as JsonValue };
}

function textPins(field: string, value: unknown): ReadonlySet<string> {
  if (!Array.isArray(value)) return new Set([field]);
  return new Set(value.map((part, index) => `${field}.${index}.${part !== null && typeof part === "object" && (part as { type?: unknown }).type === "json" ? "value" : "text"}`));
}

// Turn rows have no event sequence; their time is when the Turn was queued
// (input) or settled (response and result).
function TurnLine(props: { turn: SessionTurn; field: "input" | "response" | "result"; tone: Tone; summary: string; time?: string | undefined; expired?: boolean }) {
  const [open, setOpen] = createSignal(false);
  const value = () => props.turn[props.field];
  const opened = () => new Set([props.field, `${props.field}.*`, `${props.field}.*.value`]);
  return (
    <Line chip={props.field} tone={props.tone} outline time={props.time} summary={props.summary} size={value() === undefined ? undefined : byteLength(JSON.stringify(value()))} open={open()} onToggle={props.expired ? undefined : () => setOpen(!open())}>
      <JsonView inline kind="Turn record" value={recordOfTurn(props.turn, props.field)} label={`Turn #${props.turn.sequence} ${props.field}`} open={opened()} pinned={textPins(props.field, value())} />
    </Line>
  );
}

function InputLine(props: { turn: SessionTurn; time: string | undefined }) {
  const expired = () => !!props.turn.payload_expired_at;
  return <TurnLine turn={props.turn} field="input" tone="neutral" time={props.time} expired={expired()} summary={expired() ? "content expired" : inputSummary(props.turn)} />;
}

function OutcomeLines(props: { turn: SessionTurn }) {
  return (
    <>
      <Show when={props.turn.response}>
        {content => <TurnLine turn={props.turn} field="response" tone="success" time={props.turn.terminal_at} summary={contentSummary(content())} />}
      </Show>
      <Show when={Object.hasOwn(props.turn, "result")}>
        <TurnLine turn={props.turn} field="result" tone="success" time={props.turn.terminal_at} summary={`json ${preview((props.turn.result ?? null) as JsonValue)}`} />
      </Show>
    </>
  );
}

function EventLine(props: { event: SessionEvent; address: SessionAddress; highlight: number | undefined; turn?: SessionTurn | undefined }) {
  const parsed = createMemo(() => parseEvent(props.event));
  const view = createMemo(() => recordView(props.event));
  const [open, setOpen] = createSignal(false);
  const clipboard = useClipboard();
  const error = () => (props.event.kind === "turn.failed" ? props.turn?.error?.message : undefined);
  const link = () => `${window.location.origin}${window.location.pathname}${window.location.search}#event-${props.event.sequence}`;
  createEffect(() => {
    if (props.highlight === props.event.sequence) setOpen(true);
  });
  return (
    <Line
      id={`event-${props.event.sequence}`}
      chip={props.event.kind}
      tone={eventTone(props.event.kind)}
      time={props.event.created_at}
      sequence={props.event.sequence}
      summary={error() ? `${eventSummary(props.event, parsed())} · ${error()}` : eventSummary(props.event, parsed())}
      size={payloadSize(props.event)}
      problem={isProblem(props.event.kind)}
      highlight={props.highlight === props.event.sequence}
      ask={(() => { const value = parsed(); return value.kind === "ask.created" ? value.askID : undefined; })()}
      open={open()}
      onToggle={() => setOpen(!open())}
    >
      <Show when={error()}>
        {message => <p class="m-0 mb-1 whitespace-pre-wrap break-words text-body text-console-danger"><span class="font-mono text-micro text-console-subtle">Turn record error · </span>{message()}</p>}
      </Show>
      <JsonView
        inline
        kind="record"
        value={eventRecord(props.event)}
        label={`Event ${props.event.sequence}`}
        open={view().open}
        pinned={view().pinned}
        actions={<>
          <button type="button" class={ui.payloadAction} onClick={() => void clipboard.copy(link())}>{clipboard.state() === "copied" ? "Copied" : "Copy link"}</button>
          <clipboard.LiveRegion />
          <Show when={(() => { const value = parsed(); return value.kind === "ask.created" && props.event.turn_id ? value : undefined; })()}>
            {value => <a class={ui.payloadAction} href={askConsolePath({ sessionID: props.address.sessionID, turnID: props.event.turn_id!, askID: value().askID })}>Open question</a>}
          </Show>
        </>}
      />
    </Line>
  );
}

// One 28px log line: time, sequence, kind chip, summary and payload size. Opened,
// the line and its record share one tinted band with an accent rail, so the
// record reads as the line's continuation.
function Line(props: {
  id?: string;
  chip: string;
  tone: Tone;
  outline?: boolean;
  time?: string | undefined;
  sequence?: number;
  summary: string;
  size?: number | undefined;
  problem?: boolean;
  highlight?: boolean;
  ask?: string | undefined;
  open: boolean;
  onToggle: (() => void) | undefined;
  children: JSX.Element;
}) {
  const content = () => (
    <>
      <span class="font-mono text-badge text-console-subtle max-[720px]:order-2 max-[720px]:empty:hidden">{props.time ? clockTime(props.time) : ""}</span>
      <span class="font-mono text-badge text-console-faint max-[720px]:order-3 max-[720px]:empty:hidden">{props.sequence === undefined ? "" : `#${props.sequence}`}</span>
      <span class={cx(ui.logKind, "max-[720px]:order-1", props.outline ? "border-console-border bg-console-surface text-console-muted" : toneClass[props.tone])}>{props.chip}</span>
      <span class={cx("min-w-0 truncate max-[720px]:order-4 max-[720px]:basis-full max-[720px]:whitespace-normal max-[720px]:line-clamp-2", props.problem && "text-console-danger")}>{props.summary}</span>
    </>
  );
  return (
    <div id={props.id} tabindex="-1" class={cx("outline-none focus-visible:bg-console-accent-soft", props.open && ui.logOpen)} data-log-line data-problem={props.problem ? "" : undefined} data-ask={props.ask}>
      <div class={cx(ui.logLine, "pl-[34px]", props.open && ui.logOpenRow, props.highlight && ui.logLineHighlight, props.problem && !props.highlight && !props.open && "bg-console-danger-bg/40")}>
        <Show when={props.onToggle} fallback={<div class={ui.logRowButton}>{content()}</div>}>
          {toggle => <button type="button" aria-expanded={props.open} class={ui.logRowButton} onClick={() => toggle()()}>{content()}</button>}
        </Show>
        <span class="flex items-center justify-end self-start font-mono text-micro text-console-subtle min-[721px]:self-center">
          <Show when={props.size !== undefined}>{formatBytes(props.size!)}</Show>
        </span>
      </div>
      <Show when={props.open && props.onToggle}>
        <div class="pb-2.5 pl-[34px] pr-3">{props.children}</div>
      </Show>
    </div>
  );
}
