package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
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
