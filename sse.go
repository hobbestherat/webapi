package webapi

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// SSEvent is a single Server-Sent Event. Every field is optional; an event
// carrying only Data is the common case. Use EventStream.Comment for bare
// comment/keep-alive lines.
type SSEvent struct {
	// ID sets the event's "id:" field. The browser echoes the last seen id
	// back in the Last-Event-ID header when it reconnects.
	ID string
	// Name sets the event's "event:" field (the client-side event type).
	// Empty means the default "message" event.
	Name string
	// Data is the event payload, typically a JSON string. Multi-line data
	// is emitted as multiple "data:" lines per the SSE spec.
	Data string
	// Retry, when > 0, sets the client's reconnection delay in milliseconds.
	Retry int
}

// EventStream is handed to an EventStreamResponse.Producer to push events to
// a single connected client. Send/Comment return an error once the client
// has disconnected or the write fails; the producer should then return.
type EventStream interface {
	// Send writes one event and flushes it to the client.
	Send(ev SSEvent) error
	// Comment writes an SSE comment line (": ...") and flushes. Handy as a
	// keep-alive that stops idle proxies from closing the connection.
	Comment(text string) error
	// Context is cancelled when the client disconnects.
	Context() context.Context
}

// EventStreamResponse switches the connection into Server-Sent Events mode.
// Return one from a handler (with a nil error) and webapi sets the
// text/event-stream headers and invokes Producer with a live EventStream.
// The response ends when Producer returns, the client disconnects, or a
// Send/Comment call errors.
type EventStreamResponse struct {
	// Producer pushes events until it returns, the client disconnects
	// (stream.Context() is cancelled), or a Send/Comment call errors.
	Producer func(stream EventStream) error
	// Headers are extra response headers set before the stream starts
	// (e.g. a custom Cache-Control). Optional.
	Headers map[string]string
}

// sseStream is the concrete EventStream backed by the response writer.
type sseStream struct {
	w       io.Writer
	flusher http.Flusher
	ctx     context.Context
}

func (s *sseStream) Context() context.Context { return s.ctx }

func (s *sseStream) Send(ev SSEvent) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	var b strings.Builder
	if ev.ID != "" {
		b.WriteString("id: ")
		b.WriteString(stripNewlines(ev.ID))
		b.WriteByte('\n')
	}
	if ev.Name != "" {
		b.WriteString("event: ")
		b.WriteString(stripNewlines(ev.Name))
		b.WriteByte('\n')
	}
	if ev.Retry > 0 {
		fmt.Fprintf(&b, "retry: %d\n", ev.Retry)
	}
	// Data may be multi-line; each line needs its own "data:" prefix.
	for _, line := range strings.Split(ev.Data, "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n') // blank line dispatches the event
	return s.writeAndFlush(b.String())
}

func (s *sseStream) Comment(text string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		b.WriteString(": ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return s.writeAndFlush(b.String())
}

func (s *sseStream) writeAndFlush(payload string) error {
	if _, err := io.WriteString(s.w, payload); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

// writeEventStream sets up the SSE headers and drives the producer. It is
// called from processResults when a handler returns *EventStreamResponse.
func (api *API) writeEventStream(w http.ResponseWriter, r *http.Request, esr *EventStreamResponse) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	if esr.Producer == nil {
		http.Error(w, "Invalid event stream response", http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	// Stop nginx and similar proxies from buffering the stream.
	h.Set("X-Accel-Buffering", "no")
	for key, value := range esr.Headers {
		h.Set(key, value)
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush() // commit headers so the client opens the stream

	stream := &sseStream{w: w, flusher: flusher, ctx: r.Context()}
	if err := esr.Producer(stream); err != nil && r.Context().Err() == nil {
		log.Printf("event stream producer error: %v", err)
	}
}

// stripNewlines removes CR/LF so a field value can't inject extra SSE lines.
func stripNewlines(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}
