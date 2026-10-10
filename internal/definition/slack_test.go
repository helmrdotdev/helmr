package definition

import "testing"

func TestSlackChannelReferenceRejectsMalformedIntent(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"channelId":""}`, `{"channelId":"00000000-0000-0000-0000-000000000000"}`, `{"channelId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35"}`, `{"channelId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc35","name":"mutable"}`} {
		if _, err := SlackChannelReference([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if id, err := SlackChannelReference(nil); err != nil || id != nil {
		t.Fatalf("omission: %v %v", id, err)
	}
	if id, err := SlackChannelReference([]byte(`{"channelId":"C123"}`)); err != nil || id == nil {
		t.Fatalf("literal: %v %v", id, err)
	}
}
