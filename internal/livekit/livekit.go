// Package livekit wraps github.com/livekit/server-sdk-go with the four
// operations this sidecar exposes: mint a join session, delete a room, list a
// room's participants, and prove a set of credentials works.
//
// # Multi-tenant by construction
//
// The service stores no LiveKit credentials. Every call names the deployment it
// is for (URL, API key, API secret), so one process can serve many workspaces on
// many LiveKit clusters — which is the whole point, because the caller (pakku)
// is BYO: each workspace brings its own LiveKit account and its own key.
//
// The API clients themselves are cached, keyed by a hash of the credentials, so
// a hot channel reuses one connection pool instead of dialling per join. The
// cache holds the *client*, and the client holds the key and secret it signs
// with — so entries are capped and evicted, never unbounded.
//
// # Secrets
//
// An API secret enters through a request body, is used to sign, and is never
// logged, never returned, and never interpolated into an error. Errors carry
// LiveKit's own words and nothing of the request that produced them.
package livekit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/twitchtv/twirp"
	"google.golang.org/protobuf/proto"
)

const (
	// How long a minted join token is valid. Deliberately short: the caller
	// re-mints through its own permission checks on every join, so a token that
	// outlives the membership that justified it is the thing to avoid.
	defaultTokenTTL = 30 * time.Minute
	maxTokenTTL     = 6 * time.Hour

	// How long LiveKit keeps a room alive with nobody in it. A voice channel that
	// empties for a moment (everyone reloads) should not lose its room, but an
	// abandoned one must not be held open forever.
	defaultEmptyTimeout = 5 * time.Minute

	// Cached API clients. One entry per (url, key, secret); a deployment serving
	// a few hundred workspaces stays well inside this.
	maxCachedClients = 512
)

// Credentials names one LiveKit deployment and the key to act on it with.
type Credentials struct {
	// URL is the LiveKit host, ws:// wss:// http:// or https:// — the SDK
	// normalises it for the server API, and the browser gets it as given.
	URL string `json:"url"`
	// APIKey identifies the key pair; not secret on its own.
	APIKey string `json:"apiKey"`
	// APISecret signs tokens and API calls. Secret.
	APISecret string `json:"apiSecret"`
}

// Validate reports whether the credentials are structurally usable. It proves
// nothing about whether LiveKit accepts them — that is Verify's job.
func (c Credentials) Validate() error {
	if strings.TrimSpace(c.URL) == "" {
		return errors.New("url is required")
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return errors.New("apiKey is required")
	}
	if strings.TrimSpace(c.APISecret) == "" {
		return errors.New("apiSecret is required")
	}
	if !hasKnownScheme(c.URL) {
		return errors.New("url must start with ws://, wss://, http:// or https://")
	}
	return nil
}

func hasKnownScheme(url string) bool {
	lower := strings.ToLower(strings.TrimSpace(url))
	for _, scheme := range []string{"ws://", "wss://", "http://", "https://"} {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

// fingerprint is the cache key for a credential set. Hashed rather than
// concatenated so the secret never sits in a map key that could be ranged over
// into a log line.
func (c Credentials) fingerprint() string {
	sum := sha256.Sum256([]byte(c.URL + "\x00" + c.APIKey + "\x00" + c.APISecret))
	return hex.EncodeToString(sum[:])
}

// Client is the sidecar's LiveKit gateway. Safe for concurrent use.
type Client struct {
	timeout time.Duration

	mu    sync.Mutex
	cache map[string]*lksdk.LiveKitAPI
	// Insertion order of `cache` keys, for the eviction below.
	order []string
}

// New builds a gateway whose upstream calls are bounded by `timeout`.
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{timeout: timeout, cache: map[string]*lksdk.LiveKitAPI{}}
}

// api returns the cached LiveKit API client for these credentials, building one
// on first use. Eviction is oldest-first and only on overflow: these clients are
// cheap to rebuild, so a wrong guess costs one handshake.
func (c *Client) api(creds Credentials) (*lksdk.LiveKitAPI, error) {
	key := creds.fingerprint()

	c.mu.Lock()
	defer c.mu.Unlock()

	if client, ok := c.cache[key]; ok {
		return client, nil
	}

	client, err := lksdk.NewLiveKitAPI(
		lksdk.WithURL(creds.URL),
		lksdk.WithAPIKey(creds.APIKey, creds.APISecret),
	)
	if err != nil {
		// The SDK's construction errors are about the arguments we just passed,
		// so they are safe to surface — but they are also the only place a URL
		// could appear, hence the fixed message.
		return nil, &Error{Status: http.StatusBadRequest, Code: "invalid_credentials",
			Message: "LiveKit credentials are incomplete"}
	}

	if len(c.order) >= maxCachedClients {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.cache, oldest)
	}
	c.cache[key] = client
	c.order = append(c.order, key)
	return client, nil
}

// SessionRequest asks for one participant's way into one room.
type SessionRequest struct {
	Credentials Credentials `json:"credentials"`
	// Room is the LiveKit room name. The caller owns the namespace; this service
	// neither derives nor rewrites it.
	Room string `json:"room"`
	// RoomMetadata is set on the room at creation only — LiveKit ignores it for a
	// room that already exists, and this service does not overwrite it.
	RoomMetadata string `json:"roomMetadata,omitempty"`
	// Identity is the participant's stable id. The caller sets it to its OWN user
	// id: it is the only thing tying LiveKit's roster back to the caller's
	// presence, and the browser correlates on it.
	Identity string `json:"identity"`
	// Name is the display name LiveKit shows on its own surfaces.
	Name string `json:"name,omitempty"`
	// Metadata rides on the participant, visible to everyone in the room. Never
	// put anything private in it.
	Metadata string `json:"metadata,omitempty"`
	// Admin grants roomAdmin — muting others, removing them. Hosts only.
	Admin bool `json:"admin,omitempty"`
	// CanPublish/CanSubscribe default to true when omitted; a listener-only
	// participant sets CanPublish to false explicitly.
	CanPublish   *bool `json:"canPublish,omitempty"`
	CanSubscribe *bool `json:"canSubscribe,omitempty"`
	// TTLSeconds overrides the default token lifetime, capped at six hours.
	TTLSeconds int `json:"ttlSeconds,omitempty"`
	// EmptyTimeoutSeconds is how long LiveKit holds the room open with nobody in
	// it. Applied at creation only.
	EmptyTimeoutSeconds int `json:"emptyTimeoutSeconds,omitempty"`
	// MaxParticipants caps the room at creation; 0 means LiveKit's own default.
	MaxParticipants int `json:"maxParticipants,omitempty"`
}

func (r SessionRequest) validate() error {
	if err := r.Credentials.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Room) == "" {
		return errors.New("room is required")
	}
	if strings.TrimSpace(r.Identity) == "" {
		return errors.New("identity is required")
	}
	return nil
}

// SessionResponse is everything the browser needs to connect.
type SessionResponse struct {
	// URL is echoed back exactly as given, so the caller hands the browser one
	// value it did not have to reassemble.
	URL string `json:"url"`
	// Room is the room name, and RoomSID LiveKit's own id for it.
	Room    string `json:"room"`
	RoomSID string `json:"roomSid,omitempty"`
	// Token is the signed join token. Short-lived, single participant, single
	// room. Never logged by this service.
	Token string `json:"token"`
	// ExpiresAt is Unix milliseconds, so a JS caller can use it as-is.
	ExpiresAt int64 `json:"expiresAt"`
	// Identity is echoed so the caller can assert the token is for who it asked.
	Identity string `json:"identity"`
}

// Session ensures the room exists and mints this participant's join token.
//
// One call rather than two because it sits on the join path: a user waits on it,
// and a second round trip to the sidecar would be a second chance to be slow.
// Creating the room explicitly (rather than relying on LiveKit's auto-create)
// is what lets the caller set the empty timeout and the participant cap, and it
// works whether or not the deployment allows auto-create.
func (c *Client) Session(ctx context.Context, req SessionRequest) (*SessionResponse, error) {
	if err := req.validate(); err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}

	client, err := c.api(req.Credentials)
	if err != nil {
		return nil, err
	}

	emptyTimeout := defaultEmptyTimeout
	if req.EmptyTimeoutSeconds > 0 {
		emptyTimeout = time.Duration(req.EmptyTimeoutSeconds) * time.Second
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// CreateRoom is idempotent: LiveKit returns the existing room untouched when
	// one is already open under this name, so every join can call it.
	room, err := client.Room().CreateRoom(callCtx, &livekit.CreateRoomRequest{
		Name:            req.Room,
		Metadata:        req.RoomMetadata,
		EmptyTimeout:    uint32(emptyTimeout.Seconds()),
		MaxParticipants: uint32(req.MaxParticipants),
	})
	if err != nil {
		return nil, wrap(err, "create room")
	}

	ttl := defaultTokenTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl > maxTokenTTL {
		ttl = maxTokenTTL
	}

	grant := &auth.VideoGrant{
		RoomJoin:     true,
		Room:         req.Room,
		RoomAdmin:    req.Admin,
		CanPublish:   boolPtr(req.CanPublish, true),
		CanSubscribe: boolPtr(req.CanSubscribe, true),
		// Data messages are how a client signals anything the media path can't
		// carry. Harmless, and off by omission is a surprising default.
		CanPublishData: proto.Bool(true),
	}

	token, err := auth.NewAccessToken(req.Credentials.APIKey, req.Credentials.APISecret).
		SetIdentity(req.Identity).
		SetName(req.Name).
		SetMetadata(req.Metadata).
		SetVideoGrant(grant).
		SetValidFor(ttl).
		ToJWT()
	if err != nil {
		// Signing fails on a malformed key, not on anything LiveKit said.
		return nil, &Error{Status: http.StatusBadRequest, Code: "invalid_credentials",
			Message: "LiveKit rejected these credentials"}
	}

	return &SessionResponse{
		URL:       req.Credentials.URL,
		Room:      req.Room,
		RoomSID:   room.GetSid(),
		Token:     token,
		ExpiresAt: time.Now().Add(ttl).UnixMilli(),
		Identity:  req.Identity,
	}, nil
}

// RoomRequest names one room on one deployment.
type RoomRequest struct {
	Credentials Credentials `json:"credentials"`
	Room        string      `json:"room"`
}

func (r RoomRequest) validate() error {
	if err := r.Credentials.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Room) == "" {
		return errors.New("room is required")
	}
	return nil
}

// DeleteRoom ends a room and disconnects everyone still in it.
//
// A room LiveKit has already forgotten is not an error here: the caller deletes
// on channel deletion and on provider switch, both of which routinely run after
// the room has aged out, and a 404 would make those paths noisy for no reason.
func (c *Client) DeleteRoom(ctx context.Context, req RoomRequest) error {
	if err := req.validate(); err != nil {
		return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}

	client, err := c.api(req.Credentials)
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	_, err = client.Room().DeleteRoom(callCtx, &livekit.DeleteRoomRequest{Room: req.Room})
	if err != nil {
		if wrapped := wrap(err, "delete room"); wrapped.Status != http.StatusNotFound {
			return wrapped
		}
	}
	return nil
}

// Participant is one person LiveKit currently has in a room.
type Participant struct {
	// Identity is the caller's own user id — see SessionRequest.Identity.
	Identity string `json:"identity"`
	SID      string `json:"sid"`
	Name     string `json:"name,omitempty"`
	State    string `json:"state"`
	Metadata string `json:"metadata,omitempty"`
	JoinedAt int64  `json:"joinedAt,omitempty"`
}

// Participants lists who LiveKit has in a room right now.
//
// The caller's own roster stays authoritative for presence; this exists for
// reconciliation and support ("LiveKit thinks who is in there?"), not as a
// source of truth.
func (c *Client) Participants(ctx context.Context, req RoomRequest) ([]Participant, error) {
	if err := req.validate(); err != nil {
		return nil, &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}

	client, err := c.api(req.Credentials)
	if err != nil {
		return nil, err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	res, err := client.Room().ListParticipants(callCtx, &livekit.ListParticipantsRequest{Room: req.Room})
	if err != nil {
		wrapped := wrap(err, "list participants")
		// A room nobody is in and a room that never existed are the same answer.
		if wrapped.Status == http.StatusNotFound {
			return []Participant{}, nil
		}
		return nil, wrapped
	}

	out := make([]Participant, 0, len(res.GetParticipants()))
	for _, p := range res.GetParticipants() {
		out = append(out, Participant{
			Identity: p.GetIdentity(),
			SID:      p.GetSid(),
			Name:     p.GetName(),
			State:    p.GetState().String(),
			Metadata: p.GetMetadata(),
			JoinedAt: p.GetJoinedAt(),
		})
	}
	return out, nil
}

// Verify proves a credential set works, with the cheapest authenticated call
// LiveKit offers. It is the save-path smoke test: the caller shows the user
// whatever this refuses with.
func (c *Client) Verify(ctx context.Context, creds Credentials) (int, error) {
	if err := creds.Validate(); err != nil {
		return 0, &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: err.Error()}
	}

	client, err := c.api(creds)
	if err != nil {
		return 0, err
	}

	callCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	res, err := client.Room().ListRooms(callCtx, &livekit.ListRoomsRequest{})
	if err != nil {
		return 0, wrap(err, "list rooms")
	}
	return len(res.GetRooms()), nil
}

func boolPtr(value *bool, fallback bool) *bool {
	if value != nil {
		return proto.Bool(*value)
	}
	return proto.Bool(fallback)
}

// Error is a failure with an HTTP status already decided, so the transport layer
// does no interpretation of its own.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// wrap turns an SDK failure into an Error carrying LiveKit's own words.
//
// Twirp errors are LiveKit's considered refusals and are surfaced as such. A
// transport fault is not: twirp reports one as an `Internal` error whose message
// is the underlying `*url.Error`, and that text names the request. It is
// replaced wholesale rather than wrapped — the API secret travels in an
// Authorization header on every one of these calls, and a request object is one
// stringify away from carrying it.
func wrap(err error, op string) *Error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return &Error{
			Status:  http.StatusServiceUnavailable,
			Code:    "upstream_unavailable",
			Message: "LiveKit is unreachable",
		}
	}

	var twerr twirp.Error
	if !errors.As(err, &twerr) {
		return &Error{
			Status:  http.StatusBadGateway,
			Code:    "upstream_error",
			Message: fmt.Sprintf("LiveKit refused to %s", op),
		}
	}

	status := twirp.ServerHTTPStatusFromErrorCode(twerr.Code())
	switch twerr.Code() {
	case twirp.Unauthenticated, twirp.PermissionDenied:
		return &Error{
			Status:  http.StatusUnauthorized,
			Code:    "invalid_credentials",
			Message: "LiveKit rejected these credentials",
		}
	case twirp.NotFound:
		return &Error{Status: http.StatusNotFound, Code: "not_found", Message: twerr.Msg()}
	case twirp.InvalidArgument, twirp.Malformed:
		return &Error{Status: http.StatusBadRequest, Code: "invalid_request", Message: twerr.Msg()}
	}

	message := twerr.Msg()
	if message == "" {
		message = fmt.Sprintf("LiveKit refused to %s", op)
	}
	if status < 400 {
		status = http.StatusBadGateway
	}
	return &Error{Status: status, Code: "upstream_error", Message: message}
}
