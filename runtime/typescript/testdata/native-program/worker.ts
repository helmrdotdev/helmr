import { parentPort } from "node:worker_threads"
const value: number = 42
parentPort!.postMessage(value)
