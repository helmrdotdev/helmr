package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func (c *WebClient) PrepareStart(ctx context.Context, value agent.SlackStartContext) (json.RawMessage, error) {
	target := value.Route.Target
	if _, err := c.verifyChannel(ctx, target.InstallationID, target.CredentialRevision, target.TeamID, target.ChannelID); err != nil {
		return nil, agent.ErrSlackChannelUnavailable
	}
	if value.Route.Source == nil {
		// External and scheduled starts never publish their input implicitly.
		text := "Started " + target.AgentName + "."
		return json.Marshal(MessageBody{Text: quoteLiteral(text), Parse: "none", Blocks: []richBlock{{Type: "rich_text", Elements: []richElement{{Type: "rich_text_section", Elements: []textElement{{Type: "text", Text: text}}}}}}})
	}
	source := value.Route.Source
	if source.Connection.InstallationID != target.InstallationID {
		if _, err := c.verifyChannel(ctx, source.Connection.InstallationID, source.Connection.CredentialRevision, source.Connection.TeamID, source.Connection.ChannelID); err != nil {
			return nil, agent.ErrSlackChannelUnavailable
		}
	}
	permalink, err := c.sourcePermalink(ctx, *source)
	if err != nil {
		return nil, err
	}
	input, err := conversation.Input(value.Input)
	if err != nil {
		return nil, err
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err = json.Unmarshal(input, &parts); err != nil {
		return nil, err
	}
	var task strings.Builder
	for _, part := range parts {
		task.WriteString(part.Text)
	}
	prefix := "Request from " + source.Connection.AgentName + " to "
	text := prefix + target.AgentName + "\n" + task.String() + "\nSource conversation: " + permalink
	if utf8.RuneCountInString(quoteLiteral(text)) > contentCharacters {
		return nil, agent.ErrStartMessageTooLarge
	}
	elements := []any{map[string]any{"type": "text", "text": prefix}, map[string]any{"type": "user", "user_id": target.BotUserID}, map[string]any{"type": "text", "text": "\n" + task.String() + "\n"}, map[string]any{"type": "link", "url": permalink, "text": "Source conversation"}}
	return json.Marshal(map[string]any{"text": quoteLiteral(text), "blocks": []any{map[string]any{"type": "rich_text", "elements": []any{map[string]any{"type": "rich_text_section", "elements": elements}}}}, "mrkdwn": false, "parse": "none", "link_names": false, "unfurl_links": false, "unfurl_media": false, "reply_broadcast": false})
}

func (c *WebClient) sourcePermalink(ctx context.Context, source agent.SlackStartSource) (string, error) {
	token, err := c.credentials.BotToken(ctx, source.Connection.InstallationID, source.Connection.CredentialRevision)
	if err != nil || token == "" {
		return "", agent.ErrSourceConversationUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://slack.com/api/chat.getPermalink?"+url.Values{"channel": {source.Connection.ChannelID}, "message_ts": {source.ThreadTS}}.Encode(), nil)
	if err != nil {
		return "", agent.ErrSourceConversationUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return "", agent.ErrSlackChannelUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", agent.ErrSlackChannelUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 {
		return "", agent.ErrSlackChannelUnavailable
	}
	raw, err = jsoncanon.Transform(raw)
	if err != nil {
		return "", agent.ErrSlackChannelUnavailable
	}
	var result struct {
		OK        bool   `json:"ok"`
		Permalink string `json:"permalink"`
	}
	if json.Unmarshal(raw, &result) != nil || !result.OK {
		return "", agent.ErrSourceConversationUnavailable
	}
	parsed, err := url.Parse(result.Permalink)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || !strings.HasSuffix(parsed.Hostname(), ".slack.com") || !strings.HasPrefix(parsed.Path, "/archives/"+source.Connection.ChannelID+"/") {
		return "", agent.ErrSourceConversationUnavailable
	}
	return result.Permalink, nil
}
