package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

func adminTestRouter() *mux.Router {
	r := mux.NewRouter()
	registerAdminDashboard(r, "test-admin-key", "http://127.0.0.1:1", "")
	return r
}

func adminGet(r http.Handler, path, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if key != "" {
		req.Header.Set("X-Admin-Key", key)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// The page is static and served to anyone; it holds no data.
func TestAdminDashboardPageServed(t *testing.T) {
	rec := adminGet(adminTestRouter(), "/admin/dashboard", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Server Admin") {
		t.Fatalf("dashboard page: %d", rec.Code)
	}
}

// Every data route needs the admin key.
func TestAdminAPIRequiresTheKey(t *testing.T) {
	r := adminTestRouter()
	for _, path := range []string{"/admin/api/overview", "/admin/api/instances", "/admin/api/online", "/admin/api/players",
		"/admin/api/matches", "/admin/api/reports", "/admin/api/logs?src=mmogbrain", "/admin/api/battle-logs",
		"/admin/api/series?metric=matches", "/admin/api/sleepers", "/admin/api/wealth", "/admin/api/ships", "/admin/api/mode-stats"} {
		for _, key := range []string{"", "wrong"} {
			if rec := adminGet(r, path, key); rec.Code != http.StatusForbidden {
				t.Errorf("%s with key %q: %d, want 403", path, key, rec.Code)
			}
		}
	}
	del := httptest.NewRequest(http.MethodDelete, "/admin/api/instances/x", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, del)
	if rec.Code != http.StatusForbidden {
		t.Errorf("stopping an instance without the key: %d, want 403", rec.Code)
	}
}

// Log reading is limited to known files: no path from the request.
func TestAdminLogsRefuseArbitraryPaths(t *testing.T) {
	r := adminTestRouter()
	for _, q := range []string{"src=../../etc/passwd", "src=battle&name=../../../etc/passwd", "src=battle&name=/etc/passwd",
		"src=battle&name=secrets.env", "src=battle&name=battle-x/../../x.log"} {
		if rec := adminGet(r, "/admin/api/logs?"+q, "test-admin-key"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
	if rec := adminGet(r, "/admin/api/logs?src=mmogbrain", "test-admin-key"); rec.Code != http.StatusOK {
		t.Errorf("a known log: %d, want 200", rec.Code)
	}
}

// The store has ONE connection; a name lookup inside an open result set waits
// for itself forever. That deadlocked the server's whole database the first
// time the dashboard was opened (2026-09-29). Every data route must answer.
func TestAdminAPIDoesNotDeadlockTheStore(t *testing.T) {
	database := useTempMmogPlayerStateDB(t)
	const pid = "650dd79476a1484b8adcd01ac2f17354"
	if err := seedMmogPlayerState(database, pid); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO matches(id,game_mode,map,server_ip,server_port,status,created_at,started_at,battle_match_id) VALUES('m1','TDM','Glacier','127.0.0.1',7777,'ended','2026-09-29T20:00:00Z','2026-09-29T20:00:00Z','bm1')`,
		`INSERT INTO battle_results(match_id,user_id,outcome) VALUES('bm1','` + pid + `','win')`,
		`INSERT INTO client_reports(user_id,type,name,desc) VALUES('` + pid + `','T','N','D')`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := adminTestRouter()
	for _, path := range []string{"/admin/api/matches", "/admin/api/reports", "/admin/api/players?q=", "/admin/api/online", "/admin/api/overview"} {
		done := make(chan int, 1)
		go func() { done <- adminGet(r, path, "test-admin-key").Code }()
		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Errorf("%s: %d", path, code)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s did not answer in 3 s: the single-connection store is deadlocked", path)
		}
	}
}

	// Series bucket both timestamp spellings (DB datetime + RFC3339), sum
	// values per bucket, and never fail on an unknown table layout.
func TestAdminAPISeries(t *testing.T) {
	database := useTempMmogPlayerStateDB(t)
	const pid = "650dd79476a1484b8adcd01ac2f17354"
	if err := seedMmogPlayerState(database, pid); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO matches(id,game_mode,map,server_ip,server_port,status,created_at) VALUES('m1','TDM','Glacier','127.0.0.1',7777,'ended','2026-09-27 10:00:00')`,
		`INSERT INTO matches(id,game_mode,map,server_ip,server_port,status,created_at) VALUES('m2','TDM','Glacier','127.0.0.1',7778,'ended','2026-09-28T10:00:00Z')`,
		`INSERT INTO battle_results(match_id,user_id,outcome,kills,credits) VALUES('m1','` + pid + `','win',7,1500)`,
		`INSERT INTO client_reports(user_id,type,name,desc,created_at) VALUES('` + pid + `','T','N','D','2026-09-28 12:00:00')`,
		`INSERT INTO player_purchases(user_id,item_id,item_type,price_paid,currency,purchased_at) VALUES('` + pid + `',100,1,5000,'CR','2026-09-28 13:00:00')`,
		`INSERT INTO player_purchases(user_id,item_id,item_type,price_paid,currency,purchased_at) VALUES('` + pid + `',101,1,2000,'freexp','2026-09-28 13:00:00')`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := adminTestRouter()
	get := func(path string) (int, map[string]any) {
		t.Helper()
		rec := adminGet(r, path, "test-admin-key")
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		return rec.Code, doc
	}
	if code, _ := get("/admin/api/series?metric=bogus"); code != http.StatusBadRequest {
		t.Fatalf("unknown metric: got %d, want 400", code)
	}
	if code, _ := get("/admin/api/series?metric=matches&from=not-a-time"); code != http.StatusBadRequest {
		t.Fatalf("bad from: got %d, want 400", code)
	}
	code, doc := get("/admin/api/series?metric=matches&buckets=8")
	if code != http.StatusOK {
		t.Fatalf("matches: got %d", code)
	}
	if doc["total"] != float64(2) {
		t.Fatalf("matches total = %v, want 2 (both timestamp spellings)", doc["total"])
	}
	if pts, ok := doc["points"].([]any); !ok || len(pts) != 8 {
		t.Fatalf("matches points: %v", doc["points"])
	}
	_, doc = get("/admin/api/series?metric=kills")
	if doc["total"] != float64(7) {
		t.Fatalf("kills total = %v, want 7", doc["total"])
	}
	_, doc = get("/admin/api/series?metric=credits")
	if doc["total"] != float64(1500) {
		t.Fatalf("credits total = %v, want 1500", doc["total"])
	}
	// Spending counts credit purchases only, not research grants.
	_, doc = get("/admin/api/series?metric=spending")
	if doc["total"] != float64(5000) {
		t.Fatalf("spending total = %v, want 5000", doc["total"])
	}
	// Range filter: only the 28th counts.
	_, doc = get("/admin/api/series?metric=matches&from=2026-09-28T00:00:00Z&to=2026-09-29T00:00:00Z")
	if doc["total"] != float64(1) {
		t.Fatalf("ranged matches total = %v, want 1", doc["total"])
	}
	// Series needs the key like every other data route.
	if rec := adminGet(r, "/admin/api/series?metric=matches", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("no key: got %d, want 403", rec.Code)
	}
}

func TestAdminStatsEndpoints(t *testing.T) {
	database := useTempMmogPlayerStateDB(t)
	const pid = "650dd79476a1484b8adcd01ac2f17354"
	if err := seedMmogPlayerState(database, pid); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().AddDate(0, 0, -60).Format("2006-01-02 15:04:05")
	if _, err := database.Exec(`UPDATE player_state SET last_login_date=?,soft_currency=? WHERE user_id=?`,
		old, 75000, pid); err != nil {
		t.Fatal(err)
	}
	agosta := int32(33489262)
	if _, err := database.Exec(`INSERT INTO player_ship_xp(user_id,ship_id,xp,created_at,updated_at)
		VALUES(?,?,5000,datetime('now'),datetime('now'))`, pid, agosta); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO matches(id,game_mode,map,server_ip,server_port,status,created_at,battle_match_id) VALUES('m1','TDM','Glacier','127.0.0.1',7777,'ended','2026-09-28 10:00:00','bm1')`,
		`INSERT INTO battle_results(match_id,user_id,outcome,kills) VALUES('bm1','` + pid + `','win',7)`,
	} {
		if _, err := database.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	r := adminTestRouter()
	get := func(path string) map[string]any {
		t.Helper()
		rec := adminGet(r, path, "test-admin-key")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d", path, rec.Code)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		return doc
	}
	doc := get("/admin/api/sleepers?days=30")
	sleepers, _ := doc["sleepers"].([]any)
	if len(sleepers) != 1 {
		t.Fatalf("sleepers: %v", doc)
	}
	doc = get("/admin/api/wealth")
	bands, _ := doc["bands"].([]any)
	found := false
	for _, b := range bands {
		m := b.(map[string]any)
		if m["label"] == "50k–100k" && m["count"] == float64(1) {
			found = true
		}
	}
	if !found {
		t.Fatalf("75k pilot missing from wealth bands: %v", doc)
	}
	doc = get("/admin/api/ships")
	ships, _ := doc["ships"].([]any)
	if len(ships) != 1 {
		t.Fatalf("ships: %v", doc)
	}
	if ships[0].(map[string]any)["name"] != "Agosta" {
		t.Fatalf("ship name not resolved: %v", ships[0])
	}
	doc = get("/admin/api/mode-stats")
	modes, _ := doc["modes"].([]any)
	if len(modes) != 1 {
		t.Fatalf("modes: %v", doc)
	}
	m := modes[0].(map[string]any)
	if m["mode"] != "TDM" || m["wins"] != float64(1) || m["kills"] != float64(7) {
		t.Fatalf("mode stats wrong: %v", m)
	}
}
