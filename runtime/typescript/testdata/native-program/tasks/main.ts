import { task, actor } from "@helmr/sdk"
import { state, again } from "mixed"
import sqlite3 from "sqlite3"
import { Worker } from "node:worker_threads"
import { fork } from "node:child_process"

export const deploy = task({
  id: "deploy",
  run: async () => {
    const database = new sqlite3.Database(":memory:")
    const native = await new Promise<number>((resolve, reject) => database.get("SELECT 42 AS answer", (error, row: { answer: number }) => error ? reject(error) : resolve(row.answer)))
    await new Promise<void>((resolve, reject) => database.close(error => error ? reject(error) : resolve()))
    const worker = await new Promise<number>((resolve, reject) => {
      const child = new Worker(new URL("../worker.ts", import.meta.url))
      child.once("message", resolve)
      child.once("error", reject)
    })
    const child = fork(new URL("../child.ts", import.meta.url))
    const forked = await new Promise<number>((resolve, reject) => {
      child.once("message", value => resolve(Number(value)))
      child.once("error", reject)
    })
    await new Promise<void>((resolve, reject) => child.once("exit", code => code === 0 ? resolve() : reject(new Error(`child exit ${code}`))))
    return { asset: state.asset, native, same: state === (await again()).state, worker, fork: forked }
  },
})
export const worker = actor({ id: "worker", run: async () => {
  if (state !== (await again()).state || state.asset !== "installed") throw new Error("Actor module identity changed")
} })
