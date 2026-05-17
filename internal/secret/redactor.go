package secret

import "bytes"

const redaction = "***"

type Redactor struct {
	patterns [][]byte
}

func NewRedactor(patterns ...[]byte) Redactor {
	redactor := Redactor{patterns: make([][]byte, 0, len(patterns))}
	for _, pattern := range patterns {
		if len(pattern) == 0 {
			continue
		}
		redactor.patterns = append(redactor.patterns, bytes.Clone(pattern))
	}
	return redactor
}

func (r Redactor) Empty() bool {
	return len(r.patterns) == 0
}

func (r Redactor) RedactString(input string) string {
	if r.Empty() {
		return input
	}
	return string(r.RedactBytes([]byte(input)))
}

func (r Redactor) RedactBytes(input []byte) []byte {
	if r.Empty() {
		return bytes.Clone(input)
	}
	out := make([]byte, 0, len(input))
	for index := 0; index < len(input); {
		if match := r.longestMatch(input[index:]); match > 0 {
			out = append(out, redaction...)
			index += match
			continue
		}
		out = append(out, input[index])
		index++
	}
	return out
}

func (r Redactor) longestMatch(input []byte) int {
	longest := 0
	for _, pattern := range r.patterns {
		if len(pattern) <= longest || len(pattern) > len(input) {
			continue
		}
		if bytes.HasPrefix(input, pattern) {
			longest = len(pattern)
		}
	}
	return longest
}
