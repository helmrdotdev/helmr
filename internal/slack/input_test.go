package slack

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/conversation"
)

func TestSlackMessageInputPreservesCodeQuotesAndLinks(t *testing.T) {
	for _, sample := range []struct{ source, want string }{
		{"<@bot> hello", " hello"},
		{"hello <@bot> then <@other>", "hello  then <@other>"},
		{"explain `<@bot>` then <@bot>", "explain `<@bot>` then "},
		{"```\n<@bot>\n```\n<@bot> answer", "```\n<@bot>\n```\n answer"},
		{"> <@bot> quoted\n<@bot> answer", "> <@bot> quoted\n answer"},
		{">>> <@bot> quoted\n<@bot> also quoted", ">>> <@bot> quoted\n<@bot> also quoted"},
		{"<https://example.com|link> <@bot> *format*", "<https://example.com|link>  *format*"},
		{"&lt;@bot&gt; <@bot>", "<@bot> "},
		{"use `a &lt; b &amp;&amp; c &gt; d`", "use `a < b && c > d`"},
		{"&amp;lt;@bot&amp;gt; &quot;", "&lt;@bot&gt; &quot;"},
	} {
		t.Run(sample.source, func(t *testing.T) {
			raw, err := messageInput(messageGesture{Text: sample.source}, "bot")
			if err != nil {
				t.Fatal(err)
			}
			var value []struct{ Type, Text string }
			if err = json.Unmarshal(raw, &value); err != nil || len(value) != 1 || value[0].Type != "text" || value[0].Text != sample.want {
				t.Fatalf("input %s: %v", raw, err)
			}
		})
	}
}

func TestSlackMessageInputRejectsUnsupportedFilesAndOversizedText(t *testing.T) {
	for _, sample := range []messageGesture{{Text: "keep", UnsupportedFiles: true}, {Text: "keep", FileIDs: []string{"F1"}}} {
		if _, err := messageInput(sample, "bot"); !errors.Is(err, conversation.ErrUnsupported) {
			t.Fatalf("file silently discarded: %v", err)
		}
	}
	if _, err := messageInput(messageGesture{Text: strings.Repeat("x", conversation.ContentBytes)}, "bot"); !errors.Is(err, conversation.ErrLimit) {
		t.Fatalf("oversized input truncated or accepted: %v", err)
	}
}
