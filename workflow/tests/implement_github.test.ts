import { afterEach, expect, test } from "bun:test"

import { createOrFindPullRequest } from "../tasks/integrations/github"
import type { Input } from "../tasks/integrations/types"

const originalFetch = globalThis.fetch

afterEach(() => {
  globalThis.fetch = originalFetch
})

test("createOrFindPullRequest limits the matching PR lookup to one result", async () => {
  let requestedUrl = ""
  globalThis.fetch = (async (input: RequestInfo | URL, _init?: RequestInit) => {
    requestedUrl = String(input)
    return Response.json([{ html_url: "https://github.com/helmrdotdev/helmr/pull/1", number: 1 }])
  }) as typeof fetch

  const pr = await createOrFindPullRequest(
    "token",
    {
      kind: "github",
      repository: "helmrdotdev/helmr",
      requestedRef: "main",
      resolvedSha: "0123456789abcdef0123456789abcdef01234567",
      refKind: "branch",
      refName: "main",
    },
    input(),
    "feature",
  )

  expect(pr.number).toBe(1)
  expect(new URL(requestedUrl).searchParams.get("per_page")).toBe("1")
})

function input(): Input {
  return {
    featureDesign: "Implement feature",
    prTitle: "Implement feature",
    prBody: "Body",
    maxReviewRounds: 1,
    operatorInput: false,
    operatorInputTimeout: 60,
    maxOperatorQuestionsPerPhase: 0,
    cursorModel: "cursor-model",
  }
}
