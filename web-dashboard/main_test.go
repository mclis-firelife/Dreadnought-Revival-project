package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

func testServer() *server {
	return &server{
		cfg:      config{adminKey: "test-admin-key", internalKey: "test-admin-key"},
		log:      logrus.New(),
		sessions: newSessions(),
		http:     &http.Client{Timeout: 5 * time.Second},
	}
}

func TestCheckAdminKey(t *testing.T) {
	if !checkAdminKey("test-admin-key", "test-admin-key") {
		t.Fatal("equal keys must match")
	}
	if checkAdminKey("wrong", "test-admin-key") {
		t.Fatal("wrong key must not match")
	}
	if checkAdminKey("", "test-admin-key") {
		t.Fatal("empty key must not match")
	}
}

func TestSessionsMintValidRevoke(t *testing.T) {
	s := newSessions()
	tok, err := s.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !s.valid(tok) {
		t.Fatal("fresh token must be valid")
	}
	if s.valid("nope") {
		t.Fatal("unknown token must be invalid")
	}
	s.revoke(tok)
	if s.valid(tok) {
		t.Fatal("revoked token must be invalid")
	}
}

func TestSessionsExpiry(t *testing.T) {
	s := newSessions()
	tok, err := s.mint()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	s.mu.Lock()
	s.tokens[tok] = time.Now().Add(-time.Minute)
	s.mu.Unlock()
	if s.valid(tok) {
		t.Fatal("expired token must be invalid")
	}
}

func TestRequireAuthRejectsAnonymous(t *testing.T) {
	s := testServer()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rec := httptest.NewRecorder()
	s.requireAuth(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestRequireAuthAcceptsHeader(t *testing.T) {
	s := testServer()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec := httptest.NewRecorder()
	s.requireAuth(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
}

func TestInstanceIDValidation(t *testing.T) {
	s := testServer()
	req := httptest.NewRequest(http.MethodGet, "/api/instance/../admin", nil)
	req = mux.SetURLVars(req, map[string]string{"id": "../admin"})
	rec := httptest.NewRecorder()
	s.apiInstance(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for traversal id", rec.Code)
	}
}

func TestTailFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.log")
	var sb strings.Builder
	for i := 1; i <= 10; i++ {
		sb.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, truncated, err := tailFile(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("expected truncated=true")
	}
	if len(lines) != 3 || lines[2] != "line 10" {
		t.Fatalf("unexpected tail: %v", lines)
	}
	if _, _, err := tailFile(filepath.Join(dir, "missing.log"), 10); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestClampLines(t *testing.T) {
	if clampLines("") != 200 || clampLines("abc") != 200 {
		t.Fatal("default must be 200")
	}
	if clampLines("5000") != 2000 {
		t.Fatal("must cap at 2000")
	}
	if clampLines("50") != 50 {
		t.Fatal("must pass through valid values")
	}
}

func TestTrimLeadingSlash(t *testing.T) {
	if trimLeadingSlash("//app.js") != "app.js" {
		t.Fatal("must strip all leading slashes")
	}
}

func authedRequest(t *testing.T, method, path, body string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	return httptest.NewRecorder(), req
}

func TestNormJoinIDFoldsBothSpellings(t *testing.T) {
	dashed := "09C6570A-36D3-4DE2-9485-9AE9D6A44673"
	undashed := "09c6570a36d34de294859ae9d6a44673"
	if normJoinID(dashed) != undashed {
		t.Fatalf("normJoinID(%q) = %q, want %q", dashed, normJoinID(dashed), undashed)
	}
}

// Validation must reject before any upstream call: these tests make no
// network requests (there is no upstream listening in tests).
func TestGrantAllRejectsEmpty(t *testing.T) {
	s := testServer()
	rec, req := authedRequest(t, http.MethodPost, "/api/grant-all", `{}`)
	s.apiGrantAll(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for empty grant-all", rec.Code)
	}
}

func TestGrantAllRejectsNegative(t *testing.T) {
	s := testServer()
	rec, req := authedRequest(t, http.MethodPost, "/api/grant-all", `{"credits":-5}`)
	s.apiGrantAll(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for negative grant-all", rec.Code)
	}
}

func TestGrantAllRejectsGarbage(t *testing.T) {
	s := testServer()
	rec, req := authedRequest(t, http.MethodPost, "/api/grant-all", `not json`)
	s.apiGrantAll(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for malformed grant-all", rec.Code)
	}
}

func auditTestServer(t *testing.T, runDir string) *server {
	t.Helper()
	s := testServer()
	s.cfg.runDir = runDir
	return s
}

func TestAuditAppendsAndReadsBack(t *testing.T) {
	s := auditTestServer(t, t.TempDir())
	s.audit("grant", "alice")
	s.audit("ban", "mallory")

	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec := httptest.NewRecorder()
	s.requireAuth(http.HandlerFunc(s.apiAudit)).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	// Entries are JSON lines carried as JSON strings (double-encoded), so
	// parse twice: envelope first, then each entry.
	var doc struct {
		Entries []string `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(doc.Entries) != 2 {
		t.Fatalf("got %d entries, want 2 (%s)", len(doc.Entries), rec.Body.String())
	}
	seen := map[string]bool{}
	for _, line := range doc.Entries {
		var e struct {
			Action string `json:"action"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode entry %q: %v", line, err)
		}
		seen[e.Action+"/"+e.Detail] = true
	}
	if !seen["grant/alice"] || !seen["ban/mallory"] {
		t.Errorf("entries missing: %v", seen)
	}
}

func TestAuditEmptyWhenNoLogYet(t *testing.T) {
	s := auditTestServer(t, t.TempDir())
	rec := httptest.NewRecorder()
	s.apiAudit(rec, httptest.NewRequest(http.MethodGet, "/api/audit", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
}

func TestCrashesListsAndTails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "crash-1.log"), []byte("boom\nline2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "crash-1.dmp"), []byte{0, 1, 2, 3}, 0o600); err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.cfg.runDir = dir
	// crashDir looks beside runDir; point it by env instead.
	t.Setenv("CRASH_REPORT_DIR", dir)

	rec := httptest.NewRecorder()
	s.apiCrashes(rec, httptest.NewRequest(http.MethodGet, "/api/crashes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "crash-1.log") {
		t.Errorf("missing entry: %s", rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/crashes?file=crash-1.log", nil)
	rec = httptest.NewRecorder()
	s.apiCrashes(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("tail failed: %d %s", rec.Code, rec.Body.String())
	}

	// Binary dumps are listed but refused for viewing.
	req = httptest.NewRequest(http.MethodGet, "/api/crashes?file=crash-1.dmp", nil)
	rec = httptest.NewRecorder()
	s.apiCrashes(rec, req)
	if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"lines"`) {
		t.Errorf("binary dump served for viewing: %s", rec.Body.String())
	}

	// Traversal is rejected.
	req = httptest.NewRequest(http.MethodGet, "/api/crashes?file=../x", nil)
	rec = httptest.NewRecorder()
	s.apiCrashes(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want 400 for traversal", rec.Code)
	}
}

func TestReportsProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/api/reports" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"reports":[{"id":7,"when":"2026-09-29 10:00:00","player":"abc","type":"T","name":"N","details":"D"}]}`))
	}))
	defer upstream.Close()
	s := testServer()
	s.cfg.mmogURL = upstream.URL
	rec := httptest.NewRecorder()
	s.apiReports(rec, httptest.NewRequest(http.MethodGet, "/api/reports", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"id":7`) {
		t.Fatalf("report missing: %s", rec.Body.String())
	}
}

func TestSeriesRejectsUnknown(t *testing.T) {
	s := testServer()
	for _, path := range []string{"/api/series?metrics=bogus", "/api/series?metrics=online&range=bogus"} {
		rec := httptest.NewRecorder()
		s.apiSeries(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", path, rec.Code)
		}
	}
}

func writeSampleLines(t *testing.T, path string, n int) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	for i := 0; i < n; i++ {
		line, _ := json.Marshal(gaugeSample{T: now - int64((n - i) * 60), Online: i, Queued: i * 2})
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
}

func TestSeriesGaugesFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	// 120 one-second samples: with range=2m every bucket holds its sample
	// and the last bucket is the newest value (deterministic total).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	for i := 0; i < 120; i++ {
		line, _ := json.Marshal(gaugeSample{T: now - int64(120-i), Online: i, Queued: i * 2})
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	t.Setenv("DASH_HIST_FILE", path)
	s := testServer()
	rec := httptest.NewRecorder()
	s.apiSeries(rec, httptest.NewRequest(http.MethodGet, "/api/series?metrics=online,queued&range=2m", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Series map[string]struct {
			Points [][2]any `json:"points"`
			Total  float64  `json:"total"`
		} `json:"series"`
		Recorded string `json:"recorded_since"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	online, ok := doc.Series["online"]
	if !ok || len(online.Points) < 100 {
		t.Fatalf("online points = %d, want most of 120: %s", len(online.Points), rec.Body.String())
	}
	if online.Total != 119 {
		t.Fatalf("online total (newest value) = %v, want 119", online.Total)
	}
	queued, ok := doc.Series["queued"]
	if !ok || queued.Total != 238 {
		t.Fatalf("queued total = %v, want 238", queued.Total)
	}
	if doc.Recorded == "" {
		t.Error("recorded_since must be set with samples present")
	}
}

func TestSeriesCountersProxied(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/api/series" || r.URL.Query().Get("metric") != "kills" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metric":"kills","points":[[100,1],[200,6]],"total":7}`))
	}))
	defer upstream.Close()
	s := testServer()
	s.cfg.mmogURL = upstream.URL
	rec := httptest.NewRecorder()
	s.apiSeries(rec, httptest.NewRequest(http.MethodGet, "/api/series?metrics=kills&range=24h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Series map[string]struct {
			Points [][2]any `json:"points"`
			Total  float64  `json:"total"`
		} `json:"series"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	kills, ok := doc.Series["kills"]
	if !ok || kills.Total != 7 || len(kills.Points) != 2 {
		t.Fatalf("unexpected kills series: %s", rec.Body.String())
	}
}

func TestRecordSampleAppendsAndPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	s := testServer()
	s.cfg.runDir = t.TempDir()
	t.Setenv("DASH_HIST_FILE", path)
	s.recordSample(gaugeSample{T: 100, Online: 3})
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), `"online":3`) {
		t.Fatalf("sample not appended: %q %v", string(raw), err)
	}
	// Shrink the cap so 100 lines overflow, then prune keeps newest.
	oldMax := sampleMaxLines
	sampleMaxLines = 90
	defer func() { sampleMaxLines = oldMax }()
	writeSampleLines(t, path, 100)
	s.pruneSamples()
	after, _ := os.ReadFile(path)
	if n := strings.Count(string(after), "\n"); n >= 100 || n == 0 {
		t.Fatalf("prune kept %d lines, want fewer but non-zero", n)
	}
}

func TestSLAComputesPercent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	for i := 0; i < 10; i++ {
		up := i != 9 // auth down on the last sample: 90%.
		line, _ := json.Marshal(gaugeSample{T: now - int64(10-i)*60,
			Services: map[string]bool{"auth-server": up, "mmogbrain": true}})
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	t.Setenv("DASH_HIST_FILE", path)
	s := testServer()
	rec := httptest.NewRecorder()
	s.apiSLA(rec, httptest.NewRequest(http.MethodGet, "/api/sla?range=1h", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Services map[string]float64 `json:"services"`
		Samples  int                `json:"samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Samples != 10 {
		t.Fatalf("samples = %d, want 10", doc.Samples)
	}
	if doc.Services["auth-server"] != 90 {
		t.Fatalf("auth sla = %v, want 90", doc.Services["auth-server"])
	}
	if doc.Services["mmogbrain"] != 100 {
		t.Fatalf("mmog sla = %v, want 100", doc.Services["mmogbrain"])
	}
	if _, ok := doc.Services["game-manager"]; ok {
		t.Fatalf("unrecorded service must be absent: %v", doc.Services)
	}
}

func TestEventsAddListDelete(t *testing.T) {
	s := testServer()
	t.Setenv("DASH_EVENTS_FILE", filepath.Join(t.TempDir(), "events.jsonl"))
	post := func(body string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.apiEvents(rec, httptest.NewRequest(http.MethodPost, "/api/events", strings.NewReader(body)))
		var doc map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		return rec.Code, doc
	}
	if code, _ := post(`{"label":""}`); code != http.StatusBadRequest {
		t.Fatalf("empty label: got %d, want 400", code)
	}
	code, doc := post(`{"label":"deploy 42"}`)
	if code != http.StatusOK {
		t.Fatalf("add: got %d (%v)", code, doc)
	}
	ev, _ := doc["event"].(map[string]any)
	id, _ := ev["id"].(string)
	if id == "" {
		t.Fatalf("no id back: %v", doc)
	}
	rec := httptest.NewRecorder()
	s.apiEvents(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))
	var list struct {
		Events []dashEvent `json:"events"`
		Count  int         `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || list.Count != 1 {
		t.Fatalf("list: %v (%s)", err, rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/events/"+id, nil)
	req = mux.SetURLVars(req, map[string]string{"id": id})
	rec = httptest.NewRecorder()
	s.apiDeleteEvent(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.apiDeleteEvent(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: got %d, want 404", rec.Code)
	}
}

func TestStatsProxiesPassThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"path":"` + r.URL.Path + `"}`))
	}))
	defer upstream.Close()
	s := testServer()
	s.cfg.mmogURL = upstream.URL
	for path, fn := range map[string]func(http.ResponseWriter, *http.Request){
		"/api/sleepers":   s.apiSleepers,
		"/api/wealth":     s.apiWealth,
		"/api/ships":      s.apiShips,
		"/api/mode-stats": s.apiModeStats,
	} {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func controlTestServer(t *testing.T, root string) *server {
	t.Helper()
	s := testServer()
	s.cfg.runDir = filepath.Join(root, "run")
	if err := os.MkdirAll(s.cfg.runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPO_ROOT", root)
	return s
}

func TestSetupStateFirstRun(t *testing.T) {
	root := t.TempDir()
	s := controlTestServer(t, root)
	rec := httptest.NewRecorder()
	s.apiSetupState(rec, httptest.NewRequest(http.MethodGet, "/api/setup-state", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	var doc struct {
		SecretsExists bool           `json:"secrets_env_exists"`
		Binaries      map[string]bool `json:"binaries"`
		Running       map[string]bool `json:"running"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.SecretsExists {
		t.Fatal("fresh temp root must report no secrets.env")
	}
	if doc.Binaries["mmogbrain"] {
		t.Fatal("no binaries exist in a temp root")
	}
	if doc.Running["mmogbrain"] {
		t.Fatal("nothing can run in a temp root")
	}
}

func TestSecretsRoundtrip(t *testing.T) {
	root := t.TempDir()
	s := controlTestServer(t, root)
	rec := httptest.NewRecorder()
	s.apiSecretsSet(rec, httptest.NewRequest(http.MethodPost, "/api/secrets",
		strings.NewReader(`{"content":"# test\nJWT_SECRET=abc\n"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(filepath.Join(root, "run", "secrets.env"))
	if err != nil || !strings.Contains(string(raw), "JWT_SECRET=abc") {
		t.Fatalf("not on disk: %q %v", string(raw), err)
	}
	if st, _ := os.Stat(filepath.Join(root, "run", "secrets.env")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
	rec = httptest.NewRecorder()
	s.apiSecretsGet(rec, httptest.NewRequest(http.MethodGet, "/api/secrets", nil))
	var doc struct {
		Content string `json:"content"`
		Exists  bool   `json:"exists"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || !doc.Exists || !strings.Contains(doc.Content, "JWT_SECRET=abc") {
		t.Fatalf("read back: %v %s", err, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.apiSecretsSet(rec, httptest.NewRequest(http.MethodPost, "/api/secrets", strings.NewReader(`{"content":"  "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty save: got %d, want 400", rec.Code)
	}
}

func TestServiceStopOneValidation(t *testing.T) {
	root := t.TempDir()
	s := controlTestServer(t, root)
	stop := func(name string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/services/stop/"+name, nil)
		req = mux.SetURLVars(req, map[string]string{"name": name})
		rec := httptest.NewRecorder()
		s.apiServiceStopOne(rec, req)
		return rec.Code
	}
	if code := stop("web-dashboard"); code != http.StatusBadRequest {
		t.Fatalf("self-stop: got %d, want 400", code)
	}
	if code := stop("nope"); code != http.StatusBadRequest {
		t.Fatalf("unknown service: got %d, want 400", code)
	}
	if code := stop("mmogbrain"); code != http.StatusNotFound {
		t.Fatalf("no pidfile: got %d, want 404", code)
	}
	// Stale pidfile (no such process, or another process): never kills.
	if err := os.WriteFile(filepath.Join(s.cfg.runDir, "mmogbrain.pid"), []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := stop("mmogbrain"); code == http.StatusOK {
		t.Fatal("stale pidfile must not report success")
	}
}
