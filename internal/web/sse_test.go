package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHubDeliversToEverySubscriber(t *testing.T) {
	h := NewHub()
	a, closeA := h.Subscribe()
	b, closeB := h.Subscribe()
	defer closeA()
	defer closeB()

	h.Broadcast(7)

	for name, ch := range map[string]<-chan int64{"a": a, "b": b} {
		select {
		case got := <-ch:
			if got != 7 {
				t.Errorf("%s received %d, want 7", name, got)
			}
		case <-time.After(time.Second):
			t.Errorf("%s received nothing", name)
		}
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, stop := h.Subscribe()
	stop()
	h.Broadcast(1)

	select {
	case _, open := <-ch:
		if open {
			t.Error("received an event after unsubscribing")
		}
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHubBroadcastNeverBlocksOnASlowSubscriber(t *testing.T) {
	h := NewHub()
	_, stop := h.Subscribe() // never drained
	defer stop()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			h.Broadcast(int64(i))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast blocked on a subscriber that is not reading")
	}
}

func TestEventsEndpointStreamsChangeEvents(t *testing.T) {
	s, _ := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.routes().ServeHTTP(rec, req)
	}()

	// Give the handler time to subscribe, then push a change.
	time.Sleep(100 * time.Millisecond)
	s.hub.Broadcast(3)
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event: change") {
		t.Errorf("stream did not contain a change event:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}
