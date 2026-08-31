package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	lk "github.com/hushkey-app/realtime-kit/internal/livekit"
	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
)

const (
	sidecarToken = "a-thirty-two-character-shared-secret-value"
	apiKey       = "APIkey123"
	apiSecret    = "livekit-super-secret-value-do-not-log"
)

// fakeLiveKit is a stand-in LiveKit twirp API: it answers CreateRoom and
// ListRooms with canned protobuf, so these tests exercise the real handler, the
// real decoder and the real gateway without a LiveKit deployment.
func fakeLiveKit(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		rpc := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		var msg proto.Message
		switch rpc {
		case "CreateRoom":
			msg = &livekit.Room{Sid: "RM_test", Name: "channel-1"}
		case "ListRooms":
			msg = &livekit.ListRoomsResponse{}
		case "DeleteRoom":
			msg = &livekit.DeleteRoomResponse{}
		case "ListParticipants":
			msg = &livekit.ListParticipantsResponse{}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}

		body, err := proto.Marshal(msg)
		if err != nil {
			t.Errorf("marshalling %s: %v", rpc, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/protobuf")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func newServer(t *testing.T) http.Handler {
	t.Helper()
	return New(lk.New(5*time.Second), Options{
		Token:          sidecarToken,
		MaxRequestSize: 64 << 10,
		// Discard: these tests assert on responses, and a chatty logger in a test
		// run is noise either way.
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).Handler()
}

func post(t *testing.T, handler http.Handler, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshalling the request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	return res
}

func TestHealthNeedsNoToken(t *testing.T) {
	res := httptest.NewRecorder()
	newServer(t).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.Code)
	}
	if !strings.Contains(res.Body.String(), `"ok":true`) {
		t.Fatalf("unexpected body: %s", res.Body.String())
	}
}

// A missing token and a wrong one must be indistinguishable — the difference
// would tell an unauthenticated caller whether a token was expected at all.
func TestUnauthenticatedRequestsAreRefusedIdentically(t *testing.T) {
	handler := newServer(t)

	missing := post(t, handler, "/v1/verify", "", map[string]any{})
	wrong := post(t, handler, "/v1/verify", "not-the-token", map[string]any{})

	for name, res := range map[string]*httptest.ResponseRecorder{"missing": missing, "wrong": wrong} {
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s token: status = %d, want 401", name, res.Code)
		}
	}
	if missing.Body.String() != wrong.Body.String() {
		t.Fatalf("the two refusals differ:\n  missing: %s\n  wrong:   %s",
			missing.Body.String(), wrong.Body.String())
	}
}

// An unauthenticated caller must not reach LiveKit at all.
func TestUnauthenticatedRequestsNeverReachLiveKit(t *testing.T) {
	reached := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))
	defer upstream.Close()

	post(t, newServer(t), "/v1/sessions", "", map[string]any{
		"credentials": map[string]string{"url": upstream.URL, "apiKey": apiKey, "apiSecret": apiSecret},
		"room":        "channel-1",
		"identity":    "user-1",
	})

	if reached {
		t.Fatal("an unauthorized request was proxied to LiveKit")
	}
}

func TestSessionReturnsAJoinTicket(t *testing.T) {
	upstream := fakeLiveKit(t)

	res := post(t, newServer(t), "/v1/sessions", sidecarToken, map[string]any{
		"credentials": map[string]string{"url": upstream.URL, "apiKey": apiKey, "apiSecret": apiSecret},
		"room":        "channel-1",
		"identity":    "user-1",
		"name":        "Ada",
		"admin":       true,
	})
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", res.Code, res.Body.String())
	}
	if cache := res.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("Cache-Control = %q; a minted token must not be cacheable", cache)
	}

	var body lk.SessionResponse
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the response: %v", err)
	}
	if body.RoomSID != "RM_test" || body.Room != "channel-1" {
		t.Fatalf("unexpected room: %+v", body)
	}

	verifier, err := auth.ParseAPIToken(body.Token)
	if err != nil {
		t.Fatalf("the response token does not parse: %v", err)
	}
	if _, _, err := verifier.Verify(apiSecret); err != nil {
		t.Fatalf("the response token is not signed with the credentials sent: %v", err)
	}
}

// The API secret must not come back out of this service, in any field.
func TestSessionResponseCarriesNoSecret(t *testing.T) {
	upstream := fakeLiveKit(t)

	res := post(t, newServer(t), "/v1/sessions", sidecarToken, map[string]any{
		"credentials": map[string]string{"url": upstream.URL, "apiKey": apiKey, "apiSecret": apiSecret},
		"room":        "channel-1",
		"identity":    "user-1",
	})

	if strings.Contains(res.Body.String(), apiSecret) {
		t.Fatalf("the response echoes the API secret: %s", res.Body.String())
	}
}

// A misspelled field is a caller bug; failing here beats a puzzling 401 from
// LiveKit later.
func TestUnknownFieldsAreRejected(t *testing.T) {
	res := post(t, newServer(t), "/v1/sessions", sidecarToken, map[string]any{
		"credentials": map[string]string{"url": "wss://x", "apiKey": "k", "apiSecret": "s"},
		"room":        "channel-1",
		"identity":    "user-1",
		"typo":        true,
	})

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
}

// A rejected body must not be quoted back: the offending line may be the one
// holding the secret.
func TestMalformedBodiesAreNotEchoed(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions",
		strings.NewReader(`{"credentials":{"apiSecret":"`+apiSecret+`"},,,}`))
	req.Header.Set("Authorization", "Bearer "+sidecarToken)
	res := httptest.NewRecorder()
	newServer(t).ServeHTTP(res, req)

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.Code)
	}
	if strings.Contains(res.Body.String(), apiSecret) {
		t.Fatalf("the error echoes the request body: %s", res.Body.String())
	}
}

func TestIncompleteCredentialsAre400(t *testing.T) {
	res := post(t, newServer(t), "/v1/sessions", sidecarToken, map[string]any{
		"credentials": map[string]string{"url": "wss://x.livekit.cloud"},
		"room":        "channel-1",
		"identity":    "user-1",
	})

	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", res.Code, res.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the error: %v", err)
	}
	if body.Error.Code != "invalid_request" {
		t.Fatalf("code = %q, want invalid_request", body.Error.Code)
	}
}

func TestVerifyAndRoomRoutes(t *testing.T) {
	upstream := fakeLiveKit(t)
	handler := newServer(t)
	creds := map[string]string{"url": upstream.URL, "apiKey": apiKey, "apiSecret": apiSecret}

	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{"/v1/verify", map[string]any{"credentials": creds}},
		{"/v1/rooms/delete", map[string]any{"credentials": creds, "room": "channel-1"}},
		{"/v1/rooms/participants", map[string]any{"credentials": creds, "room": "channel-1"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			res := post(t, handler, tc.path, sidecarToken, tc.body)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body: %s)", res.Code, res.Body.String())
			}
		})
	}
}

// GET on a POST route is a 405, not a silent 404 that reads as "wrong version".
func TestWrongMethodIsRefused(t *testing.T) {
	res := httptest.NewRecorder()
	newServer(t).ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/v1/verify", nil))

	if res.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", res.Code)
	}
}
