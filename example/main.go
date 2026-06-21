package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/hobbestherat/webapi"
)

// Message represents a simple key/value entry.
type Message struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Scope string `json:"scope"`
}

// MessageRequest represents a request to add an entry.
type MessageRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// MessageService handles sample message catalog requests.
type MessageService struct {
	// Dependencies would be injected here
}

// ListMessages returns all entries for a scope.
func (s *MessageService) ListMessages(r *http.Request) (interface{}, error) {
	// Get user from context (optional for this endpoint)
	user, _ := webapi.GetUser(r.Context())

	// Scope comes from the query string. Bare scalar handler params bind
	// from the path only, so query inputs are read from r.URL.Query()
	// (or via a struct parameter).
	scope := r.URL.Query().Get("scope")

	// Logic to fetch entries.
	messages := []Message{
		{Key: "hello", Value: "Hello", Scope: scope},
		{Key: "world", Value: "World", Scope: scope},
	}

	if user != nil {
		fmt.Printf("User %d requested messages\n", user.ID)
	}

	return messages, nil
}

// AddMessage adds a new entry.
func (s *MessageService) AddMessage(r *http.Request, req MessageRequest) (interface{}, error) {
	// Get user from context (required for this endpoint)
	user, ok := webapi.GetUser(r.Context())
	if !ok {
		return nil, fmt.Errorf("user not found in context")
	}

	// Extract scope from URL.
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		return nil, fmt.Errorf("scope parameter is required")
	}

	// Logic to add entry.
	message := Message{
		Key:   req.Key,
		Value: req.Value,
		Scope: scope,
	}

	fmt.Printf("User %d added message: %+v\n", user.ID, message)

	return message, nil
}

// ActionPublish publishes entries.
func (s *MessageService) ActionPublish(r *http.Request, req struct{}) (interface{}, error) {
	// Get user from context
	user, ok := webapi.GetUser(r.Context())
	if !ok {
		return nil, fmt.Errorf("user not found in context")
	}

	// Logic to publish entries.
	fmt.Printf("User %d published messages\n", user.ID)

	return struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}{
		Success: true,
		Message: "Messages published successfully",
	}, nil
}

// StreamTicks is a Server-Sent Events endpoint. It returns an
// EventStreamResponse whose Producer pushes one event per second until the
// client disconnects. Connect with:
//
//	curl -N localhost:8080/api/messages/ticks
func (s *MessageService) StreamTicks(r *http.Request) (interface{}, error) {
	return &webapi.EventStreamResponse{
		Producer: func(stream webapi.EventStream) error {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for i := 1; ; i++ {
				select {
				case <-stream.Context().Done():
					return nil // client disconnected
				case <-ticker.C:
					ev := webapi.SSEvent{
						Name: "tick",
						Data: fmt.Sprintf(`{"n":%d}`, i),
					}
					if err := stream.Send(ev); err != nil {
						return err
					}
				}
			}
		},
	}, nil
}

// inMemorySession is a toy Session value. A real implementation would be
// hydrated from your session store / database.
type inMemorySession struct {
	userID      int64
	displayName string
}

func (s inMemorySession) GetUserID() (int64, bool) {
	if s.userID == 0 {
		return 0, false // anonymous
	}
	return s.userID, true
}

func (s inMemorySession) GetUserState() webapi.UserState {
	if s.userID == 0 {
		return webapi.UserStateUnknown
	}
	return webapi.UserStateComplete
}

// GetDisplayName is optional; webapi picks it up to fill User.DisplayName.
func (s inMemorySession) GetDisplayName() string { return s.displayName }

// inMemorySessionProvider is a DEMO SessionProvider. It maps an opaque
// session token (read from the "session" cookie) to a user via a hard-coded
// in-memory table. It exists only so this example runs standalone — in a
// real deployment you MUST bring your own SessionProvider (e.g. backed by a
// database or your auth service). webapi intentionally ships no provider.
type inMemorySessionProvider struct {
	sessions map[string]inMemorySession
}

func newInMemorySessionProvider() *inMemorySessionProvider {
	return &inMemorySessionProvider{
		sessions: map[string]inMemorySession{
			// Try it: curl --cookie "session=demo-token" localhost:8080/api/messages/list?scope=greetings
			"demo-token": {userID: 1, displayName: "Demo User"},
		},
	}
}

func (p *inMemorySessionProvider) GetSession(r *http.Request) (webapi.Session, error) {
	cookie, err := r.Cookie("session")
	if err != nil {
		return inMemorySession{}, nil // no cookie -> anonymous session
	}
	if sess, ok := p.sessions[cookie.Value]; ok {
		return sess, nil
	}
	return inMemorySession{}, nil // unknown token -> anonymous session
}

func main() {
	// Demo SessionProvider: a hard-coded in-memory token table. Real
	// services must supply their own provider.
	sessionProvider := newInMemorySessionProvider()

	// Create example service.
	messageService := &MessageService{}

	// Define API
	api := &webapi.API{
		BasePath:        "/api/messages",
		LoginPath:       "/login",
		SessionProvider: sessionProvider,
		Endpoints: []webapi.Endpoint{
			{
				Path:        "/list",
				Method:      http.MethodGet,
				Handler:     messageService.ListMessages,
				AuthLevel:   webapi.AuthOptional,
				Description: "List messages for a scope",
			},
			{
				Path:        "/add",
				Method:      http.MethodPost,
				Handler:     messageService.AddMessage,
				AuthLevel:   webapi.AuthRequired,
				Description: "Add a new message",
			},
			{
				Path:        "/action-publish",
				Method:      http.MethodPost,
				Handler:     messageService.ActionPublish,
				AuthLevel:   webapi.AuthRequired,
				Description: "Publish messages",
			},
			{
				Path:        "/ticks",
				Method:      http.MethodGet,
				Handler:     messageService.StreamTicks,
				AuthLevel:   webapi.AuthOptional,
				Description: "Stream tick events via Server-Sent Events",
			},
		},
	}

	// Create HTTP mux
	mux := http.NewServeMux()

	// Register API handlers
	api.RegisterHandlers(mux)

	// Start server
	http.ListenAndServe(":8080", mux)
}
