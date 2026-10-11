import type { ChoiceAnswer, Turn } from "@helmr/sdk"
import type { QuestionInfo } from "@opencode-ai/sdk/v2"

export async function answerQuestions(turn: Turn, questions: readonly QuestionInfo[], signal: AbortSignal): Promise<string[][]> {
  if (!questions.length) throw new Error("OpenCode returned an empty question batch")
  // Validate the whole batch before showing any question to a person.
  for (const question of questions) {
    if (!question.question?.trim() || !Array.isArray(question.options) || question.options.some(option => !option.label?.trim()) || new Set(question.options.map(option => option.label)).size !== question.options.length) {
      throw new Error("OpenCode returned an unsupported question")
    }
  }
  const answers: string[][] = []
  for (const question of questions) {
    signal.throwIfAborted()
    if (!question.options.length) {
      const reply = await turn.ask({ prompt: [{ type: "text", text: question.question }], answer: { type: "text" } }, { signal })
      answers.push([reply.answer])
    } else {
      const reply = await turn.ask({
        prompt: [{ type: "text", text: question.question }],
        answer: {
          type: "choice", multiple: question.multiple ?? false, allowText: question.custom ?? true,
          options: question.options.map((option, index) => ({ id: `option-${index + 1}`, label: option.label, description: option.description, value: option.label })),
        },
      }, { signal })
      const answer = reply.answer as unknown as ChoiceAnswer
      const values = answer.selected.map(item => {
        if (typeof item.value !== "string") throw new Error("Invalid OpenCode question answer")
        return item.value
      })
      if (answer.text !== undefined && answer.text !== "") values.push(answer.text)
      answers.push(values)
    }
  }
  signal.throwIfAborted()
  return answers
}
