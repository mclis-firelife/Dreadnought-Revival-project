package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

const syncTestKey = "test-sync-secret-aaaaaaaaaaaaaaaa"

func seedSyncCluster(t *testing.T, h *Handler) string {
	t.Helper()
	// Direct insert: register endpoint tested elsewhere; sync needs a secret.
	id := "11111111-2222-3333-4444-555555555555"
	if _, err := h.DB.Exec(`INSERT INTO clusters(id,name,web_url,battle_ip,secret_hash,last_heartbeat,registered_at)
		VALUES(?,'Sync Cluster','https://x.example','203.0.113.9',?,datetime('now'),datetime('now'))`,
		id, secretHash(syncTestKey)); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
	return id
}

func authedSyncReq(t *testing.T, method, path, body, key string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-Sync-Key", key)
	}
	rec := httptest.NewRecorder()
	return rec, req
}

func pushPayload(user, ts string) string {
	raw, _ := json.Marshal(map[string]any{
		"users": []any{map[string]any{
			"user_id": user, "username": "alice", "email": "a@x.org",
			"password_hash": "hash", "created_at": ts, "updated_at": ts,
		}},
		"snapshots": []any{map[string]any{
			"user_id": user, "updated_at": ts, "credits": 15000, "rank": 5, "ships": 3,
			"tables": map[string]any{},
		}},
		"bans": []any{},
	})
	return string(raw)
}

func TestSyncPushPullRoundtrip(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	uid := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload(uid, "2026-09-29T10:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"accepted":2`) {
		t.Fatalf("unexpected push result: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/sync/pull?since=2020-01-01T00:00:00Z", nil)
	req.Header.Set("X-Sync-Key", syncTestKey)
	h.SyncPull(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pull status %d", rec.Code)
	}
	var doc struct {
		Users     []syncUser         `json:"users"`
		Snapshots []map[string]any  `json:"snapshots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Users) != 1 || len(doc.Snapshots) != 1 {
		t.Fatalf("unexpected pull: %s", rec.Body.String())
	}
	if doc.Users[0].Username != "alice" {
		t.Errorf("wrong user mirrored: %+v", doc.Users[0])
	}
}

func TestSyncPushRejectsBadKey(t *testing.T) {
	h := testHandler(t)
	rec, req := authedSyncReq(t, "POST", "/sync/push", `{}`, "wrong-key")
	h.SyncPush(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.SyncPull(rec, httptest.NewRequest(http.MethodGet, "/sync/pull", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", rec.Code)
	}
}

func TestSyncPushLastWriteWins(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	uid := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload(uid, "2026-09-29T12:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status %d", rec.Code)
	}
	// Older data must not overwrite newer.
	rec, req = authedSyncReq(t, "POST", "/sync/push", pushPayload(uid, "2026-09-29T10:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	if !strings.Contains(rec.Body.String(), `"skipped":2`) {
		t.Fatalf("stale push not skipped: %s", rec.Body.String())
	}
	var credits int
	if err := h.DB.QueryRow(`SELECT credits FROM sync_snapshots WHERE user_id=?`, uid).Scan(&credits); err != nil {
		t.Fatal(err)
	}
	if credits != 15000 {
		t.Errorf("credits = %d, want the newer 15000", credits)
	}
}

func TestSyncPresenceBlocksOtherCluster(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	uid := "dddddddddddddddddddddddddddddddd"

	// Push with in_match=true: presence must report the cluster.
	raw, _ := json.Marshal(map[string]any{
		"users": []any{}, "bans": []any{},
		"snapshots": []any{map[string]any{
			"user_id": uid, "updated_at": "2026-09-29T12:00:00Z",
			"credits": 1, "rank": 1, "ships": 1, "in_match": true,
			"tables": map[string]any{},
		}},
	})
	rec, req := authedSyncReq(t, "POST", "/sync/push", string(raw), syncTestKey)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status %d", rec.Code)
	}

	presence := func(path string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		// mux vars are the only router magic here; set directly.
		req = mux.SetURLVars(req, map[string]string{"user_id": uid})
		h.SyncPresence(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("presence %s: status %d", path, rec.Code)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return doc
	}
	if doc := presence("/presence/" + uid); doc["in_match"] != true || doc["cluster"] != "Sync Cluster" {
		t.Fatalf("expected block on Sync Cluster, got %v", doc)
	}
	// Same cluster excluded (by id and by name): no block.
	cid := "11111111-2222-3333-4444-555555555555"
	if doc := presence("/presence/" + uid + "?except=" + cid); doc["in_match"] != false {
		t.Fatalf("own cluster id must be excluded, got %v", doc)
	}
	if doc := presence("/presence/" + uid + "?except=Sync+Cluster"); doc["in_match"] != false {
		t.Fatalf("own cluster name must be excluded, got %v", doc)
	}
	// Match over (push in_match=false) clears the block.
	raw, _ = json.Marshal(map[string]any{
		"users": []any{}, "bans": []any{},
		"snapshots": []any{map[string]any{
			"user_id": uid, "updated_at": "2026-09-29T12:05:00Z",
			"credits": 1, "rank": 1, "ships": 1, "in_match": false,
			"tables": map[string]any{},
		}},
	})
	rec, req = authedSyncReq(t, "POST", "/sync/push", string(raw), syncTestKey)
	h.SyncPush(rec, req)
	if doc := presence("/presence/" + uid); doc["in_match"] != false {
		t.Fatalf("finished match must clear, got %v", doc)
	}
	// Stale report (older than the window) counts as gone.
	if _, err := h.DB.Exec(`UPDATE sync_presence SET in_match=1,updated_at='2020-01-01T00:00:00Z'
		WHERE user_id=?`, uid); err != nil {
		t.Fatal(err)
	}
	if doc := presence("/presence/" + uid); doc["in_match"] != false {
		t.Fatalf("stale presence must not block, got %v", doc)
	}
}

func TestSyncRegisterCheck(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "2026-09-29T10:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push status %d", rec.Code)
	}
	check := func(query string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.SyncRegisterCheck(rec, httptest.NewRequest(http.MethodGet, "/register-check"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("check %s: status %d", query, rec.Code)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return doc
	}
	// pushPayload uses username alice / email a@x.org.
	if doc := check("?username=alice"); doc["taken"] != true {
		t.Fatalf("username must be taken: %v", doc)
	}
	if doc := check("?email=a@x.org"); doc["taken"] != true {
		t.Fatalf("email must be taken: %v", doc)
	}
	if doc := check("?username=bob&email=b@x.org"); doc["taken"] != false {
		t.Fatalf("fresh name+address must be free: %v", doc)
	}
	if doc := check("?username=alice&email=b@x.org"); doc["taken"] != true {
		t.Fatalf("taken name with free address must still block: %v", doc)
	}
	rec = httptest.NewRecorder()
	h.SyncRegisterCheck(rec, httptest.NewRequest(http.MethodGet, "/register-check", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty query: got %d, want 400", rec.Code)
	}
}

func TestSyncPushLogsEveryCall(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload("cccccccccccccccccccccccccccccccc", "2026-09-29T10:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	rec, req = authedSyncReq(t, "POST", "/sync/push", `{}`, "nope")
	h.SyncPush(rec, req)

	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM sync_log WHERE endpoint='push'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("sync_log push rows = %d, want 2 (ok + denied)", n)
	}
	var denied int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM sync_log WHERE status='denied'`).Scan(&denied); err != nil {
		t.Fatal(err)
	}
	if denied != 1 {
		t.Errorf("denied rows = %d, want 1", denied)
	}
}

// fakeAgents serves any number of cluster agents: path /<tag>/sync/now
// records the force flag in call order; tags in fail answer HTTP 500.
func fakeAgents(t *testing.T, calls *[]string, fail map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/sync/now")
		var req struct {
			Force bool `json:"force"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		force := "false"
		if req.Force {
			force = "true"
		}
		*calls = append(*calls, tag+":"+force)
		if fail[tag] {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","pushed":3,"applied":2}`))
	}))
}

func seedSyncClusterWithAgent(t *testing.T, h *Handler, id, name, agentURL string) {
	t.Helper()
	if _, err := h.DB.Exec(`INSERT INTO clusters(id,name,web_url,battle_ip,agent_url,last_heartbeat,registered_at)
		VALUES(?,?,?,?,?,datetime('now'),datetime('now'))`,
		id, name, "https://x.example", "203.0.113.9", agentURL); err != nil {
		t.Fatalf("seed cluster: %v", err)
	}
}

func TestAdminSyncSettings(t *testing.T) {
	h := testHandler(t)
	id := seedSyncCluster(t, h)

	get := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.AdminSyncSettings(rec, httptest.NewRequest(http.MethodGet, "/admin/api/sync-settings", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("get: status %d", rec.Code)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode: %v", err)
		}
		s, _ := doc["main_cluster_id"].(string)
		return s
	}
	post := func(body string) int {
		t.Helper()
		rec := httptest.NewRecorder()
		h.AdminSyncSettings(rec, httptest.NewRequest(http.MethodPost, "/admin/api/sync-settings",
			strings.NewReader(body)))
		return rec.Code
	}
	if got := get(); got != "" {
		t.Fatalf("fresh settings: %q, want empty", got)
	}
	if code := post(`{"main_cluster_id":"nope"}`); code != http.StatusNotFound {
		t.Fatalf("unknown cluster: got %d, want 404", code)
	}
	if code := post(`{"main_cluster_id":"` + id + `"}`); code != http.StatusOK {
		t.Fatalf("set main: got %d", code)
	}
	if got := get(); got != id {
		t.Fatalf("main = %q, want %q", got, id)
	}
	if code := post(`{"main_cluster_id":""}`); code != http.StatusOK {
		t.Fatalf("clear: got %d", code)
	}
	if got := get(); got != "" {
		t.Fatalf("after clear: %q", got)
	}
}

func TestAdminSyncNow(t *testing.T) {
	h := testHandler(t)
	var calls []string
	srv := fakeAgents(t, &calls, map[string]bool{"b": true})
	defer srv.Close()
	seedSyncClusterWithAgent(t, h, "id-a", "Alpha", srv.URL+"/a")
	seedSyncClusterWithAgent(t, h, "id-b", "Beta", srv.URL+"/b")
	seedSyncCluster(t, h) // Sync Cluster: no agent URL, skipped silently.

	rec := httptest.NewRecorder()
	h.AdminSyncNow(rec, httptest.NewRequest(http.MethodPost, "/admin/api/sync-now", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Results []triggerResult `json:"results"`
		Count   int             `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Count != 2 {
		t.Fatalf("count = %d, want 2 (agent-less cluster skipped)", doc.Count)
	}
	byName := map[string]triggerResult{}
	for _, r := range doc.Results {
		byName[r.Name] = r
	}
	if !byName["Alpha"].OK || byName["Alpha"].Pushed != 3 || byName["Alpha"].Applied != 2 {
		t.Fatalf("alpha wrong: %+v", byName["Alpha"])
	}
	if byName["Beta"].OK || byName["Beta"].Detail == "" {
		t.Fatalf("beta must fail with detail: %+v", byName["Beta"])
	}
	var logged int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM sync_log WHERE endpoint='sync-now'`).Scan(&logged); err != nil || logged != 2 {
		t.Fatalf("sync-now log rows = %d (err %v), want 2", logged, err)
	}
}

func TestAdminRollout(t *testing.T) {
	h := testHandler(t)
	var calls []string
	srv := fakeAgents(t, &calls, map[string]bool{})
	defer srv.Close()
	seedSyncClusterWithAgent(t, h, "id-a", "Alpha", srv.URL+"/a")
	seedSyncClusterWithAgent(t, h, "id-b", "Beta", srv.URL+"/b")
	if err := h.setSetting("main_cluster_id", "id-a"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.AdminRollout(rec, httptest.NewRequest(http.MethodPost, "/admin/api/rollout", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// Main first without force, then everyone else forced.
	if len(calls) != 2 || calls[0] != "a:false" || calls[1] != "b:true" {
		t.Fatalf("call order = %v, want [a:false b:true]", calls)
	}
	var doc struct {
		Main    triggerResult   `json:"main"`
		Results []triggerResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !doc.Main.OK || doc.Main.Name != "Alpha" || len(doc.Results) != 1 || !doc.Results[0].OK {
		t.Fatalf("unexpected rollout doc: %s", rec.Body.String())
	}
}

func TestAdminRolloutAbortsWhenMainFails(t *testing.T) {
	h := testHandler(t)
	var calls []string
	srv := fakeAgents(t, &calls, map[string]bool{"a": true})
	defer srv.Close()
	seedSyncClusterWithAgent(t, h, "id-a", "Alpha", srv.URL+"/a")
	seedSyncClusterWithAgent(t, h, "id-b", "Beta", srv.URL+"/b")
	if err := h.setSetting("main_cluster_id", "id-a"); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.AdminRollout(rec, httptest.NewRequest(http.MethodPost, "/admin/api/rollout", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if len(calls) != 1 || calls[0] != "a:false" {
		t.Fatalf("calls = %v, want only the failed main", calls)
	}
}

func TestAdminRolloutNeedsMain(t *testing.T) {
	h := testHandler(t)
	rec := httptest.NewRecorder()
	h.AdminRollout(rec, httptest.NewRequest(http.MethodPost, "/admin/api/rollout", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestAdminSyncStatus(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	cid := "11111111-2222-3333-4444-555555555555"
	if _, err := h.DB.Exec(`INSERT INTO sync_log(cluster_id,direction,endpoint,users,status,detail)
		VALUES(?,'in','push',5,'ok',''),(?,'out','pull',3,'ok','')`, cid, cid); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.AdminSyncStatus(rec, httptest.NewRequest(http.MethodGet, "/admin/api/syncstatus", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var doc struct {
		Clusters []struct {
			Name          string `json:"name"`
			HasSecret     bool   `json:"has_secret"`
			LastPush      string `json:"last_push"`
			LastPushState string `json:"last_push_status"`
			LastPull      string `json:"last_pull"`
		} `json:"clusters"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Count != 1 {
		t.Fatalf("count = %d: %s", doc.Count, rec.Body.String())
	}
	c := doc.Clusters[0]
	if !c.HasSecret || c.LastPush == "" || c.LastPushState != "ok" || c.LastPull == "" {
		t.Fatalf("unexpected status row: %+v", c)
	}
}

func TestAdminPresenceBoard(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	cid := "11111111-2222-3333-4444-555555555555"
	uid := "ffffffffffffffffffffffffffffffff"
	if _, err := h.DB.Exec(`INSERT INTO sync_users(user_id,username,email) VALUES(?,'zara','z@x.org')`, uid); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := h.DB.Exec(`INSERT INTO sync_presence(user_id,cluster_id,in_match,updated_at)
		VALUES(?,?,1,?), (?,'other',1,'2020-01-01T00:00:00Z')`, uid, cid, now, uid); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.AdminPresence(rec, httptest.NewRequest(http.MethodGet, "/admin/api/presence", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var doc struct {
		Players []struct {
			Username string `json:"username"`
			Cluster  string `json:"cluster"`
		} `json:"players"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Count != 1 || doc.Players[0].Username != "zara" || doc.Players[0].Cluster != "Sync Cluster" {
		t.Fatalf("unexpected board: %s", rec.Body.String())
	}
}

func TestAdminRolloutPreview(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	cid := "11111111-2222-3333-4444-555555555555"
	if err := h.setSetting("main_cluster_id", cid); err != nil {
		t.Fatal(err)
	}
	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "2026-09-29T10:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	rec = httptest.NewRecorder()
	h.AdminRolloutPreview(rec, httptest.NewRequest(http.MethodGet, "/admin/api/rollout-preview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		UsersTotal         int `json:"users_total"`
		UsersMain          int `json:"users_main"`
		UsersOnlyElsewhere int `json:"users_only_elsewhere"`
		SnapshotsFlipping  int `json:"snapshots_flipping"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.UsersTotal != 1 || doc.UsersMain != 1 || doc.UsersOnlyElsewhere != 0 || doc.SnapshotsFlipping != 0 {
		t.Fatalf("unexpected preview: %s", rec.Body.String())
	}
	// No main selected: 400.
	if err := h.setSetting("main_cluster_id", ""); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.AdminRolloutPreview(rec, httptest.NewRequest(http.MethodGet, "/admin/api/rollout-preview", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no main: got %d, want 400", rec.Code)
	}
}

func TestAdminPingAgent(t *testing.T) {
	h := testHandler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	seedSyncClusterWithAgent(t, h, "id-a", "Alpha", srv.URL+"/a")
	seedSyncClusterWithAgent(t, h, "id-b", "Beta", "http://127.0.0.1:1")
	seedSyncCluster(t, h) // no agent URL.
	ping := func(id string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin/api/clusters/"+id+"/ping", nil)
		req = mux.SetURLVars(req, map[string]string{"id": id})
		rec := httptest.NewRecorder()
		h.AdminPingAgent(rec, req)
		var doc map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		return rec.Code, doc
	}
	if code, doc := ping("id-a"); code != http.StatusOK || doc["ok"] != true || doc["latency_ms"] == nil {
		t.Fatalf("healthy agent: %d %v", code, doc)
	}
	if code, doc := ping("id-b"); code != http.StatusOK || doc["ok"] != false {
		t.Fatalf("dead agent must fail soft: %d %v", code, doc)
	}
	if code, _ := ping("11111111-2222-3333-4444-555555555555"); code != http.StatusBadRequest {
		t.Fatalf("no agent URL: got %d, want 400", code)
	}
}

func TestAdminSources(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "2026-09-29T10:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.AdminSources(rec, httptest.NewRequest(http.MethodGet, "/admin/api/sources", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var doc struct {
		Sources []struct {
			Cluster string `json:"cluster"`
			Users   int    `json:"users"`
		} `json:"sources"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Total != 1 || len(doc.Sources) != 1 || doc.Sources[0].Cluster != "Sync Cluster" || doc.Sources[0].Users != 1 {
		t.Fatalf("unexpected sources: %s", rec.Body.String())
	}
}

func roamCall(t *testing.T, h *Handler, path, key, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-Sync-Key", key)
	}
	rec := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/delegate"):
		h.SyncRoamDelegate(rec, req)
	default:
		h.SyncRoamVerify(rec, req)
	}
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Code, doc
}

func TestRoamDelegateVerify(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	uid := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload(uid, "2026-09-29T10:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push: %d", rec.Code)
	}
	// Unknown cluster key: denied on both ends.
	if code, _ := roamCall(t, h, "/roam/delegate", "nope", `{"user_id":"`+uid+`"}`); code != http.StatusForbidden {
		t.Fatalf("delegate bad key: got %d, want 403", code)
	}
	if code, _ := roamCall(t, h, "/roam/verify", "nope", `{"ticket":"x"}`); code != http.StatusForbidden {
		t.Fatalf("verify bad key: got %d, want 403", code)
	}
	// Unknown account: not mirrored yet.
	if code, _ := roamCall(t, h, "/roam/delegate", syncTestKey, `{"user_id":"00000000000000000000000000000000"}`); code != http.StatusNotFound {
		t.Fatalf("delegate unknown user: got %d, want 404", code)
	}
	// Delegate (dashed id spelling accepted too), then verify.
	code, doc := roamCall(t, h, "/roam/delegate", syncTestKey, `{"user_id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"}`)
	if code != http.StatusOK {
		t.Fatalf("delegate: %d %v", code, doc)
	}
	ticket, _ := doc["ticket"].(string)
	if len(ticket) != 48 || doc["username"] != "alice" {
		t.Fatalf("bad ticket doc: %v", doc)
	}
	code, doc = roamCall(t, h, "/roam/verify", syncTestKey, `{"ticket":"`+ticket+`"}`)
	if code != http.StatusOK || doc["user_id"] != uid || doc["username"] != "alice" {
		t.Fatalf("verify: %d %v", code, doc)
	}
	// Reuse within expiry is fine (one user, several clusters in a row).
	if code, _ := roamCall(t, h, "/roam/verify", syncTestKey, `{"ticket":"`+ticket+`"}`); code != http.StatusOK {
		t.Fatalf("verify reuse: got %d, want 200", code)
	}
	// Unknown ticket denied.
	if code, _ := roamCall(t, h, "/roam/verify", syncTestKey, `{"ticket":"`+strings.Repeat("0", 48)+`"}`); code != http.StatusForbidden {
		t.Fatalf("unknown ticket: got %d, want 403", code)
	}
	// Expired ticket pruned on sight.
	if _, err := h.DB.Exec(`UPDATE roam_tickets SET expires_at='2020-01-01 00:00:00'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := roamCall(t, h, "/roam/verify", syncTestKey, `{"ticket":"`+ticket+`"}`); code != http.StatusForbidden {
		t.Fatalf("expired ticket: got %d, want 403", code)
	}
}

func mintPairToken(t *testing.T, h *Handler, id string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/clusters/"+id+"/pair", nil)
	req = mux.SetURLVars(req, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	h.AdminPair(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pair mint: %d %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Token) != 48 {
		t.Fatalf("no token back: %s", rec.Body.String())
	}
	return doc.Token
}

func pairExchange(t *testing.T, h *Handler, token string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"token": token})
	rec := httptest.NewRecorder()
	h.SyncPair(rec, httptest.NewRequest(http.MethodPost, "/sync/pair", strings.NewReader(string(raw))))
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	return rec.Code, doc
}

func TestSyncPairFlow(t *testing.T) {
	h := testHandler(t)
	id := seedSyncCluster(t, h)
	token := mintPairToken(t, h, id)

	code, doc := pairExchange(t, h, token)
	if code != http.StatusOK {
		t.Fatalf("pair: %d %v", code, doc)
	}
	secret, _ := doc["secret"].(string)
	if len(secret) != 48 {
		t.Fatalf("no secret back: %v", doc)
	}
	// The issued secret authenticates immediately.
	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "2026-09-29T10:00:00Z"), secret)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push with paired secret: %d %s", rec.Code, rec.Body.String())
	}
	// Single use: replay fails.
	if code, _ := pairExchange(t, h, token); code != http.StatusForbidden {
		t.Fatalf("replay: got %d, want 403", code)
	}
	// Unknown token fails.
	if code, _ := pairExchange(t, h, strings.Repeat("0", 48)); code != http.StatusForbidden {
		t.Fatalf("unknown token: got %d, want 403", code)
	}
	// Expired token fails.
	expiring := mintPairToken(t, h, id)
	if _, err := h.DB.Exec(`UPDATE pairing_tokens SET expires_at='2020-01-01 00:00:00'
		WHERE token_hash=?`, secretHash(expiring)); err != nil {
		t.Fatal(err)
	}
	if code, _ := pairExchange(t, h, expiring); code != http.StatusForbidden {
		t.Fatalf("expired token: got %d, want 403", code)
	}
	// Unknown cluster: 404.
	req = httptest.NewRequest(http.MethodPost, "/admin/api/clusters/nope/pair", nil)
	req = mux.SetURLVars(req, map[string]string{"id": "nope"})
	rec = httptest.NewRecorder()
	h.AdminPair(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown cluster: got %d, want 404", rec.Code)
	}
}

func TestSyncRotate(t *testing.T) {
	h := testHandler(t)
	seedSyncCluster(t, h)
	rotate := func(key string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/sync/rotate", nil)
		req.Header.Set("X-Sync-Key", key)
		rec := httptest.NewRecorder()
		h.SyncRotate(rec, req)
		var doc map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		return rec.Code, doc
	}
	if code, _ := rotate("wrong-key"); code != http.StatusForbidden {
		t.Fatalf("bad key: got %d, want 403", code)
	}
	code, doc := rotate(syncTestKey)
	if code != http.StatusOK {
		t.Fatalf("rotate: %d %v", code, doc)
	}
	fresh, _ := doc["secret"].(string)
	if len(fresh) != 48 || fresh == syncTestKey {
		t.Fatalf("no fresh secret back: %v", doc)
	}
	// Old secret dies with the response; fresh one works.
	rec, req := authedSyncReq(t, "POST", "/sync/push", pushPayload("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "2026-09-29T10:00:00Z"), syncTestKey)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("old secret after rotate: got %d, want 403", rec.Code)
	}
	rec, req = authedSyncReq(t, "POST", "/sync/push", pushPayload("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "2026-09-29T10:00:00Z"), fresh)
	h.SyncPush(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh secret after rotate: got %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminUptimeVolumeGrowthErrors(t *testing.T) {
	h := testHandler(t)
	cid := seedSyncCluster(t, h)
	now := time.Now().UTC()
	recent := now.Add(-time.Hour).Format("2006-01-02 15:04:05")
	if _, err := h.DB.Exec(`INSERT INTO heartbeat_events(cluster_id,time,event)
		VALUES(?,?,'online'),(?,'2020-01-01 00:00:00','offline')`, cid, recent, cid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.DB.Exec(`INSERT INTO sync_log(cluster_id,direction,endpoint,users,status,detail)
		VALUES(?,'in','push',5,'ok',''),(?,'in','push',0,'denied','bad key')`, cid, cid); err != nil {
		t.Fatal(err)
	}
	uid := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if _, err := h.DB.Exec(`INSERT INTO sync_users(user_id,username,email,created_at) VALUES(?,'eve','e@x.org',?)`,
		uid, now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		switch {
		case strings.HasPrefix(path, "/admin/api/uptime"):
			h.AdminUptime(rec, httptest.NewRequest(http.MethodGet, path, nil))
		case strings.HasPrefix(path, "/admin/api/sync-volume"):
			h.AdminSyncVolume(rec, httptest.NewRequest(http.MethodGet, path, nil))
		case strings.HasPrefix(path, "/admin/api/growth"):
			h.AdminGrowth(rec, httptest.NewRequest(http.MethodGet, path, nil))
		case strings.HasPrefix(path, "/admin/api/sync-errors"):
			h.AdminSyncErrors(rec, httptest.NewRequest(http.MethodGet, path, nil))
		default:
			t.Fatalf("unknown path %s", path)
		}
		var doc map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		return rec.Code, doc
	}
	if code, doc := get("/admin/api/uptime?range=24h"); code != http.StatusOK {
		t.Fatalf("uptime: %d", code)
	} else if cls, ok := doc["clusters"].([]any); !ok || len(cls) != 1 {
		t.Fatalf("uptime clusters: %v", doc)
	} else {
		m := cls[0].(map[string]any)
		// Offline since 2020, online for the last hour: a sliver of 24h, one flap.
		if pct, _ := m["pct"].(float64); pct <= 0 || pct > 10 {
			t.Fatalf("uptime pct = %v, want (0,10]", pct)
		}
		if flaps, _ := m["flaps"].(float64); flaps != 1 {
			t.Fatalf("flaps = %v, want 1", flaps)
		}
	}
	if code, doc := get("/admin/api/uptime?range=bogus"); code != http.StatusBadRequest {
		t.Fatalf("bad range: got %d, want 400 (%v)", code, doc)
	}
	if code, doc := get("/admin/api/sync-volume?days=7"); code != http.StatusOK {
		t.Fatalf("volume: %d", code)
	} else if days, ok := doc["days"].([]any); !ok || len(days) != 7 {
		t.Fatalf("volume days: %v", doc)
	}
	if code, doc := get("/admin/api/growth?days=7"); code != http.StatusOK {
		t.Fatalf("growth: %d", code)
	} else {
		found := false
		if days, ok := doc["days"].([]any); ok {
			for _, d := range days {
				if m, ok := d.(map[string]any); ok && m["total"] == float64(1) {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("growth missing today's account: %v", doc)
		}
	}
	if code, doc := get("/admin/api/sync-errors"); code != http.StatusOK {
		t.Fatalf("errors: %d", code)
	} else if doc["count"] != float64(1) {
		t.Fatalf("error count = %v, want 1 (denied): %v", doc["count"], doc)
	}
}

func TestAdminDuplicates(t *testing.T) {
	h := testHandler(t)
	for _, u := range []struct{ id, name, mail string }{
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "amy", "amy@x.org"},
		{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "amy2", "amy@x.org"},
		{"cccccccccccccccccccccccccccccccc", "amy", "other@x.org"},
	} {
		if _, err := h.DB.Exec(`INSERT INTO sync_users(user_id,username,email) VALUES(?,?,?)`,
			u.id, u.name, u.mail); err != nil {
			t.Fatal(err)
		}
	}
	rec := httptest.NewRecorder()
	h.AdminDuplicates(rec, httptest.NewRequest(http.MethodGet, "/admin/api/duplicates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var doc struct {
		Groups []struct {
			Field string   `json:"field"`
			Value string   `json:"value"`
			IDs   []string `json:"ids"`
		} `json:"groups"`
		Count int `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Count != 2 {
		t.Fatalf("groups = %d, want 2 (email + username): %s", doc.Count, rec.Body.String())
	}
	for _, g := range doc.Groups {
		if len(g.IDs) != 2 {
			t.Fatalf("group %+v must hold 2 ids", g)
		}
	}
}

func TestAdminMotdExpiry(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Expiring")
	set := func(body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin/api/clusters/"+id+"/motd", strings.NewReader(body))
		req = mux.SetURLVars(req, map[string]string{"id": id})
		rec := httptest.NewRecorder()
		h.AdminSetMOTD(rec, req)
		return rec.Code
	}
	if code := set(`{"motd":"tonight","minutes":60}`); code != http.StatusOK {
		t.Fatalf("motd with expiry: %d", code)
	}
	var motd, until string
	if err := h.DB.QueryRow(`SELECT motd,motd_until FROM clusters WHERE id=?`, id).Scan(&motd, &until); err != nil || motd != "tonight" || until == "" {
		t.Fatalf("motd stored: %q %q (err %v)", motd, until, err)
	}
	// Expired MOTD reads blank on the public list but stays stored.
	if _, err := h.DB.Exec(`UPDATE clusters SET motd_until='2020-01-01T00:00:00Z' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	var list struct {
		Clusters []Cluster `json:"clusters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Clusters) != 1 || list.Clusters[0].MOTD != "" {
		t.Fatalf("expired motd still shown: %+v", list.Clusters)
	}
	if code := set(`{"motd":"x","minutes":-1}`); code != http.StatusBadRequest {
		t.Fatalf("negative minutes: got %d, want 400", code)
	}
}
