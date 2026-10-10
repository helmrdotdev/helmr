package slack

import (
	"encoding/json"
	"strings"

	"github.com/helmrdotdev/helmr/internal/conversation"
)

var slackTextEntities = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">")

// messageInput freezes only the submitted message. Slack files are not silently
// discarded, and neither channel history nor human identity is injected into it.
func messageInput(message messageGesture, bot string) (json.RawMessage, error) {
	if message.UnsupportedFiles || len(message.FileIDs) > 0 {
		return nil, conversation.ErrUnsupported
	}
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": slackTextEntities.Replace(stripRoutingMention(message.Text, bot))}})
	return conversation.Input(content)
}

// Slack's text contains explicit user tokens and mrkdwn delimiters. Preserve
// quoted lines, code spans and link tokens verbatim; only the bot's unquoted user
// token is routing syntax. Escaped user-token examples remain ordinary text.
func stripRoutingMention(text, bot string) string {
	if bot == "" {
		return text
	}
	token := "<@" + bot + ">"
	var result strings.Builder
	lineStart := true
	for i := 0; i < len(text); {
		if lineStart {
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				end = len(text) - i
			}
			line := text[i : i+end]
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), ">>>") {
				result.WriteString(text[i:])
				break
			}
			if strings.HasPrefix(strings.TrimLeft(line, " \t"), ">") {
				result.WriteString(line)
				i += end
				lineStart = false
				continue
			}
		}
		lineStart = false
		if text[i] == '`' {
			delimiter := "`"
			if strings.HasPrefix(text[i:], "```") {
				delimiter = "```"
			}
			start := i + len(delimiter)
			if end := strings.Index(text[start:], delimiter); end >= 0 && (delimiter == "```" || !strings.Contains(text[start:start+end], "\n")) {
				end += start + len(delimiter)
				result.WriteString(text[i:end])
				i = end
				continue
			}
		}
		if text[i] == '<' {
			if end := strings.IndexByte(text[i:], '>'); end >= 0 {
				end += i + 1
				if text[i:end] != token {
					result.WriteString(text[i:end])
				}
				i = end
				continue
			}
		}
		result.WriteByte(text[i])
		lineStart = text[i] == '\n'
		i++
	}
	return result.String()
}
