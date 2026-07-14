export type PendingWaitFormKind = "approval" | "message" | "json";

export type PendingWaitForm = {
  kind: PendingWaitFormKind;
};

function objectValue(value: unknown): Record<string, unknown> | null {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return null;
  return value as Record<string, unknown>;
}

export function pendingWaitForm(metadata: unknown): PendingWaitForm {
  const form = objectValue(objectValue(metadata)?.["form"]);
  if (form?.["version"] !== 1) return { kind: "json" };
  const type = form["type"];
  if (type === "approval" || type === "message" || type === "json") {
    return { kind: type };
  }
  return { kind: "json" };
}

export function pendingTokenID(params: unknown): string | null {
  const tokenID = objectValue(params)?.["token_id"];
  return typeof tokenID === "string" && tokenID.trim() !== "" ? tokenID : null;
}

export function parsePendingWaitCompletion(kind: PendingWaitFormKind, input: string, approved?: boolean): unknown {
  if (kind === "approval") {
    if (approved === undefined) throw new Error("Choose approve or reject.");
    return { approved };
  }
  if (kind === "message") {
    if (input.trim() === "") throw new Error("Message is required.");
    return { text: input };
  }
  try {
    return JSON.parse(input);
  } catch {
    throw new Error("Enter valid JSON.");
  }
}

export function pendingWaitCompletionErrorMessage(error: unknown): string {
  if (error !== null && typeof error === "object" && "status" in error && error.status === 409) {
    return "This wait was already completed with different data. Nothing was changed.";
  }
  if (error instanceof Error && error.message.trim() !== "") return error.message;
  return "Could not complete this wait.";
}
