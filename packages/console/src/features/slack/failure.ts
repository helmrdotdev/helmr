import { ApiError } from "../../lib/api";
import { describeFailure, type Failure } from "../../lib/error";

// What Slack request failures mean to the person on the page. The server's own
// text is never the message; it stays in the technical details.
export const SLACK_MESSAGES: Readonly<Record<string, string>> = {
  service_unavailable: "Slack is not set up for this Helmr installation.",
  forbidden: "Managing this Agent’s connection requires Manage agents permission. Your access may have changed; reload the page.",
  conflict: "The Slack connection changed since this page loaded. Reload to see its current state.",
  bad_request: "Slack did not accept this request. Check the values and try again.",
  not_found: "This Slack connection or message is no longer available here.",
  rate_limited: "Too many requests. Wait a moment, then try again.",
  unprocessable_entity: "Slack did not accept these values. Check them and try again.",
};

// `messages` replaces the shared meaning of a code where the page knows better
// (a delivery conflict is about the message, not the connection).
export function slackFailure(error: unknown, fallback: string, messages: Readonly<Record<string, string>> = {}): Failure {
  return describeFailure(error, { ...SLACK_MESSAGES, ...messages }, fallback);
}

// The control plane answers every Slack request this way when it runs without
// the Slack integration.
export function slackNotConfigured(error: unknown): boolean {
  return error instanceof ApiError && error.code === "service_unavailable";
}
