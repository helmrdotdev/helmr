// Public authoring types distinguish text and choice answers while
// preserving authenticated responder metadata for each control.
import type { Turn } from "./agent"
import type { AnswerControl, AskResponse, ChoiceAnswer } from "./question"
export async function questionTypes(turn: Turn, control: AnswerControl) {
  const text: AskResponse<string> = await turn.ask({ prompt: [], answer: { type: "text" } })
  const choice: AskResponse<ChoiceAnswer> = await turn.ask({ prompt: [], answer: { type: "choice", options: [{ id: "one", label: "One", value: { action: "review" } }] } })
  const union: AskResponse<string | ChoiceAnswer> = await turn.ask({ prompt: [], answer: control })
  // @ts-expect-error raw question strings are not a supported control declaration.
  void turn.ask("question")
  // @ts-expect-error a choice answer is not a text answer.
  const wrong: AskResponse<string> = choice
  return { text, choice, union, wrong }
}
