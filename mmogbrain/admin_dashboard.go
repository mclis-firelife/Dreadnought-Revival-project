package main

// Admin dashboard: GET /admin/dashboard serves one static page; every piece
// of data it shows comes from /admin/api/*, behind the same X-Admin-Key
// middleware as the other /admin routes (ADMIN_KEY, which requireAdminKey
// refuses to leave empty). mmogbrain's HTTP port is not forwarded to the
// internet, so the page is reachable only from the machine or the LAN.
//
// Read-only except one action -- stopping a battle server -- which is logged
// with the caller's address.

import (
	"bufio"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

//go:embed admin_dashboard.html
var adminDashboardHTML []byte

// adminDashboardConfig is what the dashboard needs from main.
type adminDashboardConfig struct {
	controlPlaneURL string // GAME_MGR_URL (dn-dedicated)
	internalKey     string // X-Internal-Key for the control plane
	startedAt       time.Time
}

var adminDash adminDashboardConfig

// constantTimeKeyMiddleware is the admin gate for the dashboard API: the
// same key as adminKeyMiddleware, compared in constant time.
func constantTimeKeyMiddleware(key string) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("X-Admin-Key")
			if key == "" || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func registerAdminDashboard(r *mux.Router, adminKey, controlPlaneURL, internalKey string) {
	adminDash = adminDashboardConfig{controlPlaneURL: controlPlaneURL, internalKey: internalKey, startedAt: time.Now()}
	r.HandleFunc("/admin/dashboard", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(adminDashboardHTML)
	}).Methods(http.MethodGet)

	api := r.PathPrefix("/admin/api").Subrouter()
	api.Use(constantTimeKeyMiddleware(adminKey))
	api.HandleFunc("/overview", adminAPIOverview).Methods(http.MethodGet)
	api.HandleFunc("/instances", adminAPIInstances).Methods(http.MethodGet)
	api.HandleFunc("/instances/{id}", adminAPIStopInstance).Methods(http.MethodDelete)
	api.HandleFunc("/online", adminAPIOnline).Methods(http.MethodGet)
	api.HandleFunc("/matches", adminAPIMatches).Methods(http.MethodGet)
	api.HandleFunc("/players", adminAPIPlayers).Methods(http.MethodGet)
	api.HandleFunc("/reports", adminAPIReports).Methods(http.MethodGet)
	api.HandleFunc("/logs", adminAPILogs).Methods(http.MethodGet)
	api.HandleFunc("/battle-logs", adminAPIBattleLogs).Methods(http.MethodGet)
	api.HandleFunc("/series", adminAPISeries).Methods(http.MethodGet)
	api.HandleFunc("/sleepers", adminAPISleepers).Methods(http.MethodGet)
	api.HandleFunc("/wealth", adminAPIWealth).Methods(http.MethodGet)
	api.HandleFunc("/ships", adminAPIShips).Methods(http.MethodGet)
	api.HandleFunc("/mode-stats", adminAPIModeStats).Methods(http.MethodGet)
}

func writeAdminJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// --- control plane ------------------------------------------------------------

type adminInstance struct {
	ID        string   `json:"id"`
	Port      int      `json:"port"`
	GameMode  string   `json:"game_mode"`
	Map       string   `json:"map"`
	MatchID   string   `json:"match_id"`
	Players   []string `json:"players"`
	Ready     bool     `json:"ready"`
	Running   bool     `json:"running"`
	StartedAt string   `json:"started_at"`
	PID       int      `json:"pid"`
	// Filled in here:
	PlayerNames []string `json:"player_names"`
	Crashed     string   `json:"crashed,omitempty"` // from the battle log
}

func controlPlaneInstances() ([]adminInstance, error) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(adminDash.controlPlaneURL, "/")+"/instances", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Internal-Key", adminDash.internalKey)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Instances []adminInstance `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	for i := range body.Instances {
		for _, pid := range body.Instances[i].Players {
			body.Instances[i].PlayerNames = append(body.Instances[i].PlayerNames, adminPlayerName(pid))
		}
		body.Instances[i].Crashed = battleLogCrash(body.Instances[i].StartedAt, body.Instances[i].Port)
	}
	return body.Instances, nil
}

func adminAPIInstances(w http.ResponseWriter, _ *http.Request) {
	list, err := controlPlaneInstances()
	if err != nil {
		writeAdminJSON(w, map[string]any{"error": "control plane unreachable: " + err.Error(), "instances": []any{}})
		return
	}
	writeAdminJSON(w, map[string]any{"instances": list})
}

func adminAPIStopInstance(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	req, err := http.NewRequest(http.MethodDelete, strings.TrimRight(adminDash.controlPlaneURL, "/")+"/instances/"+id, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.Header.Set("X-Internal-Key", adminDash.internalKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		http.Error(w, "control plane unreachable", http.StatusBadGateway)
		return
	}
	_ = resp.Body.Close()
	logrus.WithFields(logrus.Fields{"instance": id, "status": resp.StatusCode, "by": r.RemoteAddr}).
		Warn("admin dashboard: battle server stopped")
	writeAdminJSON(w, map[string]any{"status": resp.StatusCode})
}

// battleLogCrash reports the crash kind a running instance's battle log shows,
// if any: a crashed host can stay "running" (Wine's debugger holds it).
func battleLogCrash(startedAt string, port int) string {
	t, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return ""
	}
	path := filepath.Join("run", "battle-logs", fmt.Sprintf("battle-%s-port%d.log", t.Local().Format("20060102-150405"), port))
	tail := tailFile(path, 64*1024)
	switch {
	case strings.Contains(tail, "EXCEPTION_STACK_OVERFLOW"):
		return "stack overflow"
	case strings.Contains(tail, "Unhandled Exception"), strings.Contains(tail, "Unhandled page fault"):
		return "access violation"
	}
	return ""
}

// --- players ------------------------------------------------------------------

func adminPlayerName(pid string) string {
	name := mmogPlayerStateForPID(normalizedPlayerStatePID(pid)).displayName
	if name == "" || name == "Local" {
		return "?"
	}
	return name
}

func adminAPIOnline(w http.ResponseWriter, _ *http.Request) {
	type row struct {
		PID    string `json:"pid"`
		Name   string `json:"name"`
		Status string `json:"status"`
		Squad  string `json:"squad,omitempty"`
		Match  string `json:"match,omitempty"`
		Queued string `json:"queued,omitempty"`
	}
	out := []row{}
	database := currentMmogPlayerStateDB()
	for _, p := range socialHubInstance.onlinePlayers() {
		r := row{PID: p.PID, Name: p.Name, Status: p.Status, Squad: squadHubInstance.squadIDOf(p.PID)}
		if database != nil {
			_ = database.QueryRow(`SELECT m.game_mode||' / '||m.map FROM match_slots s JOIN matches m ON m.id=s.match_id
				WHERE s.user_id=? AND m.status='active' LIMIT 1`, p.PID).Scan(&r.Match)
			_ = database.QueryRow(`SELECT game_mode FROM queue_entries WHERE user_id=? AND status='waiting' LIMIT 1`, p.PID).Scan(&r.Queued)
		}
		out = append(out, r)
	}
	writeAdminJSON(w, map[string]any{"players": out})
}

func adminAPIPlayers(w http.ResponseWriter, r *http.Request) {
	database := currentMmogPlayerStateDB()
	if database == nil {
		writeAdminJSON(w, map[string]any{"players": []any{}})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// Before the query: onlinePlayers looks names up, and the store has ONE
	// connection -- a lookup inside an open result set deadlocks it.
	online := map[string]bool{}
	for _, p := range socialHubInstance.onlinePlayers() {
		online[p.PID] = true
	}
	rows, err := database.Query(`SELECT p.user_id, COALESCE(p.display_name,''), p.current_rank, p.current_xp, p.free_xp,
			p.soft_currency, p.premium_currency, p.created_at, p.updated_at,
			(SELECT COUNT(*) FROM player_ship_loadouts l WHERE l.user_id=p.user_id),
			(SELECT COUNT(*) FROM battle_results b WHERE b.user_id=p.user_id),
			(SELECT COUNT(*) FROM battle_results b WHERE b.user_id=p.user_id AND b.outcome='win'),
			(SELECT COALESCE(SUM(kills),0) FROM battle_results b WHERE b.user_id=p.user_id)
		FROM player_state p
		WHERE (?='' OR p.display_name LIKE '%'||?||'%' OR p.user_id LIKE ?||'%')
		ORDER BY p.updated_at DESC LIMIT 100`, q, q, strings.ToLower(strings.ReplaceAll(q, "-", "")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer func() { _ = rows.Close() }()
	type row struct {
		PID      string `json:"pid"`
		Name     string `json:"name"`
		Rank     int    `json:"rank"`
		XP       int64  `json:"xp"`
		FreeXP   int64  `json:"free_xp"`
		Credits  int64  `json:"credits"`
		Premium  int64  `json:"premium"`
		Created  string `json:"created"`
		Updated  string `json:"updated"`
		Ships    int    `json:"ships"`
		Matches  int    `json:"matches"`
		Wins     int    `json:"wins"`
		Kills    int    `json:"kills"`
		IsOnline bool   `json:"online"`
	}
	out := []row{}
	for rows.Next() {
		var x row
		if rows.Scan(&x.PID, &x.Name, &x.Rank, &x.XP, &x.FreeXP, &x.Credits, &x.Premium, &x.Created, &x.Updated,
			&x.Ships, &x.Matches, &x.Wins, &x.Kills) == nil {
			x.IsOnline = online[x.PID]
			out = append(out, x)
		}
	}
	writeAdminJSON(w, map[string]any{"players": out})
}

// --- matches / statistics -----------------------------------------------------

func adminAPIMatches(w http.ResponseWriter, _ *http.Request) {
	database := currentMmogPlayerStateDB()
	type result struct {
		Name    string `json:"name"`
		Team    int    `json:"team"`
		Outcome string `json:"outcome"`
		Kills   int    `json:"kills"`
		Deaths  int    `json:"deaths"`
		Credits int    `json:"credits"`
		XP      int    `json:"xp"`
	}
	type match struct {
		ID        string   `json:"id"`
		Mode      string   `json:"mode"`
		Map       string   `json:"map"`
		Status    string   `json:"status"`
		Started   string   `json:"started"`
		Ended     string   `json:"ended"`
		FleetType int      `json:"fleet_type"`
		Results   []result `json:"results"`
		battleID  string
	}
	out := []*match{}
	if database == nil {
		writeAdminJSON(w, map[string]any{"matches": out})
		return
	}
	rows, err := database.Query(`SELECT id, game_mode, map, status, COALESCE(started_at,''), COALESCE(ended_at,''),
		COALESCE(fleet_type,0), COALESCE(battle_match_id,'') FROM matches ORDER BY created_at DESC LIMIT 40`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		m := &match{}
		if rows.Scan(&m.ID, &m.Mode, &m.Map, &m.Status, &m.Started, &m.Ended, &m.FleetType, &m.battleID) == nil {
			out = append(out, m)
		}
	}
	_ = rows.Close()
	for _, m := range out {
		m.Results = []result{}
		if m.battleID == "" {
			continue
		}
		rs, err := database.Query(`SELECT user_id, team, outcome, kills, deaths, credits, xp FROM battle_results
			WHERE match_id=? OR match_id LIKE ?||'-r%' ORDER BY team, kills DESC`, m.battleID, m.battleID)
		if err != nil {
			continue
		}
		var pids []string
		for rs.Next() {
			var pid string
			var x result
			if rs.Scan(&pid, &x.Team, &x.Outcome, &x.Kills, &x.Deaths, &x.Credits, &x.XP) == nil {
				pids = append(pids, pid)
				m.Results = append(m.Results, x)
			}
		}
		_ = rs.Close()
		// Names AFTER the rows are closed: the store has ONE connection
		// (MaxOpenConns=1), and a query inside an open result set waits for
		// itself forever -- which deadlocked the whole server's database the
		// first time the dashboard was opened (2026-09-29).
		for i := range m.Results {
			m.Results[i].Name = adminPlayerName(pids[i])
		}
	}
	writeAdminJSON(w, map[string]any{"matches": out})
}

func adminAPIOverview(w http.ResponseWriter, _ *http.Request) {
	database := currentMmogPlayerStateDB()
	count := func(q string, args ...any) int {
		if database == nil {
			return 0
		}
		var n int
		_ = database.QueryRow(q, args...).Scan(&n)
		return n
	}
	since := time.Now().Add(-24 * time.Hour).UTC().Format("2006-01-02 15:04:05")
	sinceRFC := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	instances, cpErr := controlPlaneInstances()
	crashed := 0
	for _, i := range instances {
		if i.Crashed != "" {
			crashed++
		}
	}
	modes := map[string]int{}
	if database != nil {
		if rows, err := database.Query(`SELECT game_mode, COUNT(*) FROM matches WHERE created_at >= ? GROUP BY game_mode`, sinceRFC); err == nil {
			for rows.Next() {
				var m string
				var n int
				if rows.Scan(&m, &n) == nil {
					modes[m] = n
				}
			}
			_ = rows.Close()
		}
	}
	services := map[string]bool{}
	for name, port := range map[string]int{"auth-server": 8081, "legacy-api": 8082, "mmogbrain": 8083, "master-server": 8084, "dn-dedicated": 8085} {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		services[name] = err == nil && resp.StatusCode == http.StatusOK
		if err == nil {
			_ = resp.Body.Close()
		}
	}
	ov := map[string]any{
		"uptime_seconds":      int(time.Since(adminDash.startedAt).Seconds()),
		"services":            services,
		"online":              len(socialHubInstance.onlinePlayers()),
		"accounts":            count(`SELECT COUNT(*) FROM player_state`),
		"new_accounts_24h":    count(`SELECT COUNT(*) FROM player_state WHERE created_at >= ?`, since),
		"queued":              count(`SELECT COUNT(*) FROM queue_entries WHERE status='waiting'`),
		"instances":           len(instances),
		"instances_crashed":   crashed,
		"matches_24h":         count(`SELECT COUNT(*) FROM matches WHERE created_at >= ?`, sinceRFC),
		"matches_total":       count(`SELECT COUNT(*) FROM matches`),
		"results_24h":         count(`SELECT COUNT(*) FROM battle_results WHERE created_at >= ?`, since),
		"kills_24h":           count(`SELECT COALESCE(SUM(kills),0) FROM battle_results WHERE created_at >= ?`, since),
		"credits_paid_24h":    count(`SELECT COALESCE(SUM(credits),0) FROM battle_results WHERE created_at >= ?`, since),
		"reports_24h":         count(`SELECT COUNT(*) FROM client_reports WHERE created_at >= ?`, since),
		"modes_24h":           modes,
		"host_crashes_recent": recentHostCrashes(40),
	}
	if cpErr != nil {
		ov["control_plane_error"] = cpErr.Error()
	}
	writeAdminJSON(w, ov)
}

// recentHostCrashes counts crash kinds over the newest n battle logs.
func recentHostCrashes(n int) map[string]int {
	out := map[string]int{"stack overflow": 0, "access violation": 0, "clean": 0}
	for _, f := range newestBattleLogs(n) {
		tail := tailFile(f.path, 96*1024)
		switch {
		case strings.Contains(tail, "EXCEPTION_STACK_OVERFLOW"):
			out["stack overflow"]++
		case strings.Contains(tail, "Unhandled Exception"), strings.Contains(tail, "Unhandled page fault"):
			out["access violation"]++
		default:
			out["clean"]++
		}
	}
	return out
}

func adminAPIReports(w http.ResponseWriter, _ *http.Request) {
	database := currentMmogPlayerStateDB()
	type row struct {
		ID      int    `json:"id"`
		When    string `json:"when"`
		Player  string `json:"player"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Details string `json:"details"`
	}
	out := []row{}
	if database != nil {
		if rows, err := database.Query(`SELECT id, created_at, user_id, type, name, desc FROM client_reports ORDER BY id DESC LIMIT 150`); err == nil {
			var pids []string
			for rows.Next() {
				var x row
				var pid string
				if rows.Scan(&x.ID, &x.When, &pid, &x.Type, &x.Name, &x.Details) == nil {
					pids = append(pids, pid)
					out = append(out, x)
				}
			}
			_ = rows.Close()
			for i := range out { // after Close: one connection (see adminAPIMatches)
				out[i].Player = adminPlayerName(pids[i])
			}
		}
	}
	writeAdminJSON(w, map[string]any{"reports": out})
}

// --- history series ---------------------------------------------------------

// seriesSpecs maps a dashboard graph metric to its table, timestamp column
// and value expression. All timestamp columns are datetime('now') text
// ("2006-01-02 15:04:05" UTC), but rows written by tests or tools may carry
// RFC3339 — parseSeriesTime accepts both, anything else is skipped.
var seriesSpecs = map[string]struct {
	table, timeCol, valExpr, where string
}{
	"matches":  {"matches", "created_at", "1", ""},
	"results":  {"battle_results", "created_at", "1", ""},
	"kills":    {"battle_results", "created_at", "COALESCE(kills,0)", ""},
	"credits":  {"battle_results", "created_at", "COALESCE(credits,0)", ""},
	"accounts": {"player_state", "created_at", "1", ""},
	"reports":  {"client_reports", "created_at", "1", ""},
	// Store turnover in credits (research grants and admin gifts carry
	// other currencies and must not count as spending).
	"spending": {"player_purchases", "purchased_at", "COALESCE(price_paid,0)", "currency='CR'"},
}

func parseSeriesTime(raw string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// adminAPISeries serves bucketed history for dashboard graphs:
// GET /admin/api/series?metric=matches&from=<RFC3339>&to=<RFC3339>&buckets=120
// answers {metric, from, to, buckets, points:[[unix,value]...], total}.
// from/to are optional (unbounded past / now). Filtering and bucketing
// happen in Go — the tables are small (thousands of rows) and this way both
// timestamp spellings count. Unknown metric is 400; a missing table answers
// empty, never 500, so one absent table cannot kill every graph.
func adminAPISeries(w http.ResponseWriter, r *http.Request) {
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	spec, ok := seriesSpecs[metric]
	if !ok {
		http.Error(w, `{"error":"unknown metric"}`, http.StatusBadRequest)
		return
	}
	var from, to time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		t, ok := parseSeriesTime(raw)
		if !ok {
			http.Error(w, `{"error":"bad from timestamp"}`, http.StatusBadRequest)
			return
		}
		from = t
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("to")); raw != "" {
		t, ok := parseSeriesTime(raw)
		if !ok {
			http.Error(w, `{"error":"bad to timestamp"}`, http.StatusBadRequest)
			return
		}
		to = t
	} else {
		to = time.Now().UTC()
	}
	buckets := 120
	if n, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("buckets"))); err == nil {
		buckets = n
	}
	if buckets < 8 {
		buckets = 8
	}
	if buckets > 240 {
		buckets = 240
	}
	type point struct {
		at  time.Time
		val float64
	}
	var points []point
	database := currentMmogPlayerStateDB()
	if database != nil {
		query := `SELECT ` + spec.timeCol + `,` + spec.valExpr + ` FROM ` + spec.table
		if spec.where != "" {
			query += ` WHERE ` + spec.where
		}
		rows, err := database.Query(query)
		if err == nil {
			for rows.Next() {
				var rawT string
				var v float64
				if err := rows.Scan(&rawT, &v); err != nil {
					continue
				}
				at, ok := parseSeriesTime(rawT)
				if !ok {
					continue
				}
				if !from.IsZero() && at.Before(from) {
					continue
				}
				if at.After(to) {
					continue
				}
				points = append(points, point{at: at, val: v})
			}
			_ = rows.Close()
		}
	}
	// No lower bound given: start at the first point (history "since the
	// database began"); empty table: a flat zero line over the last day so
	// the graph renders instead of erroring.
	start := from
	if start.IsZero() {
		start = to.Add(-24 * time.Hour)
		for _, p := range points {
			if p.at.Before(start) {
				start = p.at
			}
		}
	}
	out := make([][2]any, buckets)
	width := to.Sub(start)
	if width <= 0 {
		width = time.Second
	}
	sums := make([]float64, buckets)
	var total float64
	for _, p := range points {
		i := int(p.at.Sub(start) * time.Duration(buckets) / width)
		if i < 0 {
			i = 0
		}
		if i >= buckets {
			i = buckets - 1
		}
		sums[i] += p.val
		total += p.val
	}
	for i := range out {
		out[i] = [2]any{start.Add(time.Duration(i) * width / time.Duration(buckets)).Unix(), sums[i]}
	}
	writeAdminJSON(w, map[string]any{
		"metric": metric, "from": start.UTC().Format(time.RFC3339),
		"to":     to.UTC().Format(time.RFC3339), "buckets": buckets,
		"points": out, "total": total,
	})
}

// AdminAPISleepers handles GET /admin/api/sleepers?days=30 — accounts whose
// last login is older than N days (or never logged in), most dormant first.
// For win-back events and dead-account audits.
func adminAPISleepers(w http.ResponseWriter, r *http.Request) {
	days := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		var n int
		if _, err := fmt.Sscanf(raw, "%d", &n); err == nil && n >= 1 && n <= 3650 {
			days = n
		}
	}
	database := currentMmogPlayerStateDB()
	type row struct {
		PID       string `json:"pid"`
		Name      string `json:"name"`
		Rank      int    `json:"rank"`
		LastLogin string `json:"last_login"`
	}
	out := []row{}
	if database != nil {
		cutoff := time.Now().UTC().AddDate(0, 0, -days)
		// Never-logged-in accounts fall back to created_at: a fresh account
		// reads as recent (not dormant), an old untouched one as dormant.
		rows, err := database.Query(`SELECT user_id,COALESCE(display_name,''),current_rank,
			COALESCE(NULLIF(last_login_date,''),created_at) FROM player_state`)
		if err == nil {
			for rows.Next() {
				var x row
				var raw string
				if err := rows.Scan(&x.PID, &x.Name, &x.Rank, &raw); err != nil {
					continue
				}
				at, ok := parseSeriesTime(raw)
				if !ok || at.After(cutoff) {
					continue
				}
				x.LastLogin = raw
				out = append(out, x)
			}
			_ = rows.Close()
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastLogin < out[j].LastLogin })
	if len(out) > 100 {
		out = out[:100]
	}
	writeAdminJSON(w, map[string]any{"sleepers": out, "count": len(out), "days": days})
}

// AdminAPIWealth handles GET /admin/api/wealth — soft-currency brackets:
// how many pilots sit in each wealth band. For economy tuning.
func adminAPIWealth(w http.ResponseWriter, r *http.Request) {
	database := currentMmogPlayerStateDB()
	bands := []struct {
		label string
		max   int64
	}{
		{"broke (<1k)", 1000},
		{"1k–10k", 10000},
		{"10k–50k", 50000},
		{"50k–100k", 100000},
		{"100k–500k", 500000},
		{"500k–1M", 1000000},
		{"whales (1M+)", 1 << 62},
	}
	counts := make([]int, len(bands))
	total := 0
	if database != nil {
		rows, err := database.Query(`SELECT soft_currency FROM player_state`)
		if err == nil {
			for rows.Next() {
				var v int64
				if err := rows.Scan(&v); err != nil {
					continue
				}
				total++
				for i, b := range bands {
					if v < b.max {
						counts[i]++
						break
					}
				}
			}
			_ = rows.Close()
		}
	}
	type band struct {
		Label string `json:"label"`
		Count int    `json:"count"`
	}
	out := make([]band, len(bands))
	for i, b := range bands {
		out[i] = band{Label: b.label, Count: counts[i]}
	}
	writeAdminJSON(w, map[string]any{"bands": out, "total": total})
}

// AdminAPIShips handles GET /admin/api/ships — most-flown hulls by total
// ship XP (the flown ledger), with pilot counts. Names resolve server-side
// from the roster; unknown ids show the raw id.
func adminAPIShips(w http.ResponseWriter, r *http.Request) {
	database := currentMmogPlayerStateDB()
	names := map[int32]string{}
	for _, hull := range baseShipLoadouts {
		names[hull.loadoutID] = hull.name
	}
	type ship struct {
		ID     int32  `json:"id"`
		Name   string `json:"name"`
		XP     int64  `json:"xp"`
		Pilots int    `json:"pilots"`
	}
	byID := map[int32]*ship{}
	if database != nil {
		rows, err := database.Query(`SELECT ship_id,COALESCE(SUM(xp),0),COUNT(DISTINCT user_id)
			FROM player_ship_xp GROUP BY ship_id`)
		if err == nil {
			for rows.Next() {
				var id int32
				var xp int64
				var pilots int
				if err := rows.Scan(&id, &xp, &pilots); err != nil {
					continue
				}
				name, ok := names[id]
				if !ok {
					name = fmt.Sprintf("ship %d", id)
				}
				byID[id] = &ship{ID: id, Name: name, XP: xp, Pilots: pilots}
			}
			_ = rows.Close()
		}
	}
	out := []ship{}
	for _, s := range byID {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].XP > out[j].XP })
	if len(out) > 20 {
		out = out[:20]
	}
	writeAdminJSON(w, map[string]any{"ships": out, "count": len(out)})
}

// AdminAPIModeStats handles GET /admin/api/mode-stats — matches, wins and
// kills per game mode (matches joined to their reported results). For
// balance reads: which modes are played, which are stomps.
func adminAPIModeStats(w http.ResponseWriter, r *http.Request) {
	database := currentMmogPlayerStateDB()
	type mode struct {
		Mode    string `json:"mode"`
		Matches int    `json:"matches"`
		Results int    `json:"results"`
		Wins    int    `json:"wins"`
		Kills   int    `json:"kills"`
	}
	byMode := map[string]*mode{}
	if database != nil {
		rows, err := database.Query(`SELECT m.game_mode,
			COUNT(DISTINCT m.id),
			COUNT(b.match_id),
			COALESCE(SUM(CASE WHEN b.outcome='win' THEN 1 ELSE 0 END),0),
			COALESCE(SUM(b.kills),0)
			FROM matches m LEFT JOIN battle_results b
			ON b.match_id=m.battle_match_id OR (m.battle_match_id!='' AND b.match_id LIKE m.battle_match_id||'-r%')
			GROUP BY m.game_mode`)
		if err == nil {
			for rows.Next() {
				var x mode
				if err := rows.Scan(&x.Mode, &x.Matches, &x.Results, &x.Wins, &x.Kills); err != nil {
					continue
				}
				if x.Mode == "" {
					x.Mode = "(unset)"
				}
				byMode[x.Mode] = &x
			}
			_ = rows.Close()
		}
	}
	out := []mode{}
	for _, m := range byMode {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Matches > out[j].Matches })
	writeAdminJSON(w, map[string]any{"modes": out, "count": len(out)})
}

// --- logs ---------------------------------------------------------------------

type battleLogFile struct {
	path string
	name string
	mod  time.Time
	size int64
}

func newestBattleLogs(n int) []battleLogFile {
	entries, err := os.ReadDir(filepath.Join("run", "battle-logs"))
	if err != nil {
		return nil
	}
	var files []battleLogFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, battleLogFile{path: filepath.Join("run", "battle-logs", e.Name()), name: e.Name(), mod: info.ModTime(), size: info.Size()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > n {
		files = files[:n]
	}
	return files
}

func adminAPIBattleLogs(w http.ResponseWriter, _ *http.Request) {
	type row struct {
		Name     string `json:"name"`
		Modified string `json:"modified"`
		Size     int64  `json:"size"`
		Crash    string `json:"crash,omitempty"`
		Mode     string `json:"mode,omitempty"`
	}
	out := []row{}
	for _, f := range newestBattleLogs(60) {
		tail := tailFile(f.path, 96*1024)
		r := row{Name: f.name, Modified: f.mod.Format("2006-01-02 15:04:05"), Size: f.size}
		switch {
		case strings.Contains(tail, "EXCEPTION_STACK_OVERFLOW"):
			r.Crash = "stack overflow"
		case strings.Contains(tail, "Unhandled Exception"), strings.Contains(tail, "Unhandled page fault"):
			r.Crash = "access violation"
		}
		if head := headFile(f.path, 32*1024); head != "" {
			if i := strings.Index(head, "?game="); i >= 0 {
				mode := head[i+6:]
				if j := strings.IndexAny(mode, "?& \"\n"); j > 0 {
					mode = mode[:j]
				}
				r.Mode = mode
			}
		}
		out = append(out, r)
	}
	writeAdminJSON(w, map[string]any{"logs": out})
}

// adminLogPath maps a log source name to a file. Only these files can be
// read: nothing from the request becomes a path except a battle-log NAME,
// which must be a plain file name inside run/battle-logs.
func adminLogPath(src, battle string) (string, bool) {
	gameDir := filepath.Dir(os.Getenv("GAME_BINARY"))
	switch src {
	case "mmogbrain":
		return filepath.Join("run", "mmogbrain.log"), true
	case "firmament":
		return "mmogbrain.log", true
	case "dn-dedicated":
		return filepath.Join("run", "dn-dedicated.log"), true
	case "gateway", "auth-server", "legacy-api", "master-server":
		return filepath.Join("run", src+".log"), true
	case "mod":
		if gameDir == "." {
			return "", false
		}
		return filepath.Join(gameDir, "dn_host_loadout.log"), true
	case "crashprobe":
		if gameDir == "." {
			return "", false
		}
		return filepath.Join(gameDir, "dn_host_stackprobe.log"), true
	case "battle":
		if battle == "" || battle != filepath.Base(battle) || !strings.HasPrefix(battle, "battle-") || !strings.HasSuffix(battle, ".log") {
			return "", false
		}
		return filepath.Join("run", "battle-logs", battle), true
	}
	return "", false
}

func adminAPILogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	path, ok := adminLogPath(q.Get("src"), q.Get("name"))
	if !ok {
		http.Error(w, "unknown log", http.StatusBadRequest)
		return
	}
	lines, _ := strconv.Atoi(q.Get("lines"))
	if lines <= 0 || lines > 2000 {
		lines = 300
	}
	filter := strings.ToLower(strings.TrimSpace(q.Get("q")))
	hideNoise := q.Get("noise") != "1"
	text := tailFile(path, 2*1024*1024)
	var picked []string
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if hideNoise && (strings.Contains(line, "NtQueryInformationProcess") || strings.Contains(line, "dispatch_exception L\"") ||
			strings.Contains(line, "OutputDebugStringW") || strings.Contains(line, "raw bytes received") || strings.Contains(line, "decrypted application bytes")) {
			continue
		}
		if filter != "" && !strings.Contains(strings.ToLower(line), filter) {
			continue
		}
		if len(line) > 600 {
			line = line[:600] + " …"
		}
		picked = append(picked, line)
	}
	if len(picked) > lines {
		picked = picked[len(picked)-lines:]
	}
	writeAdminJSON(w, map[string]any{"file": filepath.Base(path), "lines": picked})
}

func tailFile(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - max
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, max))
	s := string(b)
	if start > 0 {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return s
}

func headFile(path string, max int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	b, _ := io.ReadAll(io.LimitReader(f, max))
	return string(b)
}
