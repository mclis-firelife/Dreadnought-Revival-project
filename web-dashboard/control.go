package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

// Service control + first-run setup from the dashboard UI.
//
// The dashboard runs on the same host as the stack, so it can operate the
// shipped scripts without modifying them: "Start all" runs
// scripts/start-services.sh, "Stop all" runs scripts/stop-services.sh,
// "Run setup" runs scripts/setup.sh. Output streams into
// run/web-dashboard-setup.log, which the UI tails (long builds take
// minutes; the call returns immediately with {started:true}).
//
// Per-service stop kills run/<name>.pid after verifying /proc/<pid>/cmdline
// really is that service — never a blind kill, never the dashboard itself.
// Per-service start is intentionally absent: "start all" skips whatever
// already runs, which covers it without duplicating the start scripts'
// per-service flags and env.
//
// First run: only the dashboard is up (web-dashboard/start.sh), there is no
// run/secrets.env yet, and the UI opens on the Setup tab instead of the
// overview. The operator installs packages, writes secrets.env (JWT_SECRET
// and the ADMIN_KEY the dashboard generated on its first start are
// prefilled), runs setup, and starts everything — all from here.
// The secrets editor is raw text (lossless, comments preserved), mode 0600.

// stoppableServices are the stack services the dashboard may stop by pidfile.
// web-dashboard itself is excluded on purpose (no remote self-kill), and
// master-master is standalone with its own start/stop scripts.
var stoppableServices = map[string]bool{
	"auth-server": true, "legacy-api": true, "mmogbrain": true,
	"master-server": true, "game-manager": true, "dn-dedicated": true,
	"gateway": true, "sync-agent": true,
}

var jobMu sync.Mutex
var jobRunning = ""

// setupLogPath is the live log the UI tails while scripts run.
func (s *server) setupLogPath() string {
	return filepath.Join(s.cfg.runDir, "web-dashboard-setup.log")
}

func (s *server) appendSetupLog(format string, args ...any) {
	line := time.Now().UTC().Format("15:04:05") + " " + fmt.Sprintf(format, args...) + "\n"
	f, err := os.OpenFile(s.setupLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.log.WithError(err).Warn("setup log write failed")
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

// runScript launches one of the shipped scripts in the background (repo root
// as cwd — the dashboard is started from there, like every service).
func (s *server) runScript(tag, script string, args ...string) (started bool) {
	jobMu.Lock()
	if jobRunning != "" {
		jobMu.Unlock()
		return false
	}
	jobRunning = tag
	jobMu.Unlock()
	s.appendSetupLog("=== %s: %s %s ===", tag, script, strings.Join(args, " "))
	go func() {
		defer func() {
			jobMu.Lock()
			jobRunning = ""
			jobMu.Unlock()
		}()
		cmd := exec.Command("bash", append([]string{script}, args...)...)
		cmd.Dir = s.repoRoot()
		out, err := cmd.CombinedOutput()
		if len(out) > 0 {
			f, ferr := os.OpenFile(s.setupLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if ferr == nil {
				_, _ = f.Write(out)
				if !strings.HasSuffix(string(out), "\n") {
					_, _ = f.WriteString("\n")
				}
				_ = f.Close()
			}
		}
		if err != nil {
			s.appendSetupLog("=== %s FAILED: %v ===", tag, err)
			s.log.WithError(err).Warn("dashboard script failed: " + tag)
			return
		}
		s.appendSetupLog("=== %s done ===", tag)
	}()
	return true
}

func (s *server) repoRoot() string {
	if v := os.Getenv("REPO_ROOT"); v != "" {
		return v
	}
	return "."
}

// apiSetupState reports everything the Setup tab needs: secrets file state,
// toolchain presence, binary presence, and which services answer.
func (s *server) apiSetupState(w http.ResponseWriter, _ *http.Request) {
	root := s.repoRoot()
	secretsPath := filepath.Join(root, "run", "secrets.env")
	secrets := ""
	if raw, err := os.ReadFile(secretsPath); err == nil {
		secrets = string(raw)
	}
	has := func(key string) bool {
		for _, line := range strings.Split(secrets, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
				return strings.TrimSpace(v) != ""
			}
		}
		return false
	}
	lookPath := func(name string) bool {
		_, err := exec.LookPath(name)
		return err == nil
	}
	binaries := map[string]bool{}
	for _, b := range []string{"auth-server", "legacy-api", "mmogbrain", "master-server",
		"game-manager", "dn-dedicated", "gateway", "sync-agent", "web-dashboard",
		"dn-launcher.exe", "dn-dedicated-browser.exe"} {
		binaries[b] = fileExists(filepath.Join(root, "run", b))
	}
	running := map[string]bool{}
	for name := range stoppableServices {
		running[name] = s.serviceAlive(name)
	}
	jobMu.Lock()
	job := jobRunning
	jobMu.Unlock()
	keySource := "env"
	if os.Getenv("ADMIN_KEY") == "" {
		if has("ADMIN_KEY") {
			keySource = "secrets.env"
		} else {
			keySource = "web-dashboard.env (generated on first dashboard start)"
		}
	}
	gameBinary := os.Getenv("GAME_BINARY")
	writeJSON(w, http.StatusOK, map[string]any{
		"secrets_env_exists": secrets != "",
		"jwt_set":            has("JWT_SECRET"),
		"admin_set":          has("ADMIN_KEY"),
		"game_binary":        gameBinary,
		"game_binary_exists": gameBinary != "" && fileExists(gameBinary),
		"go_present":         lookPath("go"),
		"wine_present":       lookPath("wine"),
		"binaries":           binaries,
		"running":            running,
		"job_running":        job,
		"key_source":         keySource,
	})
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

// serviceAlive checks run/<name>.pid and verifies the process command line
// really belongs to that service before believing it.
func (s *server) serviceAlive(name string) bool {
	pid, ok := s.servicePID(name)
	if !ok {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	return strings.Contains(strings.ReplaceAll(string(raw), "\x00", " "), name)
}

func (s *server) servicePID(name string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(s.cfg.runDir, name+".pid"))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func (s *server) apiServicesStartAll(w http.ResponseWriter, _ *http.Request) {
	if !s.runScript("start-all", "scripts/start-services.sh") {
		writeError(w, http.StatusConflict, "another job is already running")
		return
	}
	s.audit("services-start-all", "scripts/start-services.sh")
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *server) apiServicesStopAll(w http.ResponseWriter, _ *http.Request) {
	if !s.runScript("stop-all", "scripts/stop-services.sh") {
		writeError(w, http.StatusConflict, "another job is already running")
		return
	}
	s.audit("services-stop-all", "scripts/stop-services.sh")
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *server) apiServiceStopOne(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	if !stoppableServices[name] {
		writeError(w, http.StatusBadRequest, "unknown or protected service")
		return
	}
	pid, ok := s.servicePID(name)
	if !ok {
		writeError(w, http.StatusNotFound, "no pidfile: not running (or started by hand)")
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		writeError(w, http.StatusNotFound, "process gone")
		return
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || !strings.Contains(strings.ReplaceAll(string(raw), "\x00", " "), name) {
		writeError(w, http.StatusConflict, "pidfile is stale (another process now): not killing")
		return
	}
	if err := proc.Signal(os.Interrupt); err != nil {
		writeError(w, http.StatusInternalServerError, "signal failed: "+err.Error())
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.runDir, name+".pid"))
	s.log.WithField("service", name).Warn("dashboard stopped a service")
	s.audit("service-stop", name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *server) apiSetupRun(w http.ResponseWriter, _ *http.Request) {
	if !s.runScript("setup", "scripts/setup.sh") {
		writeError(w, http.StatusConflict, "another job is already running")
		return
	}
	s.audit("setup-run", "scripts/setup.sh")
	writeJSON(w, http.StatusOK, map[string]string{"status": "started"})
}

func (s *server) apiSetupLog(w http.ResponseWriter, r *http.Request) {
	lines, truncated, err := tailFile(s.setupLogPath(), clampLines(r.URL.Query().Get("lines")))
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"lines": []string{}, "running": ""})
		return
	}
	jobMu.Lock()
	job := jobRunning
	jobMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines, "truncated": truncated, "running": job})
}

// ---------------------------------------------------------------- secrets editor

// apiSecretsGet returns run/secrets.env verbatim (or "" when absent — the
// editor then starts from the shipped example template).
func (s *server) apiSecretsGet(w http.ResponseWriter, _ *http.Request) {
	path := filepath.Join(s.repoRoot(), "run", "secrets.env")
	raw, err := os.ReadFile(path)
	if err != nil {
		raw, _ = os.ReadFile(filepath.Join(s.repoRoot(), "scripts", "secrets.env.example"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"content": string(raw), "exists": err == nil})
}

// apiSecretsSet writes run/secrets.env verbatim (mode 600). Raw text on
// purpose: comments and layout survive untouched, nothing is parsed.
func (s *server) apiSecretsSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "empty content refused")
		return
	}
	path := filepath.Join(s.repoRoot(), "run", "secrets.env")
	if err := os.MkdirAll(filepath.Join(s.repoRoot(), "run"), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create run dir")
		return
	}
	if !strings.HasSuffix(req.Content, "\n") {
		req.Content += "\n"
	}
	if err := os.WriteFile(path, []byte(req.Content), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot write secrets.env")
		return
	}
	s.log.Warn("dashboard wrote run/secrets.env (services pick it up on (re)start; the dashboard keeps its startup key)")
	s.audit("secrets-write", "run/secrets.env")
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// ---------------------------------------------------------------- live config keys

// configKeys is the curated whitelist for the Config tab editor: safe
// gameplay knobs an operator may change without breaking the install.
// Secrets, paths, URLs and ports stay in the raw editor above. Every key
// takes effect on service (re)start — the UI says so next to Save.
type configKey struct {
	Key     string `json:"key"`
	Desc    string `json:"desc"`
	Default string `json:"default"`
	Value   string `json:"value"`
}

var editableConfigKeys = []struct{ key, desc, def string }{
	{"PLAYERS_PER_MATCH", "Queued players per match (1 = private matches for testing)", "1"},
	{"DN_MATCH_AUTOSCALE", "Size matches by online players (1) or fixed count (0)", "1"},
	{"DN_MATCH_MAX_PLAYERS", "Largest auto-scaled match", "10"},
	{"DN_MATCH_MAX_WAIT", "Wait for idle players after first queue (e.g. 60s)", "60s"},
	{"DN_REWARD_WIN_CREDITS", "Credits per win (placeholders, originals lost)", "1500"},
	{"DN_REWARD_LOSS_CREDITS", "Credits per loss", "750"},
	{"DN_REWARD_KILL_CREDITS", "Credits per kill", "100"},
	{"DN_REWARD_WIN_XP", "XP per win", "1000"},
	{"DN_REWARD_LOSS_XP", "XP per loss", "500"},
	{"DN_REWARD_KILL_XP", "XP per kill", "50"},
	{"DN_TECHTREE_LIMIT", "Tech tree items per manufacturer group (bisect aid)", ""},
	{"DN_NO_DEFER_PLAYER_FLEETS", "Answer fleet requests immediately (1, reproduces a client bug)", ""},
	{"DN_ALLOW_MOCK_INSTANCES", "Allow mock battle instances (testing)", ""},
	{"SYNC_INTERVAL", "Roaming push/pull cadence (min 10s)", "60s"},
	{"SYNC_ROTATE_DAYS", "Secret self-rotation days (0 disables)", "30"},
	{"CLUSTER_MOTD", "Message of the day shown in the browser", ""},
	{"CLUSTER_VERSION", "Version shown in the browser", "1.0"},
}

// secretsValues parses KEY=VALUE lines (skips blanks/comments, first wins).
func secretsValues(content string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, seen := out[k]; !seen {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

func (s *server) secretsContent() string {
	raw, err := os.ReadFile(filepath.Join(s.repoRoot(), "run", "secrets.env"))
	if err != nil {
		return ""
	}
	return string(raw)
}

func (s *server) apiConfigKeys(w http.ResponseWriter, _ *http.Request) {
	vals := secretsValues(s.secretsContent())
	out := make([]configKey, 0, len(editableConfigKeys))
	for _, k := range editableConfigKeys {
		out = append(out, configKey{Key: k.key, Desc: k.desc, Default: k.def, Value: vals[k.key]})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *server) apiSetConfigKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	allowed := map[string]bool{}
	for _, k := range editableConfigKeys {
		allowed[k.key] = true
	}
	changed := []string{}
	for k := range req.Values {
		if !allowed[k] {
			writeError(w, http.StatusBadRequest, "not editable here: "+k)
			return
		}
		changed = append(changed, k)
	}
	if len(changed) == 0 {
		writeError(w, http.StatusBadRequest, "nothing to save")
		return
	}
	sort.Strings(changed)
	// Merge line-wise: replace existing KEY= lines, append the rest at the
	// end. Comments, order and unknown keys survive untouched.
	lines := strings.Split(s.secretsContent(), "\n")
	seen := map[string]bool{}
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if k, _, ok := strings.Cut(trimmed, "="); ok {
			k = strings.TrimSpace(k)
			if v, ok := req.Values[k]; ok {
				lines[i] = k + "=" + strings.TrimSpace(strings.ReplaceAll(v, "\n", " "))
				seen[k] = true
			}
		}
	}
	for _, k := range changed {
		if !seen[k] {
			lines = append(lines, k+"="+strings.TrimSpace(strings.ReplaceAll(req.Values[k], "\n", " ")))
		}
	}
	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	path := filepath.Join(s.repoRoot(), "run", "secrets.env")
	if err := os.MkdirAll(filepath.Join(s.repoRoot(), "run"), 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot create run dir")
		return
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot write secrets.env")
		return
	}
	sort.Strings(changed)
	s.log.WithField("keys", strings.Join(changed, ",")).Warn("dashboard edited live config keys (restart services to apply)")
	s.audit("config-keys", strings.Join(changed, ","))
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "keys": changed,
		"note": "restart services (Setup tab) to apply"})
}

// ---------------------------------------------------------------- player history

// apiPlayerHistory collects audit entries mentioning one account (grants,
// bans, resets, provisions): the operator paper trail per player. Matches
// both id spellings.
func (s *server) apiPlayerHistory(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id required")
		return
	}
	folded := normJoinID(id)
	lines, _, err := tailFile(s.auditPath(), 5000)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"entries": []any{}})
		return
	}
	type entry struct {
		Time   string `json:"time"`
		Action string `json:"action"`
		Detail string `json:"detail"`
	}
	out := []entry{}
	for _, line := range lines {
		var e entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		hay := normJoinID(e.Detail)
		if !strings.Contains(line, id) && !strings.Contains(hay, folded) {
			continue
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": out, "count": len(out)})
}

// ---------------------------------------------------------------- scheduled events

// scheduledItem is one future operator action: a launcher tile upsert (or
// delete) or a chat broadcast, executed by the dashboard's minute ticker.
type scheduledItem struct {
	ID      string `json:"id"`
	At      string `json:"at"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Channel string `json:"channel"`
	TileID  string `json:"tile_id"`
	Done    string `json:"done"`
}

func (s *server) scheduledPath() string {
	return filepath.Join(s.cfg.runDir, "web-dashboard-scheduled.json")
}

func (s *server) readScheduled() []scheduledItem {
	out := []scheduledItem{}
	raw, err := os.ReadFile(s.scheduledPath())
	if err != nil {
		return out
	}
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *server) writeScheduled(items []scheduledItem) error {
	raw, _ := json.MarshalIndent(items, "", "  ")
	return os.WriteFile(s.scheduledPath(), raw, 0o600)
}

func (s *server) apiScheduled(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		items := s.readScheduled()
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
	case http.MethodPost:
		var req struct {
			At      string `json:"at"`
			Kind    string `json:"kind"`
			Title   string `json:"title"`
			Body    string `json:"body"`
			Channel string `json:"channel"`
			TileID  string `json:"tile_id"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(req.At))
		if err != nil || !at.After(time.Now().Add(-time.Minute)) {
			writeError(w, http.StatusBadRequest, "at must be a future RFC3339 time")
			return
		}
		req.Kind = strings.TrimSpace(req.Kind)
		if req.Kind != "tile" && req.Kind != "tile-delete" && req.Kind != "broadcast" {
			writeError(w, http.StatusBadRequest, "kind must be tile, tile-delete or broadcast")
			return
		}
		if len([]rune(req.Title)) > 120 || len([]rune(req.Body)) > 2000 {
			writeError(w, http.StatusBadRequest, "title/body too long")
			return
		}
		if (req.Kind == "tile" || req.Kind == "tile-delete") && strings.TrimSpace(req.TileID) == "" {
			writeError(w, http.StatusBadRequest, "tile_id required for tile actions")
			return
		}
		if (req.Kind == "tile" || req.Kind == "broadcast") && strings.TrimSpace(req.Body) == "" {
			writeError(w, http.StatusBadRequest, "body required for tile/broadcast")
			return
		}
		items := s.readScheduled()
		id := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
		items = append(items, scheduledItem{
			ID: id, At: at.UTC().Format(time.RFC3339), Kind: req.Kind,
			Title: strings.TrimSpace(req.Title), Body: req.Body,
			Channel: strings.TrimSpace(req.Channel), TileID: strings.TrimSpace(req.TileID),
		})
		if err := s.writeScheduled(items); err != nil {
			writeError(w, http.StatusInternalServerError, "cannot store schedule")
			return
		}
		s.audit("schedule-add", req.Kind+" @ "+at.UTC().Format(time.RFC3339))
		writeJSON(w, http.StatusOK, map[string]any{"status": "scheduled", "id": id})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *server) apiDeleteScheduled(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "/\\. ") {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	kept := []scheduledItem{}
	found := false
	for _, it := range s.readScheduled() {
		if it.ID == id {
			found = true
			continue
		}
		kept = append(kept, it)
	}
	if !found {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.writeScheduled(kept); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store schedule")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// startScheduler runs due items every minute: tiles upserted/deleted via the
// same upstream calls as the manual buttons, broadcasts sent as operator.
func (s *server) startScheduler() {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.runDueScheduled()
		}
	}()
}

func (s *server) runDueScheduled() {
	now := time.Now().UTC()
	items := s.readScheduled()
	changed := false
	for i := range items {
		it := &items[i]
		if it.Done != "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, it.At)
		if err != nil || at.After(now) {
			continue
		}
		var runErr string
		switch it.Kind {
		case "tile":
			code, _ := s.upstreamPostJSON(s.cfg.legacyURL, "/admin/tiles", s.cfg.adminKey, map[string]any{
				"id": it.TileID, "type": "announcement", "section_size": "full",
				"title": it.Title, "body": it.Body, "active": true,
			})
			if code != http.StatusOK && code != http.StatusCreated {
				runErr = fmt.Sprintf("HTTP %d", code)
			}
		case "tile-delete":
			code, _ := s.upstreamDelete(s.cfg.legacyURL, "/admin/tiles/"+it.TileID, s.cfg.adminKey)
			if code != http.StatusOK && code != http.StatusNotFound {
				runErr = fmt.Sprintf("HTTP %d", code)
			}
		case "broadcast":
			channel := it.Channel
			if channel == "" {
				channel = "dreadnought.global"
			}
			code, _ := s.upstreamPostJSON(s.cfg.mmogURL, "/admin/broadcast", s.cfg.adminKey, map[string]string{
				"channel": channel, "content": it.Body,
			})
			if code != http.StatusOK {
				runErr = fmt.Sprintf("HTTP %d", code)
			}
		}
		if runErr != "" {
			s.log.WithFields(logrus.Fields{"id": it.ID, "error": runErr}).Warn("scheduled item failed (kept for retry)")
			continue
		}
		it.Done = now.Format(time.RFC3339)
		changed = true
		s.log.WithField("id", it.ID).Info("scheduled item executed")
		s.audit("schedule-run", it.Kind+" "+it.Title)
	}
	if changed {
		// Keep executed items visible (done timestamp) but cap the file.
		if len(items) > 200 {
			items = items[len(items)-200:]
		}
		_ = s.writeScheduled(items)
	}
}
