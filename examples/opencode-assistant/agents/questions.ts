import { validateQuestion, type Question, type ChoiceAnswer, type Turn } from "@helmr/sdk"
import type { QuestionInfo } from "@opencode-ai/sdk/v2"

export async function answerQuestions(turn: Turn, questions: readonly QuestionInfo[], signal: AbortSignal): Promise<string[][]> {
  if (!questions.length) throw new Error("OpenCode returned an empty question batch")
  // Validate the whole batch before showing any question to a person.
  for (const question of questions) {
    if (!question.question?.trim() || !Array.isArray(question.options) || question.options.some(option => !option.label?.trim()) || new Set(question.options.map(option => option.label)).size !== question.options.length) {
      throw new Error("OpenCode returned an unsupported question")
    }
  }
  const mapped = questions.map((question): Question => ({
    ...(question.header ? { title: question.header } : {}),
    prompt: [{ type: "text", text: question.question }],
    ...(question.options.length ? {
      type: "choice", multiple: question.multiple ?? false, allowCustom: question.custom ?? true,
      options: question.options.map((option, index) => ({ id: `option-${index + 1}`, label: option.label, description: option.description, value: option.label })),
    } : { type: "text" }),
  }))
  for (const question of mapped) validateQuestion(question)
  const answers: string[][] = []
  for (const question of mapped) {
    signal.throwIfAborted()
    if (question.type === "text") {
      const reply = await turn.ask(question, { signal })
      answers.push([reply.answer])
    } else {
      const reply = await turn.ask(question, { signal })
      const answer = reply.answer as unknown as ChoiceAnswer
      const values = answer.selected.map(item => {
        if (typeof item.value !== "string") throw new Error("Invalid OpenCode question answer")
        return item.value
      })
      if (answer.custom !== undefined && answer.custom !== "") values.push(answer.custom)
      answers.push(values)
    }
  }
  signal.throwIfAborted()
  return answers
}
