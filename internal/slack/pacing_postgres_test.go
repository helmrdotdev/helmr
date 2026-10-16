package slack

import (
	"testing"
	"time"
)

func TestDeliveryPacingSeparatesChannelsStatusAndFinalUpdates(t *testing.T) {
	f := newStatusFixture(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	reserve := func(channel, method string, final, want bool) {
		t.Helper()
		got, err := reserveDelivery(t.Context(), tx, f.installation, channel, method, final)
		if err != nil || got != want {
			t.Fatalf("reserve %s/%s final=%v: %v %v", channel, method, final, got, err)
		}
	}
	reserve("C1", "chat.postMessage", false, true)
	reserve("C1", "chat.postMessage", false, false)
	reserve("C2", "chat.postMessage", false, true)
	reserve("C1", "agents.sessions.setStatus", false, true)
	reserve("C1", "agents.sessions.setStatus", false, false)
	reserve("C1", "chat.update", true, true)
	reserve("C1", "chat.stopStream", true, true)
	if err := recordMethodBackoff(t.Context(), tx, f.installation, "chat.update", time.Minute); err != nil {
		t.Fatal(err)
	}
	reserve("C1", "chat.update", true, false)
	reserve("C2", "chat.update", true, false)
	reserve("C1", "chat.stopStream", true, true)
	if err := recordMethodBackoff(t.Context(), tx, f.installation, "conversations.info", time.Minute); err != nil {
		t.Fatal(err)
	}
	reserve("C1", "chat.stopStream", true, false)
}
