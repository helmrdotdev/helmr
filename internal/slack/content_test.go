package slack

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestContentPreservesWritesAndAtomicDataBoundaries(t *testing.T) {
	posts, err := RenderContent([]ContentSource{
		{1, json.RawMessage(`[{"type":"text","text":"hel"}]`)},
		{3, json.RawMessage(`[{"type":"text","text":"lo"},{"type":"json","value":{"b":2,"a":1}},{"type":"json","value":false},{"type":"text","text":"after"}]`)},
		{4, json.RawMessage(`[{"type":"text","text":"ward"}]`)},
	}, false)
	if err != nil || len(posts) != 1 {
		t.Fatalf("render: %+v %v", posts, err)
	}
	post := posts[0]
	elements := post.Body.Blocks[0].Elements
	if len(elements) != 4 || elements[0].Elements[0].Text != "hello" || elements[1].Type != "rich_text_preformatted" || elements[1].Elements[0].Text != `{"a":1,"b":2}` || elements[2].Elements[0].Text != "false" || elements[3].Elements[0].Text != "afterward" {
		t.Fatalf("framing changed: %+v", elements)
	}
	if post.Body.Text != "hello\n{\"a\":1,\"b\":2}\nfalse\nafterward" || post.StartSequence != 1 || post.StartOffset != 0 || post.EndSequence != 4 || post.EndOffset != 4 {
		t.Fatalf("fallback/range: %+v", post)
	}
}

func TestContentContinuationsPreserveLongUTF8AndLiteralControls(t *testing.T) {
	text := strings.Repeat("雪😀<&>@here<@U123>", 1400)
	value := strings.Repeat("data```<&>雪", 1800)
	raw, _ := json.Marshal([]any{map[string]any{"type": "text", "text": text}, map[string]any{"type": "json", "value": value}})
	posts, err := RenderContent([]ContentSource{{9, raw}}, false)
	if err != nil || len(posts) < 2 {
		t.Fatalf("long rendering: %d %v", len(posts), err)
	}
	var got strings.Builder
	var end int64
	for _, post := range posts {
		if post.StartSequence != 9 || post.EndSequence != 9 || post.StartOffset != end || post.EndOffset <= end {
			t.Fatalf("lost range: %+v", post)
		}
		end = post.EndOffset
		if utf8.RuneCountInString(post.Body.Text) > contentCharacters || strings.Contains(post.Body.Text, "<@") || post.Body.Markdown || post.Body.LinkNames || post.Body.UnfurlLinks || post.Body.UnfurlMedia || post.Body.ReplyBroadcast || post.Body.Parse != "none" {
			t.Fatal("unsafe or oversized fallback")
		}
		for _, block := range post.Body.Blocks {
			for _, element := range block.Elements {
				for _, leaf := range element.Elements {
					if leaf.Type != "text" || !utf8.ValidString(leaf.Text) {
						t.Fatal("nonliteral or broken UTF8")
					}
					got.WriteString(leaf.Text)
				}
			}
		}
	}
	// The JSON value uses its complete canonical serialization; code-fence-like
	// content remains text in a preformatted element and cannot terminate it.
	wantJSON := `"` + value + `"`
	if got.String() != text+wantJSON || end != int64(len(text+wantJSON)) {
		t.Fatal("long content truncated or changed")
	}
	retry, err := RenderContent([]ContentSource{{9, raw}}, false)
	if err != nil || !reflect.DeepEqual(posts, retry) {
		t.Fatal("retry rerender changed bytes/ranges")
	}
}

func TestContentEmptyResponseAndAdmission(t *testing.T) {
	source := []ContentSource{{1, json.RawMessage(`[{"type":"text","text":""}]`)}}
	if posts, err := RenderContent(source, false); err != nil || len(posts) != 0 {
		t.Fatalf("empty progress: %+v %v", posts, err)
	}
	if posts, err := RenderContent(source, true); err != nil || len(posts) != 1 || posts[0].Body.Text != "Completed." {
		t.Fatalf("empty response: %+v %v", posts, err)
	}
	if _, err := RenderContent([]ContentSource{{1, json.RawMessage(`[{"type":"file","path":"private"}]`)}}, false); err == nil {
		t.Fatal("unqualified file rendered")
	}
	if _, err := RenderContent([]ContentSource{{2, json.RawMessage(`[]`)}, {1, json.RawMessage(`[]`)}}, false); err == nil {
		t.Fatal("out of order source accepted")
	}
	a, err := RenderContent([]ContentSource{{1, json.RawMessage(`[{"type":"json","value":{"a":1,"b":2}}]`)}}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderContent([]ContentSource{{1, json.RawMessage(`[{"value":{"b":2.0,"a":1.0},"type":"json"}]`)}}, false)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("canonical replay changed")
	}
}
