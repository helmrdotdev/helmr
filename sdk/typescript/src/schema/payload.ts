const payloadSchemaValidationErrorBrand = Symbol.for("helmr.sdk.PayloadSchemaValidationError")

export interface StandardSchemaV1<Input = unknown, Output = Input> {
  readonly "~standard": {
    readonly version: 1
    readonly vendor: string
    validate(value: unknown): StandardSchemaV1.Result<Output> | PromiseLike<StandardSchemaV1.Result<Output>>
    readonly types?: StandardSchemaV1.Types<Input, Output> | undefined
  }
}

export namespace StandardSchemaV1 {
  export interface Types<Input = unknown, Output = Input> {
    readonly input: Input
    readonly output: Output
  }

  export type Result<Output> = SuccessResult<Output> | FailureResult

  export interface SuccessResult<Output> {
    readonly value: Output
    readonly issues?: undefined
  }

  export interface FailureResult {
    readonly issues: readonly Issue[]
  }

  export interface Issue {
    readonly message: string
    readonly path?: readonly (PropertyKey | PathSegment)[]
  }

  export interface PathSegment {
    readonly key: PropertyKey
  }
}

export interface PayloadSchema<Input = unknown, Output = Input> extends StandardSchemaV1<Input, Output> {
  toJSONSchema(params?: PayloadSchemaJSONSchemaOptions): unknown
}

export interface PayloadSchemaJSONSchemaOptions {
  readonly io?: "input" | "output"
  readonly unrepresentable?: "throw" | "any"
  readonly target?: string
}

export type PayloadSchemaInput<TSchema> =
  TSchema extends PayloadSchema<infer Input, any> ? Input : unknown

export type PayloadSchemaOutput<TSchema> =
  TSchema extends PayloadSchema<any, infer Output> ? Output : unknown

export class PayloadSchemaValidationError extends Error {
  readonly issues: readonly StandardSchemaV1.Issue[]

  constructor(label: string, issues: readonly StandardSchemaV1.Issue[]) {
    super(formatPayloadSchemaValidationMessage(label, issues))
    this.name = "PayloadSchemaValidationError"
    this.issues = issues
    Object.defineProperty(this, payloadSchemaValidationErrorBrand, { value: true })
  }

  static override [Symbol.hasInstance](value: unknown): boolean {
    return (
      this === PayloadSchemaValidationError &&
      typeof value === "object" &&
      value !== null &&
      payloadSchemaValidationErrorBrand in value
    )
  }
}

export function assertPayloadSchema(value: unknown, label = "payloadSchema"): asserts value is PayloadSchema {
  if (value === undefined) {
    return
  }
  if (value === null || (typeof value !== "object" && typeof value !== "function")) {
    throw new Error(`${label} must implement the Standard Schema v1 interface`)
  }
  const standard = (value as Record<PropertyKey, unknown>)["~standard"]
  if (standard === null || typeof standard !== "object") {
    throw new Error(`${label} must implement the Standard Schema v1 interface`)
  }
  const record = standard as Record<PropertyKey, unknown>
  if (
    record["version"] !== 1 ||
    typeof record["validate"] !== "function" ||
    typeof (value as Record<PropertyKey, unknown>)["toJSONSchema"] !== "function"
  ) {
    throw new Error(`${label} must implement the payload schema interface`)
  }
}

export async function parsePayloadWithSchema<TSchema extends PayloadSchema<any, any>>(
  schema: TSchema,
  payload: unknown,
  label: string,
): Promise<PayloadSchemaOutput<TSchema>> {
  assertPayloadSchema(schema, label)
  const result = await schema["~standard"].validate(payload)
  if ("issues" in result && result.issues !== undefined) {
    throw new PayloadSchemaValidationError(label, result.issues)
  }
  return result.value as PayloadSchemaOutput<TSchema>
}

function formatPayloadSchemaValidationMessage(
  label: string,
  issues: readonly StandardSchemaV1.Issue[],
): string {
  if (issues.length === 0) {
    return `${label} failed validation`
  }
  const formattedIssues = issues.slice(0, 5).map(formatPayloadSchemaIssue)
  const suffix = issues.length > formattedIssues.length ? `; and ${issues.length - formattedIssues.length} more` : ""
  return `${label} failed validation: ${formattedIssues.join("; ")}${suffix}`
}

function formatPayloadSchemaIssue(issue: StandardSchemaV1.Issue): string {
  const path = formatPayloadSchemaIssuePath(issue.path)
  return path === "" ? issue.message : `${path}: ${issue.message}`
}

function formatPayloadSchemaIssuePath(path: readonly (PropertyKey | StandardSchemaV1.PathSegment)[] | undefined): string {
  if (path === undefined || path.length === 0) {
    return ""
  }
  let formatted = "payload"
  for (const segment of path) {
    const key = typeof segment === "object" && segment !== null && "key" in segment ? segment.key : segment
    if (typeof key === "string" && /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(key)) {
      formatted += `.${key}`
    } else if (typeof key === "string" && isArrayIndexKey(key)) {
      formatted += `[${key}]`
    } else if (typeof key === "number") {
      formatted += `[${key}]`
    } else {
      formatted += `[${JSON.stringify(String(key))}]`
    }
  }
  return formatted
}

function isArrayIndexKey(value: string): boolean {
  if (!/^(0|[1-9]\d*)$/.test(value)) {
    return false
  }
  const parsed = Number(value)
  return Number.isSafeInteger(parsed)
}
