import { batch, createEffect, createMemo, createSignal, on, onCleanup } from "solid-js";
import { ApiError } from "../../lib/api";
import {
  getSessionEvents,
  listSessionTurns,
  type Session,
  type SessionAddress,
  type SessionEvent,
  type SessionTurn,
} from "../../lib/sessions";

const eventPage = 1000;
const settled = (status: string | undefined) => status === "completed" || status === "failed" || status === "interrupted" || status === "cancelled";

// A Session's whole history: every event and every Turn record. It loads once,
// then each Session poll reads only events past the last one held and re-reads
// the Turns that can still change.
export function createSessionLog(props: { address: () => SessionAddress; session: () => Session | undefined }) {
  const [events, setEvents] = createSignal<SessionEvent[]>([]);
  const [turnList, setTurnList] = createSignal<SessionTurn[]>([]);
  const [expired, setExpired] = createSignal(false);
  const [ready, setReady] = createSignal(false);
  // The history has loaded at least once; until then polls do not read.
  const [loaded, setLoaded] = createSignal(false);
  const [busy, setBusy] = createSignal(false);
  const [error, setError] = createSignal<unknown>();
  const [appended, setAppended] = createSignal(0);
  const byID = createMemo(() => new Map(turnList().map(turn => [turn.id, turn])));

  // Operations run one at a time; a retry runs the one that failed.
  let chain = Promise.resolve();
  let failed: (() => Promise<void>) | undefined;
  const run = (operation: () => Promise<void>) => {
    chain = chain.then(async () => {
      setBusy(true);
      try {
        await operation();
        if (!failed || failed === operation) {
          failed = undefined;
          setError(undefined);
        }
      } catch (cause) {
        failed = operation;
        setError(cause);
      } finally {
        setBusy(false);
      }
    });
    return chain;
  };

  async function readEvents(after: number): Promise<SessionEvent[]> {
    const records: SessionEvent[] = [];
    let cursor = after;
    for (;;) {
      try {
        const page = await getSessionEvents(props.address(), { after: cursor, limit: eventPage });
        records.push(...page.records);
        cursor = page.next_after;
        if (!page.has_more) return records;
      } catch (cause) {
        // Retention removed the Session's history; Turn status and times remain.
        if (cause instanceof ApiError && cause.code === "cursor_expired") {
          setExpired(true);
          return records;
        }
        throw cause;
      }
    }
  }

  // Turn records from `after` on, merged over the ones already held.
  async function readTurns(after: number) {
    const loaded: SessionTurn[] = [];
    let cursor = after > 0 ? String(after) : undefined;
    do {
      const page = await listSessionTurns(props.address(), { cursor, limit: 100 });
      loaded.push(...page.turns);
      cursor = page.next_cursor;
    } while (cursor);
    setTurnList(current => [...current.filter(turn => turn.sequence <= after), ...loaded]);
  }

  const load = () => run(async () => {
    try {
      const records = await readEvents(0);
      await readTurns(0);
      batch(() => {
        setEvents(records);
        setAppended(0);
        setLoaded(true);
      });
    } finally {
      setReady(true);
    }
  });

  const sync = () => run(async () => {
    if (!loaded() || expired()) return;
    const records = await readEvents(events().at(-1)?.sequence ?? 0);
    const open = turnList().find(turn => !settled(turn.status));
    const unknown = records.some(event => event.turn_id && !byID().has(event.turn_id));
    if (open || unknown) await readTurns(open ? open.sequence - 1 : turnList().at(-1)?.sequence ?? 0);
    if (records.length) {
      batch(() => {
        setEvents(current => [...current, ...records]);
        setAppended(count => count + records.length);
      });
    }
  });

  const retry = () => (failed ? run(failed) : sync());
  // Each Session poll (only while the Session is open) brings the log up to date.
  createEffect(on(props.session, (session, previous) => {
    if (!previous || !session) return;
    if (failed) void retry();
    void sync();
  }));
  // A failed read also retries on its own, so content settles after the Session
  // closes and polling stops.
  let retries = 0;
  createEffect(on(error, failure => {
    if (!failure) {
      retries = 0;
      return;
    }
    if (retries >= 5) return;
    retries++;
    const timer = setTimeout(retry, 2_000 * retries);
    onCleanup(() => clearTimeout(timer));
  }));

  return {
    events,
    timeline: turnList,
    turnByID: (id: string) => byID().get(id),
    turnSequence: (id: string) => byID().get(id)?.sequence,
    activeTurn: createMemo(() => turnList().find(turn => turn.status === "running" || turn.status === "finalizing")),
    expired,
    ready,
    loaded,
    busy,
    error,
    appended,
    clearAppended: () => setAppended(0),
    load,
    retry,
  };
}

export type SessionLog = ReturnType<typeof createSessionLog>;
