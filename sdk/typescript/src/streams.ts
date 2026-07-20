import { stream } from "./definitions"

export const streams = Object.freeze({
  define: stream,
})

export type {
  RunStreamData,
  RunStreamDefinition,
  RunStreamInput,
  RunStreamRecord,
  TypedRunStreamDefinition,
} from "./contract"
