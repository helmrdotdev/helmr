package dbnotify

import "testing"

func TestHintsAreScopedCoalescedAndReconnectWakesAll(t *testing.T) {
	h := New()
	a, stopA := h.Subscribe("session:a")
	b, stopB := h.Subscribe("session:b")
	defer stopB()
	h.publish("session:a", false)
	h.publish("session:a", false)
	select {
	case <-a:
	default:
		t.Fatal("missing matching hint")
	}
	select {
	case <-a:
		t.Fatal("hints did not coalesce")
	default:
	}
	select {
	case <-b:
		t.Fatal("cross-session hint")
	default:
	}
	stopA()
	h.publish("", true)
	select {
	case <-a:
		t.Fatal("unsubscribed listener woke")
	default:
	}
	select {
	case <-b:
	default:
		t.Fatal("reconnect did not wake subscriber")
	}
}
