package definition

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

// SlackChannelReference validates an actual Slack channel identifier offline.
func SlackChannelReference(raw json.RawMessage) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	var value struct {
		ChannelID string `json:"channelId"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, errors.New("slack destination must contain only channelId")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("invalid Slack destination")
	}
	if !ValidSlackChannelID(value.ChannelID) {
		return nil, errors.New("slack channelId must be a Slack channel ID")
	}
	return &value.ChannelID, nil
}

var slackChannelPattern = regexp.MustCompile(`^[CG][A-Z0-9]{1,99}$`)

func ValidSlackChannelID(value string) bool { return slackChannelPattern.MatchString(value) }
