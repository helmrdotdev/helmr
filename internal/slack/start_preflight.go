package slack

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/slackroute"
)

func (c *WebClient) PrepareStart(ctx context.Context, value slackroute.StartContext) (json.RawMessage, error) {
	target := value.Route.Target
	if _, err := c.verifyChannel(ctx, target.InstallationID, target.CredentialRevision, target.TeamID, target.ChannelID); err != nil {
		return nil, slackroute.ErrChannelUnavailable
	}
	// Delivery is explicitly requested, but input content is not implicitly published.
	text := "Started " + target.AgentName + "."
	if value.Input == nil {
		text = "Created a session for " + target.AgentName + "."
	}
	return json.Marshal(MessageBody{Text: quoteLiteral(text), Parse: "none", Blocks: []richBlock{{Type: "rich_text", Elements: []richElement{{Type: "rich_text_section", Elements: []textElement{{Type: "text", Text: text}}}}}}})
}
