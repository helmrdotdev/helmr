// Package slack owns Slack presentation and transport; core execution remains in agent.
package slack

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/conversation"
)

// Leave room for identity and exact controls below Slack's message limits. These
// are renderer batch limits, never smaller limits on accepted authored Content.
const contentCharacters = 3000
const contentElements = 40

type textElement struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type richElement struct {
	Type     string        `json:"type"`
	Elements []textElement `json:"elements"`
}
type richBlock struct {
	Type     string        `json:"type"`
	Elements []richElement `json:"elements"`
}

// MessageBody intentionally contains no destination, identity or action authority.
// Text is a complete accessible fallback. Both forms treat authored mentions and
// links as literal text, with unfurls and broadcast explicitly disabled.
type MessageBody struct {
	Text           string      `json:"text"`
	Blocks         []richBlock `json:"blocks"`
	Markdown       bool        `json:"mrkdwn"`
	Parse          string      `json:"parse"`
	LinkNames      bool        `json:"link_names"`
	UnfurlLinks    bool        `json:"unfurl_links"`
	UnfurlMedia    bool        `json:"unfurl_media"`
	ReplyBroadcast bool        `json:"reply_broadcast"`
}

type ContentSource struct {
	Sequence int64           `json:"sequence"`
	Content  json.RawMessage `json:"content"`
}

// Offsets count UTF-8 bytes in decoded text and canonical JSON values, concatenated
// in part order within one event. Part types/boundaries remain in the frozen source.
// SourceDigest covers the canonical events intersecting the range and its bounds.
type ContentPost struct {
	Body                                               MessageBody
	StartSequence, StartOffset, EndSequence, EndOffset int64
	SourceDigest                                       [32]byte
}

// RenderContent preserves adjacent text across write boundaries and treats each
// JSON part as a distinct preformatted data block. Continuations preserve every
// byte on UTF-8 boundaries; none is truncated to fit a message. Empty progress
// produces no content, while an explicitly empty committed response has a label.
func RenderContent(sources []ContentSource, emptyCompletion bool) ([]ContentPost, error) {
	canonical := make([]ContentSource, len(sources))
	for i, source := range sources {
		if source.Sequence < 1 || (i > 0 && source.Sequence <= sources[i-1].Sequence) {
			return nil, fmt.Errorf("content source sequence is not increasing")
		}
		value, err := conversation.Content(source.Content, false)
		if err != nil {
			return nil, err
		}
		canonical[i] = ContentSource{source.Sequence, value}
	}
	emptyLabel := ""
	if emptyCompletion {
		emptyLabel = "Completed."
	}
	return renderCanonicalContent(canonical, emptyLabel)
}

// Authored content is validated before presentation-only decoration reaches here.
func renderCanonicalContent(canonical []ContentSource, emptyLabel string) ([]ContentPost, error) {
	var posts []ContentPost
	current := ContentPost{Body: MessageBody{Parse: "none", Blocks: []richBlock{{Type: "rich_text"}}}}
	characters := 0
	flush := func() {
		if len(current.Body.Blocks[0].Elements) == 0 {
			return
		}
		posts = append(posts, current)
		current = ContentPost{Body: MessageBody{Parse: "none", Blocks: []richBlock{{Type: "rich_text"}}}}
		characters = 0
	}
	for _, source := range canonical {
		var parts []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(source.Content, &parts); err != nil {
			return nil, err
		}
		var offset int64
		for _, part := range parts {
			text, kind := part.Text, "rich_text_section"
			if part.Type == "json" {
				text, kind = string(part.Value), "rich_text_preformatted"
			}
			partStarted := false
			for text != "" {
				elements := current.Body.Blocks[0].Elements
				merge := len(elements) > 0 && elements[len(elements)-1].Type == kind && (kind == "rich_text_section" || partStarted)
				separator := ""
				if len(elements) > 0 && !merge {
					separator = "\n"
				}
				if characters+len(separator) >= contentCharacters || (!merge && len(elements) >= contentElements) {
					flush()
					partStarted = false
					continue
				}
				end, cost := literalPrefix(text, contentCharacters-characters-len(separator))
				if end == 0 {
					flush()
					partStarted = false
					continue
				}
				piece := text[:end]
				if current.StartSequence == 0 {
					current.StartSequence = source.Sequence
					current.StartOffset = offset
				}
				if merge {
					elements[len(elements)-1].Elements[0].Text += piece
				} else {
					elements = append(elements, richElement{Type: kind, Elements: []textElement{{Type: "text", Text: piece}}})
				}
				current.Body.Blocks[0].Elements = elements
				current.Body.Text += separator + quoteLiteral(piece)
				characters += len(separator) + cost
				offset += int64(end)
				current.EndSequence = source.Sequence
				current.EndOffset = offset
				text = text[end:]
				partStarted = true
			}
		}
	}
	flush()
	if len(posts) == 0 && emptyLabel != "" && len(canonical) > 0 {
		sequence := canonical[len(canonical)-1].Sequence
		posts = []ContentPost{{Body: MessageBody{Text: emptyLabel, Parse: "none", Blocks: []richBlock{{Type: "rich_text", Elements: []richElement{{Type: "rich_text_section", Elements: []textElement{{Type: "text", Text: emptyLabel}}}}}}}, StartSequence: sequence, EndSequence: sequence}}
	}
	for i := range posts {
		post := &posts[i]
		var selected []ContentSource
		for _, source := range canonical {
			if source.Sequence >= post.StartSequence && source.Sequence <= post.EndSequence {
				selected = append(selected, source)
			}
		}
		frozen, err := json.Marshal(struct {
			Sources    []ContentSource
			Start, End int64
		}{selected, post.StartOffset, post.EndOffset})
		if err != nil {
			return nil, err
		}
		post.SourceDigest = sha256.Sum256(frozen)
	}
	return posts, nil
}

func quoteLiteral(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

func literalPrefix(text string, budget int) (int, int) {
	end, cost := 0, 0
	for at, r := range text {
		n := 1
		switch r {
		case '&':
			n = 5
		case '<', '>':
			n = 4
		}
		if cost+n > budget {
			break
		}
		cost += n
		end = at + utf8.RuneLen(r)
	}
	return end, cost
}
