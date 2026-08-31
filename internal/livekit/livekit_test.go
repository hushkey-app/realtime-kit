package livekit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"google.golang.org/protobuf/proto"
)

// The one string that must never leave this package. Every leak assertion is
// made against this exact value, and the fake upstream records the Authorization
// header it was sent, so "the message does not contain it" is not vacuous — the
// secret really did travel.
const testSecret = "livekit-super-secret-value-do-not-log"

const testKey = "APIkey123"

// fakeLiveKit stands in for a LiveKit deployment's twirp API.
//
// `reply` is consulted per RPC (the last path segment, e.g. "CreateRoom") and
// answers either a protobuf message or a twirp error. Requests are recorded so
// a test can assert on what was actually sent.
type fakeLiveKit struct {
	server *httptest.Server
	// calls is the RPC name of every request, in order.
	calls []string
	// auth is the Authorization header of the last request.
	auth string
}

type twirpFailure struct {
	status int
	code   string
	msg    string
}

func newFakeLiveKit(t *testing.T, reply func(rpc string) (proto.Message, *twirpFailure)) *fakeLiveKit {
	t.Helper()
	fake := &fakeLiveKit{}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpc := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		fake.calls = append(fake.calls, rpc)
		fake.auth = r.Header.Get("Authorization")
		_, _ = io.Copy(io.Discard, r.Body)

		msg, failure := reply(rpc)
		if failure != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failure.status)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"code": failure.code, "msg": failure.msg,
			})
			return
		}

		body, err := proto.Marshal(msg)
		if err != nil {
			t.Errorf("marshalling the fake reply for %s: %v", rpc, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/protobuf")
		_, _ = w.Write(body)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *fakeLiveKit) credentials() Credentials {
	return Credentials{URL: f.server.URL, APIKey: testKey, APISecret: testSecret}
}

func TestCredentialsValidate(t *testing.T) {
	cases := []struct {
		name  string
		creds Credentials
		want  string
	}{
		{"complete", Credentials{URL: "wss://x.livekit.cloud", APIKey: "k", APISecret: "s"}, ""},
		{"http is fine", Credentials{URL: "https://x.livekit.cloud", APIKey: "k", APISecret: "s"}, ""},
		{"no url", Credentials{APIKey: "k", APISecret: "s"}, "url is required"},
		{"no key", Credentials{URL: "wss://x", APISecret: "s"}, "apiKey is required"},
		{"no secret", Credentials{URL: "wss://x", APIKey: "k"}, "apiSecret is required"},
		{"bare host", Credentials{URL: "x.livekit.cloud", APIKey: "k", APISecret: "s"}, "url must start with"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.creds.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("expected valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected an error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// A cache key must separate credentials that differ in any field, and must not
// be the credentials themselves.
func TestFingerprint(t *testing.T) {
	base := Credentials{URL: "wss://a", APIKey: "k", APISecret: testSecret}
	same := Credentials{URL: "wss://a", APIKey: "k", APISecret: testSecret}
	other := Credentials{URL: "wss://a", APIKey: "k", APISecret: testSecret + "!"}

	if base.fingerprint() != same.fingerprint() {
		t.Fatal("equal credentials must share a fingerprint")
	}
	if base.fingerprint() == other.fingerprint() {
		t.Fatal("a different secret must produce a different fingerprint")
	}
	if strings.Contains(base.fingerprint(), testSecret) {
		t.Fatal("the fingerprint carries the secret in the clear")
	}
}

// The join path end to end: the room is created, the token is signed with the
// credentials given, and it carries exactly the grants asked for.
func TestSessionMintsAJoinToken(t *testing.T) {
	fake := newFakeLiveKit(t, func(rpc string) (proto.Message, *twirpFailure) {
		if rpc != "CreateRoom" {
			t.Errorf("unexpected RPC %q", rpc)
		}
		return &livekit.Room{Sid: "RM_abc", Name: "channel-1"}, nil
	})

	client := New(5 * time.Second)
	res, err := client.Session(context.Background(), SessionRequest{
		Credentials: fake.credentials(),
		Room:        "channel-1",
		Identity:    "user-42",
		Name:        "Ada",
		Admin:       true,
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	if res.RoomSID != "RM_abc" {
		t.Fatalf("roomSid = %q, want RM_abc", res.RoomSID)
	}
	if res.URL != fake.server.URL {
		t.Fatalf("url = %q, want the url we passed in", res.URL)
	}
	if res.Identity != "user-42" {
		t.Fatalf("identity = %q, want user-42", res.Identity)
	}
	if res.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatal("the token expires in the past")
	}

	verifier, err := auth.ParseAPIToken(res.Token)
	if err != nil {
		t.Fatalf("the minted token does not parse: %v", err)
	}
	if verifier.APIKey() != testKey {
		t.Fatalf("token apiKey = %q, want %q", verifier.APIKey(), testKey)
	}
	_, grants, err := verifier.Verify(testSecret)
	if err != nil {
		t.Fatalf("the token is not signed with the secret we passed: %v", err)
	}
	if grants.Identity != "user-42" {
		t.Fatalf("grant identity = %q, want user-42", grants.Identity)
	}
	if grants.Name != "Ada" {
		t.Fatalf("grant name = %q, want Ada", grants.Name)
	}
	if !grants.Video.RoomJoin || grants.Video.Room != "channel-1" {
		t.Fatalf("the token does not admit its holder to channel-1: %+v", grants.Video)
	}
	if !grants.Video.RoomAdmin {
		t.Fatal("a host's token must carry roomAdmin")
	}
	// The secret must have been used, not merely accepted.
	if fake.auth == "" {
		t.Fatal("the API call carried no Authorization header")
	}
}

// A participant is not an admin, and publishing can be withheld.
func TestSessionHonoursRestrictedGrants(t *testing.T) {
	fake := newFakeLiveKit(t, func(string) (proto.Message, *twirpFailure) {
		return &livekit.Room{Sid: "RM_x", Name: "channel-1"}, nil
	})

	no := false
	res, err := New(5*time.Second).Session(context.Background(), SessionRequest{
		Credentials: fake.credentials(),
		Room:        "channel-1",
		Identity:    "listener",
		CanPublish:  &no,
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	verifier, err := auth.ParseAPIToken(res.Token)
	if err != nil {
		t.Fatalf("ParseAPIToken: %v", err)
	}
	_, grants, err := verifier.Verify(testSecret)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if grants.Video.RoomAdmin {
		t.Fatal("a participant must not get roomAdmin")
	}
	if grants.Video.CanPublish == nil || *grants.Video.CanPublish {
		t.Fatal("canPublish: false was not honoured")
	}
	if grants.Video.CanSubscribe == nil || !*grants.Video.CanSubscribe {
		t.Fatal("canSubscribe should default to true")
	}
}

// A refused credential comes back as LiveKit's refusal — and carries none of
// the request that produced it.
func TestSessionSurfacesRefusalWithoutTheSecret(t *testing.T) {
	fake := newFakeLiveKit(t, func(string) (proto.Message, *twirpFailure) {
		return nil, &twirpFailure{
			status: http.StatusUnauthorized,
			code:   "unauthenticated",
			msg:    "invalid API key",
		}
	})

	_, err := New(5*time.Second).Session(context.Background(), SessionRequest{
		Credentials: fake.credentials(),
		Room:        "channel-1",
		Identity:    "user-42",
	})
	if err == nil {
		t.Fatal("expected the provider's refusal")
	}

	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", apiErr.Status)
	}
	if apiErr.Code != "invalid_credentials" {
		t.Fatalf("code = %q, want invalid_credentials", apiErr.Code)
	}
	if strings.Contains(apiErr.Message, testSecret) || strings.Contains(apiErr.Message, testKey) {
		t.Fatalf("the error carries the credentials: %q", apiErr.Message)
	}
}

// A transport fault must not surface the underlying error: its text can name
// the request, and the request carries the secret in a header.
func TestSessionHidesTransportFaults(t *testing.T) {
	// A port nothing is listening on — the dial fails rather than the RPC.
	_, err := New(2*time.Second).Session(context.Background(), SessionRequest{
		Credentials: Credentials{URL: "http://127.0.0.1:1", APIKey: testKey, APISecret: testSecret},
		Room:        "channel-1",
		Identity:    "user-42",
	})
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	apiErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T", err)
	}
	if apiErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", apiErr.Status)
	}
	if strings.Contains(apiErr.Message, testSecret) {
		t.Fatalf("the error carries the secret: %q", apiErr.Message)
	}
}

// Bad input is refused before any client is built or any call is made.
func TestSessionRejectsIncompleteRequests(t *testing.T) {
	client := New(time.Second)
	creds := Credentials{URL: "wss://x.livekit.cloud", APIKey: "k", APISecret: "s"}

	for _, tc := range []struct {
		name string
		req  SessionRequest
	}{
		{"no room", SessionRequest{Credentials: creds, Identity: "u"}},
		{"no identity", SessionRequest{Credentials: creds, Room: "r"}},
		{"no credentials", SessionRequest{Room: "r", Identity: "u"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := client.Session(context.Background(), tc.req)
			apiErr, ok := err.(*Error)
			if !ok {
				t.Fatalf("expected *Error, got %T (%v)", err, err)
			}
			if apiErr.Status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", apiErr.Status)
			}
		})
	}
}

// Deleting a room LiveKit has already forgotten is the normal case on channel
// deletion, not a failure.
func TestDeleteRoomTreatsAMissingRoomAsDone(t *testing.T) {
	fake := newFakeLiveKit(t, func(string) (proto.Message, *twirpFailure) {
		return nil, &twirpFailure{status: http.StatusNotFound, code: "not_found", msg: "no such room"}
	})

	err := New(5*time.Second).DeleteRoom(context.Background(), RoomRequest{
		Credentials: fake.credentials(),
		Room:        "channel-1",
	})
	if err != nil {
		t.Fatalf("a missing room should not be an error, got %v", err)
	}
}

func TestDeleteRoomSurfacesRealFailures(t *testing.T) {
	fake := newFakeLiveKit(t, func(string) (proto.Message, *twirpFailure) {
		return nil, &twirpFailure{status: http.StatusInternalServerError, code: "internal", msg: "boom"}
	})

	err := New(5*time.Second).DeleteRoom(context.Background(), RoomRequest{
		Credentials: fake.credentials(),
		Room:        "channel-1",
	})
	if err == nil {
		t.Fatal("expected the upstream failure to propagate")
	}
}

func TestParticipantsMapsLiveKitsRoster(t *testing.T) {
	fake := newFakeLiveKit(t, func(string) (proto.Message, *twirpFailure) {
		return &livekit.ListParticipantsResponse{Participants: []*livekit.ParticipantInfo{
			{Identity: "user-1", Sid: "PA_1", Name: "Ada", State: livekit.ParticipantInfo_ACTIVE},
			{Identity: "user-2", Sid: "PA_2", Name: "Grace", State: livekit.ParticipantInfo_JOINED},
		}}, nil
	})

	people, err := New(5*time.Second).Participants(context.Background(), RoomRequest{
		Credentials: fake.credentials(),
		Room:        "channel-1",
	})
	if err != nil {
		t.Fatalf("Participants: %v", err)
	}
	if len(people) != 2 {
		t.Fatalf("got %d participants, want 2", len(people))
	}
	if people[0].Identity != "user-1" || people[0].State != "ACTIVE" {
		t.Fatalf("unexpected first participant: %+v", people[0])
	}
}

func TestParticipantsOfAMissingRoomIsEmpty(t *testing.T) {
	fake := newFakeLiveKit(t, func(string) (proto.Message, *twirpFailure) {
		return nil, &twirpFailure{status: http.StatusNotFound, code: "not_found", msg: "no such room"}
	})

	people, err := New(5*time.Second).Participants(context.Background(), RoomRequest{
		Credentials: fake.credentials(),
		Room:        "channel-1",
	})
	if err != nil {
		t.Fatalf("a missing room should read as empty, got %v", err)
	}
	if len(people) != 0 {
		t.Fatalf("got %d participants, want none", len(people))
	}
}

func TestVerifyCountsRooms(t *testing.T) {
	fake := newFakeLiveKit(t, func(rpc string) (proto.Message, *twirpFailure) {
		if rpc != "ListRooms" {
			t.Errorf("verify should use the cheapest read, got %q", rpc)
		}
		return &livekit.ListRoomsResponse{Rooms: []*livekit.Room{{Name: "a"}}}, nil
	})

	count, err := New(5*time.Second).Verify(context.Background(), fake.credentials())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if count != 1 {
		t.Fatalf("rooms = %d, want 1", count)
	}
}

// The client cache must not hand a second tenant the first tenant's client.
func TestClientCacheIsPerCredential(t *testing.T) {
	first := newFakeLiveKit(t, func(string) (proto.Message, *twirpFailure) {
		return &livekit.ListRoomsResponse{Rooms: []*livekit.Room{{Name: "first"}}}, nil
	})
	second := newFakeLiveKit(t, func(string) (proto.Message, *twirpFailure) {
		return &livekit.ListRoomsResponse{}, nil
	})

	client := New(5 * time.Second)
	if _, err := client.Verify(context.Background(), first.credentials()); err != nil {
		t.Fatalf("first Verify: %v", err)
	}
	if _, err := client.Verify(context.Background(), second.credentials()); err != nil {
		t.Fatalf("second Verify: %v", err)
	}

	if len(first.calls) != 1 || len(second.calls) != 1 {
		t.Fatalf("each deployment should have been called once, got %d and %d",
			len(first.calls), len(second.calls))
	}
}
