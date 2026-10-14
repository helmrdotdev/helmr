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

export async function optionalAnswerFields(turn: Turn, dynamic: boolean) {
  const options = [{ id: "one", label: "One", value: true }] as const
  const omitted = (await turn.ask({ type: "choice", prompt: "Pick", options })).answer
  const disabled = (await turn.ask({ type: "choice", prompt: "Pick", options, allowCustom: false, allowNotes: false })).answer
  const enabled = (await turn.ask({ type: "choice", prompt: "Pick", options, allowCustom: true, allowNotes: true })).answer
  const variable = (await turn.ask({ type: "choice", prompt: "Pick", options, allowCustom: dynamic, allowNotes: dynamic })).answer
  const absent: readonly undefined[] = [omitted.custom, omitted.notes, disabled.custom, disabled.notes]
  const possible: readonly (string | undefined)[] = [enabled.custom, enabled.notes, variable.custom, variable.notes]
  // @ts-expect-error an enabled custom answer is not always absent.
  const custom: undefined = enabled.custom
  // @ts-expect-error a dynamic notes flag can permit notes.
  const notes: undefined = variable.notes
  return { absent, possible, custom, notes }
}


import { validateQuestion } from "./index"
export function validatedQuestion(value: unknown): Question {
  validateQuestion(value)
  return value
}
