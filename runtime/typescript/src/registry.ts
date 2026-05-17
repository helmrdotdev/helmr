import { levenshteinDistance } from "@helmr/sdk/internal/fuzzy"

import type { RegisteredTask } from "./config"

export class TaskNotFoundError extends Error {
  readonly taskId: string
  readonly available: readonly string[]
  readonly suggestion: string | null

  constructor(taskId: string, available: readonly string[], suggestion: string | null) {
    super(formatMissingTaskMessage(taskId, available, suggestion))
    this.name = "TaskNotFoundError"
    this.taskId = taskId
    this.available = available
    this.suggestion = suggestion
  }
}

export function lookupRegisteredTask(
  registry: ReadonlyMap<string, RegisteredTask>,
  taskId: string,
): RegisteredTask {
  const task = registry.get(taskId)
  if (task) {
    return task
  }
  const available = [...registry.keys()].sort()
  throw new TaskNotFoundError(taskId, available, closestTaskId(taskId, available))
}

function closestTaskId(taskId: string, available: readonly string[]): string | null {
  let bestId: string | null = null
  let bestDistance = Number.POSITIVE_INFINITY

  for (const candidate of available) {
    const distance = levenshteinDistance(taskId, candidate)
    if (distance < bestDistance) {
      bestId = candidate
      bestDistance = distance
    }
  }

  if (bestId === null) {
    return null
  }
  const threshold = Math.min(
    Math.max(2, Math.ceil(Math.max(taskId.length, bestId.length) * 0.34)),
    Math.floor(Math.min(taskId.length, bestId.length) / 2),
  )
  return bestDistance <= threshold ? bestId : null
}

function formatMissingTaskMessage(
  taskId: string,
  available: readonly string[],
  suggestion: string | null,
): string {
  const hint =
    suggestion === null ? "" : ` (did you mean "${suggestion}"?)`
  const availableLine =
    available.length === 0 ? "available: (none)" : `available: ${available.join(", ")}`
  return `task "${taskId}" not found${hint}\n${availableLine}`
}
