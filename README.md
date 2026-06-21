# webapi

A small, dependency-free HTTP API framework for Go. You declare endpoints as
a list of plain Go handler **methods**, and `webapi` handles routing
(including path parameters), JSON body binding, query binding,
authentication/session integration (but you need to link it to a session and auth provider), permission checks, request-body size
limits, and response serialization — including binary, streamed, cookie, and
[Server-Sent Events](#server-sent-events-sse) responses.

It is built on the standard library's `net/http` and `http.ServeMux`.

```go
import "github.com/hobbestherat/webapi"
```

---

## Table of contents

- [Why webapi?](#why-webapi)
- [Quick start](#quick-start)
- [Core concepts](#core-concepts)
- [Handler signatures](#handler-signatures)
- [Routing](#routing)
- [Request binding](#request-binding)
- [Responses](#responses)
- [Server-Sent Events (SSE)](#server-sent-events-sse)
- [Authentication & sessions](#authentication--sessions)
- [Permissions](#permissions)
- [Account lifecycle (states)](#account-lifecycle-states)
- [Request body size limits](#request-body-size-limits)
- [Errors](#errors)
- [Logging](#logging)
- [API reference](#api-reference)
- [Running the example](#running-the-example)

---

## Why webapi?

- **Zero dependencies.** Standard library only — no router, validation, or
  middleware frameworks pulled in.
- **Reflection-based binding.** Write handlers with idiomatic Go signatures
  (`func (s *Svc) Update(r *http.Request, req UpdateReq) (*Thing, error)`);
  `webapi` inspects the signature and binds path/query/body automatically.
- **Pluggable auth.** Bring your own `SessionProvider` (cookie, JWT, bearer
  token, …). `webapi` never assumes how you store sessions.
- **First-class streaming.** Binary, chunked `StreamResponse`, and SSE
  (`EventStreamResponse`) are built in.

---

## Quick start

```go
package main

import (
	"net/http"

	"github.com/hobbestherat/webapi"
)

type Message struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type MessageService struct{}

// GET  /api/messages      (path params bound positionally)
func (s *MessageService) List(r *http.Request) (interface{}, error) {
	return []Message{{Key: "hello", Value: "Hello"}}, nil
}

// POST /api/messages      (JSON body bound into req)
func (s *MessageService) Add(r *http.Request, req Message) (interface{}, error) {
	return req, nil // marshaled to JSON
}

func main() {
	svc := &MessageService{}

	api := &webapi.API{
		BasePath:  "/api",
		LoginPath: "/login",
		// You MUST provide a SessionProvider. See "Authentication & sessions".
		SessionProvider: mySessionProvider{},
		Endpoints: []webapi.Endpoint{
			{Path: "/messages", Method: http.MethodGet,  Handler: svc.List, AuthLevel: webapi.AuthNone},
			{Path: "/messages", Method: http.MethodPost, Handler: svc.Add,  AuthLevel: webapi.AuthRequired},
		},
	}

	mux := http.NewServeMux()
	api.RegisterHandlers(mux)
	http.ListenAndServe(":8080", mux)
}
```

---

## Core concepts

| Type              | Purpose                                                        |
| ----------------- | ------------------------------------------------------------- |
| `API`             | A collection of `Endpoint`s plus shared config. Mounted onto a mux. |
| `Endpoint`        | One route: `Path`, `Method`, `Handler`, `AuthLevel`, etc.     |
| `SessionProvider` | Resolves an `*http.Request` to a `Session`. **You implement this.** |
| `Session`         | Exposes the authenticated user id/state.                      |
| `PermissionChecker` | Optional; backs `Endpoint.Permissions`.                     |

The flow for each request is:

1. Resolve the session via `SessionProvider.GetSession`.
2. Enforce `AuthLevel` (redirect-to-login / 401 / 403 for bad account state).
3. Enforce `Endpoint.Permissions` (if any) via `PermissionChecker`.
4. Apply the request-body size cap (`http.MaxBytesReader`).
5. Bind arguments (path → query → body) by reflecting on the handler signature.
6. Call the handler and serialize its return value.

---

## Handler signatures

Every handler is a **method** (or function value) of the form:

```go
func (s *Service) Name(r *http.Request [, body] [, pathParams...] [, queryStruct]) (interface{}, error)
```

- **1st param:** always `*http.Request`.
- **2nd param (body):** for `POST`/`PUT`/`PATCH`, may be a `struct` or
  `map`. Decoded from the JSON request body. Omit it for handlers that take
  no body.
- **Remaining params:** bound from the route's **path parameters**
  (positional, in route order). Supported kinds:
  - `string` — one path param
  - `int` / `int32` / `int64` — one path param, parsed as an integer
  - `...string` (variadic) — consumes all remaining path params
  - `struct` — populated from **query** parameters (via `json` tags, falling
    back to lowercased field names)

> **Query parameters bind only through a `struct` parameter.** Go reflection
> cannot recover a scalar parameter's name, so a bare `int`/`string` param can
> only be filled from the path. To read query values, declare a `struct`
> parameter (or read `r.URL.Query()` directly in the handler).

The handler must return **exactly two values**: a result and an `error`.

```go
// Path: /api/things/:id/edit/:field
func (s *Service) Edit(r *http.Request, body EditReq, id string, field string) (interface{}, error)
```

---

## Routing

Routes are declared with `:name` path parameters:

```go
{Path: "/things/:id",         Method: http.MethodGet, ...}
{Path: "/things/:id/sub/:sub", Method: http.MethodGet, ...}
```

- **Catch-all / wildcard:** a trailing `:name...` matches the rest of the path
  including slashes (e.g. `/files/:path...` → `GET /files/a/b/c`). It must be
  the last segment.
- **Overlapping routes:** static segments are preferred over parameters, and
  the same path can serve multiple HTTP methods. `webapi` registers static
  routes directly with `http.ServeMux` and routes parameterized routes through
  a central catch-all.
- **Specificity:** routes with fewer parameters are matched first.
- **Method enforcement:** the request method must match the endpoint's
  `Method` exactly. A request to a known path with an unsupported method gets
  `405 Method Not Allowed` plus an `Allow` header listing the methods
  registered for that path. An unknown path gets `404 Not Found`. Any HTTP
  verb may be used (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, …); only
  `POST`/`PUT`/`PATCH` attempt to decode a request body.

> Path parameters are also available via `webapi.GetPathParams(r.Context())`,
> returning a `map[string]string`.

---

## Request binding

- **JSON bodies** (`POST`/`PUT`/`PATCH` whose 2nd handler param is a struct or
  map) require a `Content-Type` of `application/json` (or any `…+json`
  variant such as `application/vnd.foo+json`). Anything else yields
  `415 Unsupported Media Type`.
- **Empty body:** a struct-typed body is allowed to be empty only if the
  struct has zero fields; otherwise a body is required (`400`).
- **Multiple JSON values** in one body are rejected (`400`).
- **Query params** bind only into a `struct` handler parameter (using `json`
  tags, falling back to lowercased field names). Bare positional
  `int`/`string` params are filled from the **path** only — see the note under
  [Handler signatures](#handler-signatures).

---

## Responses

Return `(interface{}, error)`. The result value controls the response shape:

| Return type                       | Behavior                                                         |
| --------------------------------- | ---------------------------------------------------------------- |
| `nil` result                      | `204 No Content`                                                 |
| any JSON-serializable value       | `200` + `application/json` body                                  |
| `*webapi.BinaryResponse`          | Raw bytes with a `Content-Type` / `Content-Encoding`             |
| `*webapi.StreamResponse`          | Streams from an `io.ReadCloser`; sets `Content-Length` when `> 0`, else chunked |
| `*webapi.CookieResponse`          | Sets cookies, then JSON-encodes `Data` (or `204` if `Data == nil`) |
| `*webapi.EventStreamResponse`     | Switches to SSE — see below                                      |
| error                             | See [Errors](#errors)                                            |

`StreamResponse` is the right choice for large downloads so the whole payload
isn't buffered in memory:

```go
return &webapi.StreamResponse{
	ContentType:   "application/zip",
	ContentLength: size, // omitted (chunked) when <= 0
	Reader:        rc, // closed by webapi
}, nil
```

---

## Server-Sent Events (SSE)

Return `*webapi.EventStreamResponse`. `webapi` sets the
`text/event-stream` headers and invokes your `Producer` with a live
`EventStream`:

```go
func (s *Service) Stream(r *http.Request) (interface{}, error) {
	return &webapi.EventStreamResponse{
		Producer: func(stream webapi.EventStream) error {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for i := 1; ; i++ {
				select {
				case <-stream.Context().Done():
					return nil // client disconnected
				case <-ticker.C:
					if err := stream.Send(webapi.SSEvent{
						Name: "tick",
						Data: fmt.Sprintf(`{"n":%d}`, i),
					}); err != nil {
						return err
					}
				}
			}
		},
	}, nil
}
```

- `stream.Send(SSEvent{...})` writes and flushes one event. Multi-line
  `Data` is split into one `data:` line per source line.
- `stream.Comment("...")` emits a `: ...` keep-alive line.
- `stream.Context()` is cancelled when the client disconnects — check it to
  stop producing.
- Field values are sanitized so newlines cannot inject extra SSE lines.
- `X-Accel-Buffering: no` is set to stop nginx buffering the stream.

---

## Authentication & sessions

`webapi` does **not** ship a session store. You implement two small
interfaces:

```go
type SessionProvider interface {
	GetSession(r *http.Request) (Session, error)
}

type Session interface {
	GetUserID() (int64, bool) // userID, isAuthenticated
	GetUserState() UserState
}
```

`AuthLevel` per endpoint:

| Level            | Behavior                                                         |
| ---------------- | ---------------------------------------------------------------- |
| `AuthNone`       | No session lookup. Handler always runs.                          |
| `AuthOptional`   | Session is resolved; if present, `User` is put in context.       |
| `AuthRequired`   | Unauthenticated → `GET` redirects to `LoginPath?redirect=…`; other methods → `401`. |

The authenticated user is available in the handler via:

```go
user, ok := webapi.GetUser(r.Context())
// user.ID, user.State, user.DisplayName
```

`DisplayName` is filled in automatically if your `Session` also implements the
optional `GetDisplayName() string` method (a duck-typed extension — see
`displayNameSession` in the source).

For ad-hoc checks inside a handler:

```go
user, httpErr := webapi.RequireAuthenticatedUser(r.Context())
user, httpErr := webapi.RequirePermission(r.Context(), checker, "admin")
```

### Local-only tools (no auth)

For a local tool that has no real authentication — e.g. a web UI fronting a
command-line program — use the built-in `LocalSessionProvider`. It treats every
request as the same fixed, fully-activated local user, so endpoints at any
`AuthLevel` (including `AuthRequired`) work without a session store:

```go
api := &webapi.API{
	// ...
	SessionProvider: webapi.LocalSessionProvider{DisplayName: "Local User"},
}
```

> **Security:** never use `LocalSessionProvider` on a network-exposed server —
> it grants every caller the local user's identity. Bind such tools to loopback
> (`127.0.0.1`) only.

---

## Permissions

An endpoint can declare required permission keys. All listed keys are
required (logical AND), and declaring any implicitly upgrades the endpoint to
`AuthRequired`:

```go
{Path: "/admin/users/:id", Method: http.MethodDelete,
 Handler: s.DeleteUser, AuthLevel: webapi.AuthRequired,
 Permissions: []string{"admin", "user.delete"}}
```

Provide a `PermissionChecker`:

```go
type PermissionChecker interface {
	HasPermission(ctx context.Context, userID int64, perm string) (bool, error)
}

api := &webapi.API{
	// ...
	PermissionChecker: myChecker,
}
```

Permission checks run **before** the request body is decoded. For
request-dependent permission decisions, call
`webapi.RequirePermission(...)` from inside the handler instead.

---

## Account lifecycle (states)

`AuthRequired` endpoints only admit users in `UserStateComplete`. The full
set:

| State                       | Meaning                                              |
| --------------------------- | ---------------------------------------------------- |
| `UserStateUnknown`          | Default/unknown                                      |
| `UserStatePendingVerification` | Registered but email not verified                 |
| `UserStateComplete`         | Fully usable account                                 |
| `UserStateSuspended`        | Locked by an admin                                   |
| `UserStateDeleted`          | Hard-deleted                                         |
| `UserStatePendingDelete`    | Inside the deletion grace period — **locked down** |

A `UserStatePendingDelete` account is allowed to reach **only** endpoints
that opt in with `Endpoint.AllowPendingDelete = true` (e.g. cancel-deletion,
data export). All other `AuthRequired` endpoints return `403`.

---

## Request body size limits

Every request is capped with `http.MaxBytesReader`, even bodyless ones. The
effective cap, in priority order:

1. `Endpoint.MaxBodyBytes` (if `> 0`)
2. `API.MaxBodyBytes` (if `> 0`)
3. `webapi.DefaultMaxBodyBytes` (`1 << 20` = 1 MiB)

Set `Endpoint.MaxBodyBytes = -1` to disable the cap for an endpoint (rarely
needed — prefer an explicit large value, and use `StreamResponse` for uploads).

---

## Errors

Return an `error`. A plain `error` becomes `500 Internal Server Error` with
the error message. For control over the status code, return a
`*webapi.HTTPError`:

```go
return nil, webapi.NewHTTPError(http.StatusBadRequest, "invalid email")

// Attach structured details -> response body is
// {"error": "...", "details": {...}}
return nil, webapi.NewHTTPError(http.StatusPaymentRequired, "quota exceeded").
	WithDetails(map[string]int{"used": 950, "limit": 1000})
```

When `Details` is non-nil the body is JSON of the form
`{"error": "<Message>", "details": <Details>}`; otherwise the legacy
plain-text body from `http.Error` is used.

---

## Logging

`webapi` only logs its own internal warnings — session-provider failures,
internal permission-check errors, and stream-copy failures. By default these
go to `log.Default()`. To route them through your own logger, set `API.Logger`
to anything satisfying the small `Logger` interface (the stdlib `*log.Logger`
already does):

```go
type Logger interface {
	Printf(format string, args ...interface{})
}

api := &webapi.API{
	// ...
	Logger: log.New(os.Stderr, "webapi ", log.LstdFlags),
}
```

A `*slog.Logger` can be adapted with a tiny wrapper exposing `Printf`.

---

## API reference

### `API`

```go
type API struct {
	BasePath          string
	LoginPath         string
	Endpoints         []Endpoint
	SessionProvider   SessionProvider
	PermissionChecker PermissionChecker // optional
	MaxBodyBytes      int64             // 0 -> DefaultMaxBodyBytes
	Logger            Logger            // optional; defaults to log.Default()

	// routes []route  // internal, populated by RegisterHandlers
}

func (api *API) RegisterHandlers(mux *http.ServeMux)
```

Handler signatures are validated when `RegisterHandlers` is called: each
`Handler` must be a func whose first parameter is `*http.Request` and that
returns exactly two values, the second being `error`. A misconfigured
endpoint panics at startup rather than failing on the first request.

### `Endpoint`

```go
type Endpoint struct {
	Path              string
	Method            string
	Handler           interface{}
	AuthLevel         AuthLevel
	Description       string
	Permissions       []string
	MaxBodyBytes      int64
	AllowPendingDelete bool
}
```

### Context helpers

```go
func GetUser(ctx context.Context) (*User, bool)
func GetPathParams(ctx context.Context) map[string]string
func RequireAuthenticatedUser(ctx context.Context) (*User, *HTTPError)
func RequirePermission(ctx context.Context, checker PermissionChecker, perm string) (*User, *HTTPError)
```

### Response types

`BinaryResponse`, `StreamResponse`, `CookieResponse`, `EventStreamResponse`,
`SSEvent`.

---

## Running the example

A standalone, runnable server lives in [`example`](example/main.go).
It uses an in-memory toy `SessionProvider` (cookie `session=demo-token`)
so you can exercise auth without a database:

```bash
go run ./example

# anonymous (AuthOptional):
curl 'localhost:8080/api/messages/list?scope=greetings'

# authenticated (AuthRequired), using the demo token:
curl --cookie "session=demo-token" 'localhost:8080/api/messages/add?scope=greetings' \
     -H 'Content-Type: application/json' -d '{"key":"hi","value":"Hi"}'

# Server-Sent Events stream:
curl -N localhost:8080/api/messages/ticks
```

> The example is intentionally self-contained. In a real deployment you must
> supply your own `SessionProvider` (e.g. backed by your session store /
> database). `webapi` ships no provider by design. For a local-only tool with
> no auth, use the built-in [`LocalSessionProvider`](#local-only-tools-no-auth).

---

## License

Licensed under the [Apache License, Version 2.0](LICENSE). See the `LICENSE`
file for the full text.
