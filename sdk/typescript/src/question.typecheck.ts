// Public authoring types distinguish text and choice answers while
// preserving authenticated responder metadata for each control.
import type { Turn } from "./agent"
import type { Question, AskAnswer, ChoiceAnswer } from "./question"
export async function questionTypes(turn: Turn, question: Question) {
  const text: AskAnswer<string> = await turn.ask({ prompt: [],  type: "text" })
  const choice: AskAnswer<ChoiceAnswer> = await turn.ask({ prompt: [],  type: "choice", options: [{ id: "one", label: "One", value: { action: "review" } }] })
  const union: AskAnswer<string | ChoiceAnswer> = await turn.ask(question)
  // @ts-expect-error raw question strings are not a supported control declaration.
  void turn.ask("question")
  // @ts-expect-error a choice answer is not a text answer.
  const wrong: AskAnswer<string> = choice
  return { text, choice, union, wrong }
}
