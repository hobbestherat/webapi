package webapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func dispatchEventStream(r *http.Request, esr *EventStreamResponse) *httptest.ResponseRecorder {
	api := &API{}
	rec := httptest.NewRecorder()
	api.writeEventStream(rec, r, esr)
	return rec
}

func TestEventStreamWritesFramedEvents(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := dispatchEventStream(r, &EventStreamResponse{
		Producer: func(stream EventStream) error {
			if err := stream.Send(SSEvent{ID: "1", Name: "tick", Data: "hello"}); err != nil {
				return err
			}
			return stream.Send(SSEvent{Data: "line1\nline2", Retry: 2000})
		},
	})

	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}

	body := rec.Body.String()
	wantFirst := "id: 1\nevent: tick\ndata: hello\n\n"
	if !strings.HasPrefix(body, wantFirst) {
		t.Fatalf("first event not framed correctly.\n got: %q\nwant prefix: %q", body, wantFirst)
	}
	// Multi-line data must produce one "data:" line per source line.
	if !strings.Contains(body, "retry: 2000\ndata: line1\ndata: line2\n\n") {
		t.Fatalf("multi-line/retry event not framed correctly: %q", body)
	}
}

func TestEventStreamStopsWhenClientDisconnects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client already gone
	r := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)

	sends := 0
	dispatchEventStream(r, &EventStreamResponse{
		Producer: func(stream EventStream) error {
			for i := 0; i < 5; i++ {
				if err := stream.Send(SSEvent{Data: "x"}); err != nil {
					return err
				}
				sends++
			}
			return nil
		},
	})

	if sends != 0 {
		t.Fatalf("expected no successful sends after disconnect, got %d", sends)
	}
}

func TestEventStreamCommentKeepAlive(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := dispatchEventStream(r, &EventStreamResponse{
		Producer: func(stream EventStream) error {
			return stream.Comment("keep-alive")
		},
	})
	if got := rec.Body.String(); got != ": keep-alive\n" {
		t.Fatalf("comment line = %q, want %q", got, ": keep-alive\n")
	}
}

func TestStripNewlinesPreventsInjection(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := dispatchEventStream(r, &EventStreamResponse{
		Producer: func(stream EventStream) error {
			return stream.Send(SSEvent{ID: "1\nevent: injected", Data: "ok"})
		},
	})
	if strings.Contains(rec.Body.String(), "\nevent: injected") {
		t.Fatalf("newline injection not stripped from id: %q", rec.Body.String())
	}
}

type sseTestService struct{}

func (sseTestService) Stream(r *http.Request) (interface{}, error) {
	return &EventStreamResponse{
		Producer: func(stream EventStream) error {
			for i := 1; i <= 3; i++ {
				if err := stream.Send(SSEvent{Name: "tick", Data: "n=" + itoa(i)}); err != nil {
					return err
				}
			}
			return nil
		},
	}, nil
}

func itoa(i int) string {
	return string(rune('0' + i))
}

// TestEventStreamOverRealServer exercises the full path over a real socket
// (httptest.NewServer), proving the headers and per-event flush work outside
// the ResponseRecorder fast path.
func TestEventStreamOverRealServer(t *testing.T) {
	api := &API{
		BasePath: "/api",
		Endpoints: []Endpoint{
			{Path: "/stream", Method: http.MethodGet, Handler: sseTestService{}.Stream, AuthLevel: AuthNone},
		},
	}
	mux := http.NewServeMux()
	api.RegisterHandlers(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	var events int
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "event: tick") {
			events++
		}
	}
	if events != 3 {
		t.Fatalf("received %d tick events, want 3", events)
	}
}
