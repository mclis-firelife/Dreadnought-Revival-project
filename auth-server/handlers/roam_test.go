package handlers

// Cross-cluster single sign-on: grant mints a ticket on the home cluster,
// redeem trades it for a local session elsewhere. The fake master below
// plays the directory (delegate + verify behind the cluster key).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeRoamMaster(t *testing.T, key string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Sync-Key") != key {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/roam/delegate":
			var req struct {
				UserID string `json:"user_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.UserID == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok","ticket":"ticketticketticketticketticketticket12","expires_in":300,"user_id":"` + req.UserID + `","username":"roamer"}`))
		case "/roam/verify":
			_, _ = w.Write([]byte(`{"status":"ok","user_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","username":"roamer","email":"roamer@example.org"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func roamEnv(t *testing.T, master, secret string) {
	t.Helper()
	t.Setenv("SYNC_MASTER_URL", master)
	t.Setenv("SYNC_SECRET", secret)
}

func TestRoamGrantNeedsAuth(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	h.RoamGrant(rec, httptest.NewRequest(http.MethodPost, "/auth/roam/grant", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 without user", rec.Code)
	}
}

func TestRoamGrantNeedsRoaming(t *testing.T) {
	h := newTestHandler(t)
	roamEnv(t, "", "")
	req := httptest.NewRequest(http.MethodPost, "/auth/roam/grant", nil)
	req.Header.Set("X-User-ID", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	rec := httptest.NewRecorder()
	h.RoamGrant(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 without master configured", rec.Code)
	}
}

func TestRoamGrantRedeemRoundtrip(t *testing.T) {
	master := fakeRoamMaster(t, "cluster-key")
	defer master.Close()

	home := newTestHandler(t)
	roamEnv(t, master.URL, "cluster-key")
	register(t, home, "roamer", "roamer@example.org", "hunter2x")

	// Grant with the logged-in user (X-User-ID like jwtMiddleware sets it).
	req := httptest.NewRequest(http.MethodPost, "/auth/roam/grant", nil)
	req.Header.Set("X-User-ID", "roamer-id")
	rec := httptest.NewRecorder()
	home.RoamGrant(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body.String())
	}
	var grant struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &grant); err != nil || grant.Ticket == "" {
		t.Fatalf("no ticket back: %s", rec.Body.String())
	}

	// Redeem on another cluster: fresh session, stub account, dashed id.
	away := newTestHandler(t)
	rec = httptest.NewRecorder()
	away.RoamRedeem(rec, httptest.NewRequest(http.MethodPost, "/auth/roam/redeem",
		strings.NewReader(`{"ticket":"`+grant.Ticket+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if token, _ := body["access_token"].(string); token == "" {
		t.Fatal("redeem returned no access_token")
	}
	// The fake master vouches for aaaa…: the dashed form must come back.
	if id, _ := body["user_id"].(string); id != "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa" {
		t.Fatalf("user_id = %q", id)
	}
	var storedHash string
	if err := away.DB.QueryRow(`SELECT password_hash FROM users WHERE id='aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'`).Scan(&storedHash); err != nil {
		t.Fatalf("stub account missing: %v", err)
	}
	if !strings.HasPrefix(storedHash, "!roam-") {
		t.Fatalf("stub hash = %q, must be unusable", storedHash)
	}

	// The stub hash can never log in by password.
	rec = passwordLogin(t, away, "roamer@example.org", "hunter2x")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("password login on stub: got %d, want 401", rec.Code)
	}

	// Redeeming again for the existing account still works.
	rec = httptest.NewRecorder()
	away.RoamRedeem(rec, httptest.NewRequest(http.MethodPost, "/auth/roam/redeem",
		strings.NewReader(`{"ticket":"`+grant.Ticket+`"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("second redeem: %d %s", rec.Code, rec.Body.String())
	}

	// A locally banned account is refused even with a valid ticket.
	if _, err := away.DB.Exec(`UPDATE users SET banned_at=datetime('now') WHERE id='aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'`); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	away.RoamRedeem(rec, httptest.NewRequest(http.MethodPost, "/auth/roam/redeem",
		strings.NewReader(`{"ticket":"`+grant.Ticket+`"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("banned redeem: got %d, want 403", rec.Code)
	}
}

func TestRoamRedeemRejectsGarbage(t *testing.T) {
	h := newTestHandler(t)
	roamEnv(t, "http://127.0.0.1:1", "cluster-key")
	for _, tc := range []struct{ name, body string }{
		{"empty", `{}`},
		{"malformed", `not json`},
	} {
		rec := httptest.NewRecorder()
		h.RoamRedeem(rec, httptest.NewRequest(http.MethodPost, "/auth/roam/redeem", strings.NewReader(tc.body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", tc.name, rec.Code)
		}
	}
	// Unreachable master: 502-ish failure, never a session.
	rec := httptest.NewRecorder()
	h.RoamRedeem(rec, httptest.NewRequest(http.MethodPost, "/auth/roam/redeem",
		strings.NewReader(`{"ticket":"ticketticketticketticketticketticket12"}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("dead master must not issue sessions: %d", rec.Code)
	}
}
