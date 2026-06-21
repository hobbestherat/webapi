package webapi

import "net/http"

// LocalSessionProvider is a SessionProvider for local-only tools — e.g. a web
// UI fronting a command-line program — that have no real authentication. It
// treats every request as the same fixed, fully-activated local user, so
// endpoints at any AuthLevel (AuthNone, AuthOptional, AuthRequired) work
// without wiring up a session store.
//
// SECURITY: never use this on a network-exposed server. It grants every caller
// the local user's identity and permissions. Bind such a tool to loopback
// (127.0.0.1) only.
type LocalSessionProvider struct {
	// UserID is the id reported for the local user. Defaults to 1 when 0.
	UserID int64
	// DisplayName is surfaced as User.DisplayName. Optional.
	DisplayName string
}

// GetSession returns the fixed local user for every request.
func (p LocalSessionProvider) GetSession(r *http.Request) (Session, error) {
	id := p.UserID
	if id == 0 {
		id = 1
	}
	return localSession{userID: id, displayName: p.DisplayName}, nil
}

// localSession is the always-authenticated session handed out by
// LocalSessionProvider.
type localSession struct {
	userID      int64
	displayName string
}

func (s localSession) GetUserID() (int64, bool) { return s.userID, true }

func (s localSession) GetUserState() UserState { return UserStateComplete }

// GetDisplayName lets webapi populate User.DisplayName.
func (s localSession) GetDisplayName() string { return s.displayName }
