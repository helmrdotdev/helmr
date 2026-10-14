import { expect, test } from "bun:test";

import { COPY_FEEDBACK_MS, createCopyFeedback, type CopyState } from "./clipboard";

function harness(write: (text: string) => Promise<void>) {
  const states: CopyState[] = [];
  const timers = new Map<number, { run: () => void; ms: number }>();
  let next = 0;
  const feedback = createCopyFeedback({
    write,
    onState: (state) => states.push(state),
    schedule: (run, ms) => {
      timers.set(++next, { run, ms });
      return next;
    },
    cancel: (timer) => timers.delete(timer as number),
  });
  const fire = () => {
    for (const [id, timer] of [...timers]) {
      timers.delete(id);
      timer.run();
    }
  };
  return { states, timers, feedback, fire };
}

test("a successful copy shows copied, then returns to idle", async () => {
  const written: string[] = [];
  const { states, timers, feedback, fire } = harness(async (text) => { written.push(text); });
  expect(await feedback.copy("abc")).toBe("copied");
  expect(written).toEqual(["abc"]);
  expect([...timers.values()].map((timer) => timer.ms)).toEqual([COPY_FEEDBACK_MS.copied]);
  fire();
  expect(states).toEqual(["copied", "idle"]);
});

test("a failed write shows failed for longer", async () => {
  const { states, timers, feedback } = harness(() => Promise.reject(new Error("denied")));
  expect(await feedback.copy("abc")).toBe("failed");
  expect(states).toEqual(["failed"]);
  expect([...timers.values()][0]?.ms).toBe(COPY_FEEDBACK_MS.failed);
});

test("a second copy replaces the first timer", async () => {
  const { states, timers, feedback, fire } = harness(async () => {});
  await feedback.copy("a");
  await feedback.copy("b");
  expect(timers.size).toBe(1);
  fire();
  expect(states).toEqual(["copied", "copied", "idle"]);
});

test("dispose cancels the pending reset", async () => {
  const { states, timers, feedback } = harness(async () => {});
  await feedback.copy("a");
  feedback.dispose();
  expect(timers.size).toBe(0);
  expect(states).toEqual(["copied"]);
});
