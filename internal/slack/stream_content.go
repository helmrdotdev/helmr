package slack

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// Stream text is an append-only serialization of the renderer's literal text and
// complete JSON parts. Escape Markdown punctuation instead of allowing authored
// links/mentions/formatting to become native instructions. JSON uses a fence
// longer than every run of backticks in its value. A continuation's JSON fragment
// is still literal data, never interpreted as an application status vocabulary.
func streamContent(raw []byte) (string, error) {
	var body MessageBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	var out strings.Builder
	for _, block := range body.Blocks {
		if block.Type != "rich_text" {
			continue
		}
		for _, element := range block.Elements {
			var text strings.Builder
			for _, part := range element.Elements {
				if part.Type != "text" {
					return "", errors.New("unsupported stream content")
				}
				text.WriteString(part.Text)
			}
			switch element.Type {
			case "rich_text_section":
				for _, r := range text.String() {
					if r == '\n' {
						out.WriteString("\\\n")
						continue
					}
					if r < 128 && (unicode.IsPunct(r) || unicode.IsSymbol(r)) {
						out.WriteByte('\\')
					}
					out.WriteRune(r)
				}
			case "rich_text_preformatted":
				largest, run := 0, 0
				for _, r := range text.String() {
					if r == '`' {
						run++
						if run > largest {
							largest = run
						}
					} else {
						run = 0
					}
				}
				fence := strings.Repeat("`", max(3, largest+1))
				out.WriteString("\n\n" + fence + "\n" + text.String() + "\n" + fence + "\n\n")
			default:
				return "", errors.New("unsupported stream element")
			}
		}
	}
	if out.Len() > 65536 {
		return "", errors.New("stream content exceeds message bound")
	}
	return out.String(), nil
}
