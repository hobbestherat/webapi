// Package webapi is a small, dependency-free HTTP API framework built on the
// standard library's net/http and http.ServeMux.
//
// You declare endpoints as a list of plain Go handler methods; webapi handles
// routing (including path parameters), JSON body binding, query binding,
// authentication/session integration, permission checks, request-body size
// limits, and response serialization — including binary, streamed, cookie, and
// Server-Sent Events responses.
//
// Bring your own SessionProvider (cookie, JWT, bearer token, …); webapi never
// assumes how sessions are stored. See the README and webapi/example for a
// runnable, self-contained server.
package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Reflection types reused across handler validation and result processing.
var (
	httpRequestType = reflect.TypeOf((*http.Request)(nil))
	errorType       = reflect.TypeOf((*error)(nil)).Elem()
)

// Logger is the minimal logging surface webapi needs; it is satisfied by the
// stdlib *log.Logger. Set API.Logger to route webapi's internal warnings
// (session, permission-check and stream-copy failures) through your own
// logger. When nil, log.Default() is used.
type Logger interface {
	Printf(format string, args ...interface{})
}

// reflectValueIsNil reports whether v holds a nil value, without panicking on
// kinds that are not nilable. Non-nilable kinds (e.g. a struct returned by
// value) are reported as non-nil, which is the desired behavior for both the
// error and result return values of a handler.
func reflectValueIsNil(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

// Context keys
type contextKey string

const (
	UserContextKey contextKey = "user"
	PathParamsKey  contextKey = "pathParams"
)

// AuthLevel defines the required level of authentication for an endpoint
type AuthLevel int

const (
	AuthNone     AuthLevel = iota // Public access
	AuthOptional                  // Authentication is optional
	AuthRequired                  // Authentication is required
)

// User represents an authenticated user
type User struct {
	ID          int64
	Username    string
	DisplayName string
	State       UserState
}

// displayNameSession is implemented by Session implementations that can
// surface a human-friendly display name without an extra DB round-trip
// (e.g. sessionsdb.liveSession). webapi populates User.DisplayName from
// it when present.
type displayNameSession interface {
	GetDisplayName() string
}

// UserState represents the state of a user account
type UserState int

const (
	UserStateUnknown UserState = iota
	UserStatePendingVerification
	UserStateComplete
	UserStateSuspended
	UserStateDeleted
	// UserStatePendingDelete marks an account that the user asked to
	// delete and that is now inside its grace period. Such an account is
	// locked down: only endpoints that explicitly opt in via
	// Endpoint.AllowPendingDelete (cancel-deletion, data export, take-out
	// list/download) remain reachable. See the AuthRequired gate in
	// wrapHandler.
	UserStatePendingDelete
)

// HTTPError is a custom error type that includes an HTTP status code.
//
// When Details is non-nil the response body is emitted as JSON of the form
//
//	{"error": "<Message>", "details": <Details>}
//
// otherwise the response is the legacy plain-text body produced by
// http.Error. Use the JSON form to give the client structured information
// it can act on (e.g. quota usage, suggested upgrade plan, trash size).
type HTTPError struct {
	Code    int
	Message string
	Details interface{}
}

// Error implements the error interface
func (e *HTTPError) Error() string {
	return e.Message
}

// WithDetails attaches structured details to the error and returns it for
// chaining. The details value must be JSON serialisable.
func (e *HTTPError) WithDetails(details interface{}) *HTTPError {
	e.Details = details
	return e
}

// BinaryResponse represents a binary response with content type, encoding, and data
type BinaryResponse struct {
	ContentType     string
	ContentEncoding string
	Data            []byte
}

// StreamResponse represents a streamed response body. Use this for large
// downloads to avoid loading the full payload into server memory.
type StreamResponse struct {
	ContentType     string
	ContentEncoding string
	// ContentLength sets the Content-Length header when > 0. A value of 0
	// (the zero value) or negative omits the header, letting the response
	// be sent with chunked transfer encoding for streams of unknown size.
	ContentLength int64
	Headers       map[string]string
	Reader        io.ReadCloser
}

// CookieResponse represents a response that sets cookies
type CookieResponse struct {
	Cookies []*http.Cookie // Cookies to set in the response
	Data    interface{}    // The actual response data (will be marshaled to JSON)
}

// NewHTTPError creates a new HTTPError with the given status code and message
func NewHTTPError(code int, message string) *HTTPError {
	return &HTTPError{
		Code:    code,
		Message: message,
	}
}

// Session provides access to session information
type Session interface {
	GetUserID() (int64, bool)
	GetUserState() UserState
}

// SessionProvider provides session management
type SessionProvider interface {
	GetSession(r *http.Request) (Session, error)
}

// Endpoint defines a single API endpoint
type Endpoint struct {
	Path        string
	Method      string
	Handler     interface{}
	AuthLevel   AuthLevel
	Description string

	// Permissions, when non-empty, gates the endpoint behind one or more
	// permission keys. The request user must hold ALL listed permissions
	// (logical AND). Requires the parent API to have a PermissionChecker
	// configured. Implicitly upgrades AuthLevel to AuthRequired.
	Permissions []string

	// MaxBodyBytes caps the JSON request body. 0 means use the API
	// default. -1 disables the cap (rarely what you want).
	MaxBodyBytes int64

	// AllowPendingDelete keeps the endpoint reachable for users whose
	// account is in UserStatePendingDelete (inside the deletion grace
	// period). All other AuthRequired endpoints reject such users with
	// HTTP 403 so a soon-to-be-deleted account can only cancel the
	// deletion or download its data / take-outs. Has no effect on
	// AuthOptional / AuthNone endpoints.
	AllowPendingDelete bool
}

// DefaultMaxBodyBytes is the per-request JSON body cap applied when neither
// the API nor the Endpoint specifies one. 1 MiB is plenty for the JSON
// payloads we currently accept and shields all endpoints from trivial
// memory-exhaustion attempts.
const DefaultMaxBodyBytes int64 = 1 << 20

// API represents a collection of endpoints
type API struct {
	BasePath        string
	LoginPath       string
	Endpoints       []Endpoint
	SessionProvider SessionProvider

	// PermissionChecker resolves "does this user hold this permission" for
	// any Endpoint that declares Permissions. May be nil if no endpoint
	// uses Permissions.
	PermissionChecker PermissionChecker

	// MaxBodyBytes is the default body cap for all endpoints that don't
	// set their own. 0 falls back to DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// Logger receives webapi's internal warnings (session, permission and
	// stream-copy failures). When nil, log.Default() is used.
	Logger Logger

	routes []route // Internal compiled routes
}

// logger returns the configured Logger, falling back to log.Default().
func (api *API) logger() Logger {
	if api.Logger != nil {
		return api.Logger
	}
	return log.Default()
}

// validateHandler checks an endpoint's handler signature at registration time
// so misconfiguration fails fast at startup instead of as a 500 or panic on
// the first matching request. It enforces the invariants the reflection layer
// relies on: the handler is a func (a method value such as svc.Method), its
// first parameter is *http.Request, and it returns exactly two values whose
// second is an error.
func validateHandler(endpoint Endpoint) error {
	if endpoint.Handler == nil {
		return fmt.Errorf("webapi: endpoint %s %s has a nil Handler", endpoint.Method, endpoint.Path)
	}
	t := reflect.TypeOf(endpoint.Handler)
	if t.Kind() != reflect.Func {
		return fmt.Errorf("webapi: endpoint %s %s Handler must be a func (e.g. a method value svc.Method), got %s", endpoint.Method, endpoint.Path, t.Kind())
	}
	if t.NumIn() < 1 || t.In(0) != httpRequestType {
		return fmt.Errorf("webapi: endpoint %s %s Handler's first parameter must be *http.Request", endpoint.Method, endpoint.Path)
	}
	if t.NumOut() != 2 {
		return fmt.Errorf("webapi: endpoint %s %s Handler must return exactly two values (result, error), got %d", endpoint.Method, endpoint.Path, t.NumOut())
	}
	if !t.Out(1).Implements(errorType) {
		return fmt.Errorf("webapi: endpoint %s %s Handler's second return value must be error, got %s", endpoint.Method, endpoint.Path, t.Out(1))
	}
	return nil
}

// route is an internal structure for compiled route patterns
type route struct {
	original    string           // Original path template
	pattern     *regexp.Regexp   // Compiled pattern
	paramNames  []string         // Parameter names in order
	endpoint    Endpoint         // The associated endpoint
	handlerFunc http.HandlerFunc // The wrapped handler
}

// RegisterHandlers registers all endpoints with the provided HTTP mux
func (api *API) RegisterHandlers(mux *http.ServeMux) {
	// Compile all routes first
	api.routes = make([]route, 0, len(api.Endpoints))

	// Create a map to track path patterns and their methods
	pathPatterns := make(map[string]map[string]bool) // path -> method -> exists

	// First pass: compile routes and collect path patterns
	for _, endpoint := range api.Endpoints {
		// Validate the handler signature up front so misconfiguration
		// fails fast at startup rather than as a 500/panic on traffic.
		if err := validateHandler(endpoint); err != nil {
			panic(err)
		}

		// Add the route to our compiled routes
		r := api.compileRoute(endpoint)
		api.routes = append(api.routes, r)

		// Track this path and method
		if pathPatterns[endpoint.Path] == nil {
			pathPatterns[endpoint.Path] = make(map[string]bool)
		}
		pathPatterns[endpoint.Path][endpoint.Method] = true

		// For parameterized paths, track all parent paths
		if strings.Contains(endpoint.Path, ":") {
			segments := strings.Split(endpoint.Path, "/")
			currentPath := ""

			for _, segment := range segments {
				if segment == "" {
					continue
				}

				currentPath += "/" + segment

				// If this segment doesn't contain a parameter, mark it as a potential conflict
				if !strings.Contains(segment, ":") {
					if pathPatterns[currentPath] == nil {
						pathPatterns[currentPath] = make(map[string]bool)
					}
					// Mark this path as potentially conflicting for all methods
					pathPatterns[currentPath]["*CONFLICT*"] = true
				} else {
					// Once we hit a parameter, we've gone far enough
					break
				}
			}
		}
	}

	// Sort routes by specificity (more specific routes first)
	sort.Slice(api.routes, func(i, j int) bool {
		// Routes with fewer parameters are more specific
		return len(api.routes[i].paramNames) < len(api.routes[j].paramNames)
	})

	// Second pass: register handlers with the mux
	needsCatchAll := false
	for _, endpoint := range api.Endpoints {
		exactPath := api.BasePath + endpoint.Path

		// Check if this path needs to go through the router:
		// 1. If it contains parameters
		// 2. If it conflicts with a parameterized path
		// 3. If there are multiple methods for this exact path
		needsRouter := strings.Contains(endpoint.Path, ":") ||
			pathPatterns[endpoint.Path]["*CONFLICT*"] ||
			len(pathPatterns[endpoint.Path]) > 1

		// If path doesn't need the router, register it directly
		if !needsRouter {
			mux.HandleFunc(exactPath, api.wrapHandler(endpoint))
		} else {
			needsCatchAll = true
		}
	}

	if needsCatchAll {
		// Register a catch-all handler for the base path to handle parameterized
		// routes and method-specific routing.
		catchAllPath := api.BasePath + "/"
		mux.HandleFunc(catchAllPath, api.routeHandler)
	}
}

// compileRoute converts an endpoint to a route with compiled regex
func (api *API) compileRoute(endpoint Endpoint) route {
	// Convert path template to regex pattern
	paramNames := []string{}
	patternStr := endpoint.Path

	// First check for trailing ellipsis parameters (e.g., :path...)
	reEllipsis := regexp.MustCompile(`:([a-zA-Z0-9_]+)\.\.\.`)
	hasEllipsis := reEllipsis.MatchString(patternStr)

	if hasEllipsis {
		// Make sure the ellipsis parameter is the last one in the path
		lastSlashIndex := strings.LastIndex(patternStr, "/")
		if lastSlashIndex != -1 {
			lastSegment := patternStr[lastSlashIndex+1:]
			if !reEllipsis.MatchString(lastSegment) {
				// Ellipsis parameter is not the last segment, which is not allowed
				// Fall back to standard behavior
				hasEllipsis = false
			}
		}
	}

	if hasEllipsis {
		// Handle the ellipsis parameter
		patternStr = reEllipsis.ReplaceAllStringFunc(patternStr, func(match string) string {
			paramName := match[1 : len(match)-3] // Remove the leading colon and trailing ...
			paramNames = append(paramNames, paramName)
			return `(.*)` // Match any characters including slashes
		})
	}

	// Handle regular parameters
	re := regexp.MustCompile(`:([a-zA-Z0-9_]+)`)
	patternStr = re.ReplaceAllStringFunc(patternStr, func(match string) string {
		// Skip if this is part of an ellipsis pattern that was already processed
		if strings.HasSuffix(match+"...", "...") && strings.Contains(endpoint.Path, match+"...") {
			return match
		}
		paramName := match[1:] // Remove the leading colon
		paramNames = append(paramNames, paramName)
		return `([^/]+)` // Match any characters except slash
	})

	// Complete the regex pattern
	patternStr = "^" + patternStr + "$"
	pattern := regexp.MustCompile(patternStr)

	// Create a handler function
	handlerFunc := api.wrapHandler(endpoint)

	return route{
		original:    endpoint.Path,
		pattern:     pattern,
		paramNames:  paramNames,
		endpoint:    endpoint,
		handlerFunc: handlerFunc,
	}
}

// routeHandler is the central handler that routes requests to the appropriate endpoint
func (api *API) routeHandler(w http.ResponseWriter, r *http.Request) {
	// Get path relative to BasePath
	path := strings.TrimPrefix(r.URL.Path, api.BasePath)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	// Find matching route. Track whether the path matched any route (for a
	// 405 vs 404 distinction) and which methods are allowed on it.
	pathMatched := false
	allowed := make([]string, 0, 4)
	seenMethod := make(map[string]bool, 4)
	for _, route := range api.routes {
		matches := route.pattern.FindStringSubmatch(path)
		if matches == nil {
			continue
		}
		pathMatched = true

		if r.Method != route.endpoint.Method {
			if !seenMethod[route.endpoint.Method] {
				seenMethod[route.endpoint.Method] = true
				allowed = append(allowed, route.endpoint.Method)
			}
			continue
		}

		// Extract path parameters
		params := make(map[string]string)
		for i, name := range route.paramNames {
			if i+1 < len(matches) {
				params[name] = matches[i+1]
			}
		}

		// Store path parameters in request context
		ctx := context.WithValue(r.Context(), PathParamsKey, params)
		r = r.WithContext(ctx)

		// Call the handler
		route.handlerFunc(w, r)
		return
	}

	// The path exists but not for this method -> 405 with an Allow header.
	if pathMatched {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// No route matched
	http.NotFound(w, r)
}

// GetPathParams retrieves path parameters from request context
func GetPathParams(ctx context.Context) map[string]string {
	params, ok := ctx.Value(PathParamsKey).(map[string]string)
	if !ok {
		return map[string]string{}
	}
	return params
}

// GetUser retrieves the authenticated user from the request context
func GetUser(ctx context.Context) (*User, bool) {
	user, ok := ctx.Value(UserContextKey).(*User)
	return user, ok
}

// PermissionChecker decides whether a user holds a given permission key.
// It is intentionally string-keyed so endpoints can declare arbitrary
// permissions (e.g. "admin", "vecforge.export", "user.delete") without
// extending this interface.
//
// Implementations are expected to be fast on the hot path -- typically
// backed by an in-memory cache with a few-second TTL (see authcache).
type PermissionChecker interface {
	HasPermission(ctx context.Context, userID int64, perm string) (bool, error)
}

// RequireAuthenticatedUser returns the current user or an HTTP 401 error.
func RequireAuthenticatedUser(ctx context.Context) (*User, *HTTPError) {
	user, ok := GetUser(ctx)
	if !ok || user == nil || user.ID == 0 {
		return nil, NewHTTPError(http.StatusUnauthorized, "Authentication required")
	}
	return user, nil
}

// RequirePermission returns the current user if authenticated and granted
// perm, otherwise an HTTP 401 / 403 / 500 error suitable for direct
// handler return. Use this from handler bodies when a static
// Endpoint.Permissions declaration is not flexible enough (e.g. the
// permission depends on request data).
func RequirePermission(ctx context.Context, checker PermissionChecker, perm string) (*User, *HTTPError) {
	user, httpErr := RequireAuthenticatedUser(ctx)
	if httpErr != nil {
		return nil, httpErr
	}
	if checker == nil {
		return nil, NewHTTPError(http.StatusInternalServerError, "Permission checker not configured")
	}
	ok, err := checker.HasPermission(ctx, user.ID, perm)
	if err != nil {
		return nil, NewHTTPError(http.StatusInternalServerError, "Failed to verify permission")
	}
	if !ok {
		return nil, NewHTTPError(http.StatusForbidden, "Permission denied: "+perm)
	}
	return user, nil
}

// wrapHandler creates a handler function that manages authentication and invokes the endpoint handler
func (api *API) wrapHandler(endpoint Endpoint) http.HandlerFunc {
	// Endpoints that declare Permissions implicitly require auth.
	authLevel := endpoint.AuthLevel
	if len(endpoint.Permissions) > 0 && authLevel != AuthRequired {
		authLevel = AuthRequired
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Directly-registered routes are single-method; enforce it so a
		// GET endpoint doesn't silently answer POST/PUT/DELETE/... The
		// router path (routeHandler) performs the equivalent check.
		if r.Method != endpoint.Method {
			w.Header().Set("Allow", endpoint.Method)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Step 1: Check if authentication is required
		var user *User

		if authLevel != AuthNone {
			session, err := api.SessionProvider.GetSession(r)
			if err != nil {
				api.logger().Printf("Session error: %v", err)
				http.Error(w, "Session error", http.StatusInternalServerError)
				return
			}

			// Get user ID from session
			userID, authenticated := session.GetUserID()
			if (!authenticated || userID == 0) && authLevel == AuthRequired {
				// Redirect to login for GET requests, return 401 for others
				if r.Method == http.MethodGet {
					http.Redirect(w, r, api.LoginPath+"?redirect="+r.URL.String(), http.StatusFound)
				} else {
					http.Error(w, "Authentication required", http.StatusUnauthorized)
				}
				return
			}

			if authenticated && userID > 0 {
				user = &User{
					ID:    userID,
					State: session.GetUserState(),
				}
				if dn, ok := session.(displayNameSession); ok {
					user.DisplayName = dn.GetDisplayName()
				}

				// Check if user account is in valid state
				if authLevel == AuthRequired {
					switch user.State {
					case UserStateComplete:
						// fully usable account
					case UserStatePendingDelete:
						// Account is inside its deletion grace period:
						// locked down to the cancel-deletion / data-export
						// / take-out endpoints that opt in explicitly.
						if !endpoint.AllowPendingDelete {
							http.Error(w, "Account is pending deletion", http.StatusForbidden)
							return
						}
					default:
						http.Error(w, "Account not fully activated", http.StatusForbidden)
						return
					}
				}
			}
		}

		// Step 2: Set up context with user information
		ctx := r.Context()
		if user != nil {
			ctx = context.WithValue(ctx, UserContextKey, user)
			r = r.WithContext(ctx)
		}

		// Step 3: Per-endpoint permission gate. Runs BEFORE body decode so a
		// non-permitted user cannot force us to buffer a large request body.
		if len(endpoint.Permissions) > 0 {
			for _, p := range endpoint.Permissions {
				if _, httpErr := RequirePermission(ctx, api.PermissionChecker, p); httpErr != nil {
					if httpErr.Code == http.StatusInternalServerError {
						api.logger().Printf("permission check failed for perm=%s: %s", p, httpErr.Message)
					}
					http.Error(w, httpErr.Message, httpErr.Code)
					return
				}
			}
		}

		// Step 4: Cap request body BEFORE we hand it to processRequestBody.
		// Even endpoints without a JSON body benefit: a misbehaving client
		// can't ship gigabytes of body to be discarded.
		bodyCap := endpoint.MaxBodyBytes
		if bodyCap == 0 {
			bodyCap = api.MaxBodyBytes
			if bodyCap == 0 {
				bodyCap = DefaultMaxBodyBytes
			}
		}
		if bodyCap > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, bodyCap)
		}

		// Step 5: Process the request
		handlerValue := reflect.ValueOf(endpoint.Handler)
		handlerType := handlerValue.Type()
		api.handleRequest(w, r, handlerValue, handlerType)
	}
}

// getOrderedParamNames gets the parameter names in the correct order for the current route
func (api *API) getOrderedParamNames(r *http.Request, params map[string]string) []string {
	// Find the matching route to get the parameter names in the correct order
	path := strings.TrimPrefix(r.URL.Path, api.BasePath)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	var orderedParamNames []string
	for _, route := range api.routes {
		if r.Method != route.endpoint.Method {
			continue
		}

		matches := route.pattern.FindStringSubmatch(path)
		if matches != nil {
			// Found the matching route, use its paramNames in the correct order
			orderedParamNames = route.paramNames
			break
		}
	}

	// If no route matched, fall back to alphabetical order
	if orderedParamNames == nil {
		orderedParamNames = make([]string, 0, len(params))
		for name := range params {
			orderedParamNames = append(orderedParamNames, name)
		}
		sort.Strings(orderedParamNames)
	}

	return orderedParamNames
}

// prepareHandlerArgs prepares arguments for the handler function
func (api *API) prepareHandlerArgs(w http.ResponseWriter, r *http.Request, handlerType reflect.Type) ([]reflect.Value, bool) {
	// Get method signature information
	numIn := handlerType.NumIn()
	if numIn < 1 {
		http.Error(w, "Internal server error: invalid handler", http.StatusInternalServerError)
		return nil, false
	}

	// First argument must be *http.Request
	if handlerType.In(0) != reflect.TypeOf(r) {
		http.Error(w, "Internal server error: invalid handler", http.StatusInternalServerError)
		return nil, false
	}

	// Prepare arguments for the handler function
	args := make([]reflect.Value, numIn)
	args[0] = reflect.ValueOf(r)

	return args, true
}

// processRequestBody processes the request body for data parameter
func (api *API) processRequestBody(w http.ResponseWriter, r *http.Request, args []reflect.Value, handlerType reflect.Type) (interface{}, bool) {
	if handlerType.NumIn() < 2 {
		return nil, true
	}

	paramType := handlerType.In(1)
	if paramType.Kind() != reflect.Map && paramType.Kind() != reflect.Struct {
		return nil, true
	}

	if !isJSONContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return nil, false
	}

	// Create a new instance of the parameter type
	paramValue := reflect.New(paramType).Interface()

	// Decode the request body into the parameter
	decoder := json.NewDecoder(r.Body)
	err := decoder.Decode(paramValue)
	if err != nil {
		if errors.Is(err, io.EOF) && paramType.Kind() == reflect.Struct && paramType.NumField() == 0 {
			args[1] = reflect.ValueOf(paramValue).Elem()
			return paramValue, true
		} else if errors.Is(err, io.EOF) {
			http.Error(w, "Request body is required", http.StatusBadRequest)
		} else {
			http.Error(w, "Invalid request body", http.StatusBadRequest)
		}
		return nil, false
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "Invalid request body: multiple JSON values", http.StatusBadRequest)
		return nil, false
	}

	// Store the parameter value
	args[1] = reflect.ValueOf(paramValue).Elem()
	return paramValue, true
}

func isJSONContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// processPathAndQueryParams processes path and query parameters
func (api *API) processPathAndQueryParams(w http.ResponseWriter, r *http.Request, args []reflect.Value, handlerType reflect.Type,
	orderedParamNames []string, params map[string]string, startIdx int) bool {

	numIn := handlerType.NumIn()
	paramIndex := 0

	for i := startIdx; i < numIn; i++ {
		paramType := handlerType.In(i)

		// Handle different parameter types
		if paramType.Kind() == reflect.String {
			// Use the parameter in the correct order from the route
			if paramIndex < len(orderedParamNames) {
				paramName := orderedParamNames[paramIndex]
				if value, ok := params[paramName]; ok {
					args[i] = reflect.ValueOf(value)
					paramIndex++
				}
			}

			// If no value was found, return an error
			if args[i].Kind() == reflect.Invalid {
				http.Error(w, "Missing required parameter", http.StatusBadRequest)
				return false
			}
		} else if paramType.Kind() == reflect.Int || paramType.Kind() == reflect.Int64 || paramType.Kind() == reflect.Int32 {
			// Bind from the next positional path parameter. Query inputs
			// are bound via a struct parameter (handled below): Go
			// reflection cannot recover a scalar parameter's name, so a
			// bare int/string param can only come from the path.
			if paramIndex < len(orderedParamNames) {
				paramName := orderedParamNames[paramIndex]
				if value, ok := params[paramName]; ok {
					intValue, err := strconv.ParseInt(value, 10, 64)
					if err == nil {
						args[i] = reflect.ValueOf(intValue).Convert(paramType)
						paramIndex++
					}
				}
			}

			// If no value was found, return an error
			if args[i].Kind() == reflect.Invalid {
				http.Error(w, "Missing required parameter", http.StatusBadRequest)
				return false
			}
		} else if paramType.Kind() == reflect.Struct {
			// For struct parameters, read query parameters
			structValue := reflect.New(paramType).Elem()
			for j := 0; j < paramType.NumField(); j++ {
				field := paramType.Field(j)
				tag := field.Tag.Get("json")
				if tag == "" {
					tag = strings.ToLower(field.Name)
				} else {
					tag = strings.Split(tag, ",")[0]
				}

				queryValue := r.URL.Query().Get(tag)
				if queryValue != "" {
					fieldValue := structValue.Field(j)
					if fieldValue.Kind() == reflect.String {
						fieldValue.SetString(queryValue)
					} else if fieldValue.Kind() == reflect.Int || fieldValue.Kind() == reflect.Int64 {
						intValue, err := strconv.ParseInt(queryValue, 10, 64)
						if err == nil {
							fieldValue.SetInt(intValue)
						}
					}
				}
			}
			args[i] = structValue
		} else if paramType.Kind() == reflect.Slice && paramType.Elem().Kind() == reflect.String {
			// For variadic string parameters, use remaining path parameters in order
			variadic := make([]string, 0)

			// Use all remaining parameters in their correct order
			for j := paramIndex; j < len(orderedParamNames); j++ {
				paramName := orderedParamNames[j]
				if value, ok := params[paramName]; ok {
					variadic = append(variadic, value)
				}
			}

			// Create a slice of the appropriate type and populate it
			sliceValue := reflect.MakeSlice(paramType, len(variadic), len(variadic))
			for j, val := range variadic {
				sliceValue.Index(j).Set(reflect.ValueOf(val))
			}
			args[i] = sliceValue
			break // We've handled all remaining parameters
		} else {
			// Unsupported parameter type
			http.Error(w, "Internal server error: unsupported parameter type", http.StatusInternalServerError)
			return false
		}
	}

	return true
}

// callHandlerAndProcessResults calls the handler function and processes the results
func (api *API) callHandlerAndProcessResults(w http.ResponseWriter, r *http.Request, handlerValue reflect.Value, handlerType reflect.Type, args []reflect.Value) {
	// Call the handler function
	var results []reflect.Value
	if handlerType.IsVariadic() {
		results = handlerValue.CallSlice(args)
	} else {
		results = handlerValue.Call(args)
	}

	// Process the result
	api.processResults(w, r, results)
}

// handleRequest processes any HTTP request and invokes the handler function.
// Methods that carry a request body (POST, PUT, PATCH) will attempt to decode
// the body into the handler's second parameter if it is a struct or map.
func (api *API) handleRequest(w http.ResponseWriter, r *http.Request, handlerValue reflect.Value, handlerType reflect.Type) {
	// Prepare arguments for the handler function
	args, ok := api.prepareHandlerArgs(w, r, handlerType)
	if !ok {
		return
	}

	// Extract path parameters
	params := GetPathParams(r.Context())

	// For methods that carry a body, try to decode the request body
	startIdx := 1
	hasBody := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch
	if hasBody {
		requestData, ok := api.processRequestBody(w, r, args, handlerType)
		if !ok {
			return
		}
		if requestData != nil {
			startIdx = 2
		}
	}

	// Get ordered parameter names
	orderedParamNames := api.getOrderedParamNames(r, params)

	// Process path and query parameters
	if !api.processPathAndQueryParams(w, r, args, handlerType, orderedParamNames, params, startIdx) {
		return
	}

	// Call the handler function and process results
	api.callHandlerAndProcessResults(w, r, handlerValue, handlerType, args)
}

// processResults handles the return values from handler functions
func (api *API) processResults(w http.ResponseWriter, r *http.Request, results []reflect.Value) {
	if len(results) != 2 {
		http.Error(w, "Internal server error: invalid handler return values", http.StatusInternalServerError)
		return
	}

	// Check for error
	errValue := results[1]
	if !reflectValueIsNil(errValue) {
		// Get the error message
		err := errValue.Interface().(error)

		// Check if it's an HTTPError
		if httpErr, ok := err.(*HTTPError); ok {
			if httpErr.Details != nil {
				// Emit structured JSON so the client can render a rich
				// error UI (e.g. quota-exceeded dialog).
				body, mErr := json.Marshal(map[string]interface{}{
					"error":   httpErr.Message,
					"details": httpErr.Details,
				})
				if mErr == nil {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(httpErr.Code)
					_, _ = w.Write(body)
					return
				}
				// Fall through to plain-text on marshal failure.
			}
			http.Error(w, httpErr.Message, httpErr.Code)
		} else {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Get the result value
	resultValue := results[0]
	if reflectValueIsNil(resultValue) {
		// No result, return 204 No Content
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Get the result
	result := resultValue.Interface()

	// Check if it's a CookieResponse
	if cookieResponse, ok := result.(*CookieResponse); ok {
		// Set cookies
		for _, cookie := range cookieResponse.Cookies {
			http.SetCookie(w, cookie)
		}

		// Process the actual response data
		if cookieResponse.Data == nil {
			// No data, return 204 No Content
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Marshal the data to JSON
		jsonData, err := json.Marshal(cookieResponse.Data)
		if err != nil {
			http.Error(w, "Failed to marshal response: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Set content type and write response
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonData)
		return
	}

	// Check if it's a BinaryResponse
	if binaryResponse, ok := result.(*BinaryResponse); ok {
		// Set content type if provided
		if binaryResponse.ContentType != "" {
			w.Header().Set("Content-Type", binaryResponse.ContentType)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}

		// Set content encoding if provided
		if binaryResponse.ContentEncoding != "" {
			w.Header().Set("Content-Encoding", binaryResponse.ContentEncoding)
		}

		// Write binary data
		_, _ = w.Write(binaryResponse.Data)
		return
	}

	// Check if it's a StreamResponse
	if streamResponse, ok := result.(*StreamResponse); ok {
		if streamResponse.Reader == nil {
			http.Error(w, "Invalid stream response", http.StatusInternalServerError)
			return
		}
		defer func() { _ = streamResponse.Reader.Close() }()

		if streamResponse.ContentType != "" {
			w.Header().Set("Content-Type", streamResponse.ContentType)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		if streamResponse.ContentEncoding != "" {
			w.Header().Set("Content-Encoding", streamResponse.ContentEncoding)
		}
		if streamResponse.ContentLength > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(streamResponse.ContentLength, 10))
		}
		for key, value := range streamResponse.Headers {
			w.Header().Set(key, value)
		}
		if _, err := io.Copy(w, streamResponse.Reader); err != nil {
			api.logger().Printf("stream response copy failed: %v", err)
		}
		return
	}

	// Check if it's an EventStreamResponse (Server-Sent Events)
	if eventStreamResponse, ok := result.(*EventStreamResponse); ok {
		api.writeEventStream(w, r, eventStreamResponse)
		return
	}

	// For other types, marshal to JSON
	jsonData, err := json.Marshal(result)
	if err != nil {
		http.Error(w, "Failed to marshal response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Set content type and write response
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(jsonData)
}
