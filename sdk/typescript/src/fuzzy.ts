export function levenshteinDistance(left: string, right: string): number {
  if (left === right) {
    return 0
  }
  if (left.length === 0) {
    return right.length
  }
  if (right.length === 0) {
    return left.length
  }

  const previous = new Array<number>(right.length + 1)
  const current = new Array<number>(right.length + 1)
  for (let column = 0; column <= right.length; column += 1) {
    previous[column] = column
  }

  for (let row = 1; row <= left.length; row += 1) {
    current[0] = row
    for (let column = 1; column <= right.length; column += 1) {
      const cost = left[row - 1] === right[column - 1] ? 0 : 1
      current[column] = Math.min(
        current[column - 1]! + 1,
        previous[column]! + 1,
        previous[column - 1]! + cost,
      )
    }
    previous.splice(0, previous.length, ...current)
  }

  return previous[right.length] ?? right.length
}
