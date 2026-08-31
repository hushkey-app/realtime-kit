// Package api is the sidecar's HTTP surface: four JSON endpoints over the
// LiveKit gateway, plus a health check.
//
// # Trust model
//
// This service is NOT internet-facing and must not be exposed as such. Callers
// pass LiveKit API secrets through it in request bodies, so it belongs on a
// private network (or a loopback/unix hop) between the app and itself, behind
// TLS if that network is not already private. Every /v1 route additionally
// requires a shared bearer token, compared in constant time — a defence in
// depth, not a substitute for the network boundary.
//
// # What never appears in a log
//
// Request bodies. They carry LiveKit API secrets on the way in and a signed join
// token on the way out. The access log records method, path, status and
// duration; handlers log room names and identities at debug level at most.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/hushkey-app/realtime-kit/internal/livekit"
)

// Options configures a Server.
type Options struct {
	// Token is the shared secret callers present as `Authorization: Bearer …`.
	Token string
	// MaxRequestSize caps a request body. Every request here is small JSON; the
	// cap is what stops an unbounded read from a peer that has gone wrong.
	MaxRequestSize int64
	Logger         *slog.Logger
}

// Server routes HTTP requests onto the LiveKit gateway.
type Server struct {
	client *livekit.Client
	opts   Options
	mux    *http.ServeMux
}

// New builds the server and registers its routes.
func New(client *livekit.Client, opts Options) *Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxRequestSize <= 0 {
		opts.MaxRequestSize = 64 << 10
	}

	s := &Server{client: client, opts: opts, mux: http.NewServeMux()}

	// Unauthenticated on purpose: a load balancer's health probe holds no token,
	// and the response says nothing a prober could not already infer.
	s.mux.HandleFunc("GET /healthz", s.handleHealth)

	s.mux.Handle("POST /v1/sessions", s.authed(s.handleSession))
	s.mux.Handle("POST /v1/rooms/delete", s.authed(s.handleDeleteRoom))
	s.mux.Handle("POST /v1/rooms/participants", s.authed(s.handleParticipants))
	s.mux.Handle("POST /v1/verify", s.authed(s.handleVerify))

	return s
}

// Handler is the server as one http.Handler, access log included.
func (s *Server) Handler() http.Handler {
	return s.logRequests(s.mux)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.Handler().ServeHTTP(w, r)
}

// --- middleware --------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests writes one line per request. Deliberately no body, no headers:
// the body carries provider secrets and the Authorization header carries ours.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.opts.Logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// authed requires the shared bearer token.
//
// The comparison is constant-time, and a missing header is refused with exactly
// the same response as a wrong one: the two must not be distinguishable, or the
// difference becomes an oracle for whether a token was even expected.
func (s *Server) authed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := strings.TrimSpace(
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		)
		if subtle.ConstantTimeCompare([]byte(presented), []byte(s.opts.Token)) != 1 {
			writeError(w, &livekit.Error{
				Status:  http.StatusUnauthorized,
				Code:    "unauthorized",
				Message: "Invalid or missing bearer token",
			})
			return
		}
		next(w, r)
	})
}

// --- handlers ----------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "realtime-kit"})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	var req livekit.SessionRequest
	if !s.decode(w, r, &req) {
		return
	}

	res, err := s.client.Session(r.Context(), req)
	if err != nil {
		s.fail(w, "session", err)
		return
	}

	s.opts.Logger.Debug("session minted", "room", res.Room, "identity", res.Identity)
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	var req livekit.RoomRequest
	if !s.decode(w, r, &req) {
		return
	}

	if err := s.client.DeleteRoom(r.Context(), req); err != nil {
		s.fail(w, "delete room", err)
		return
	}

	s.opts.Logger.Debug("room deleted", "room", req.Room)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleParticipants(w http.ResponseWriter, r *http.Request) {
	var req livekit.RoomRequest
	if !s.decode(w, r, &req) {
		return
	}

	participants, err := s.client.Participants(r.Context(), req)
	if err != nil {
		s.fail(w, "list participants", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"participants": participants})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Credentials livekit.Credentials `json:"credentials"`
	}
	if !s.decode(w, r, &req) {
		return
	}

	rooms, err := s.client.Verify(r.Context(), req.Credentials)
	if err != nil {
		s.fail(w, "verify", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rooms": rooms})
}

// --- plumbing ----------------------------------------------------------------

// decode reads the JSON body into `target`, answering the request itself and
// returning false when it can't. Unknown fields are rejected so a caller that
// misspells `apiSecret` learns it here rather than from LiveKit's 401.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, target any) bool {
	defer func() { _ = r.Body.Close() }()

	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.opts.MaxRequestSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		// Never echo the decoder's message: it quotes the offending input, and
		// the offending input may be the line holding the API secret.
		writeError(w, &livekit.Error{
			Status:  http.StatusBadRequest,
			Code:    "invalid_request",
			Message: "Request body is not valid JSON for this endpoint",
		})
		return false
	}
	return true
}

// fail answers with the error's own status when it decided one, and a bad
// gateway otherwise. Logged at warn without the request that produced it.
func (s *Server) fail(w http.ResponseWriter, op string, err error) {
	var apiErr *livekit.Error
	if !errors.As(err, &apiErr) {
		apiErr = &livekit.Error{
			Status:  http.StatusBadGateway,
			Code:    "upstream_error",
			Message: "LiveKit call failed",
		}
	}
	s.opts.Logger.Warn("livekit call failed",
		"op", op, "code", apiErr.Code, "status", apiErr.Status, "message", apiErr.Message)
	writeError(w, apiErr)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A minted token must not sit in any cache between here and the app.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, err *livekit.Error) {
	writeJSON(w, err.Status, map[string]any{"error": err})
}
