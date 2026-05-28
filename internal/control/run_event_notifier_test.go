package control

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestRunEventNotifierUnsubscribeDoesNotCloseChannel(t *testing.T) {
	notifier := &PostgresRunEventNotifier{
		subscribers: map[string]map[chan struct{}]struct{}{},
	}
	runID := ids.ToPG(ids.New())
	events, unsubscribe := notifier.SubscribeRunEvents(context.Background(), runID)

	unsubscribe()
	notifier.publish(ids.MustFromPG(runID).String())

	select {
	case _, ok := <-events:
		if !ok {
			t.Fatal("subscription channel was closed")
		}
	default:
	}
}
