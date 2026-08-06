import { resourceID } from "./id"

interface DefinitionQuery {
  readonly deploymentId?: string
}

interface DefinitionListQuery extends DefinitionQuery {
  readonly cursor?: string
  readonly limit?: number
}

export function definitionItemQuery(
  queryInput: DefinitionQuery,
  label: string,
): string {
  if ("cursor" in queryInput || "limit" in queryInput) {
    throw new Error(`${label} item query does not accept cursor or limit`)
  }
  const query = new URLSearchParams()
  if (queryInput.deploymentId !== undefined) {
    query.set("deployment_id", resourceID(queryInput.deploymentId, "Deployment ID"))
  }
  return query.size === 0 ? "" : `?${query.toString()}`
}

export function definitionListQuery(
  queryInput: DefinitionListQuery,
  label: string,
): string {
  const query = new URLSearchParams()
  if (queryInput.deploymentId !== undefined) {
    query.set("deployment_id", resourceID(queryInput.deploymentId, "Deployment ID"))
  }
  if (queryInput.cursor !== undefined) {
    if (queryInput.cursor.length === 0) throw new Error(`${label} cursor is required`)
    query.set("cursor", queryInput.cursor)
  }
  if (queryInput.limit !== undefined) {
    if (!Number.isInteger(queryInput.limit) || queryInput.limit < 1 || queryInput.limit > 100) {
      throw new Error(`${label} limit must be an integer in [1,100]`)
    }
    query.set("limit", queryInput.limit.toString())
  }
  return query.size === 0 ? "" : `?${query.toString()}`
}
