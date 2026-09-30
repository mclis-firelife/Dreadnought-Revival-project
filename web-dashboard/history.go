package main

// Gauge history for the dashboard graphs. mmogbrain's tables carry
// created_at timestamps (matches, results, accounts, reports), so counters
// graph "since the database began" straight from the source. Live gauges
// (online, queued, instances, servers, matches) have no history table
// anywhere — the dashboard records its own: one JSON line per minute into
// run/web-dashboard-samples.jsonl, capped at ~370 days (oldest 10% dropped
// on overflow). No new dependency (plain JSONL, scanned per request; the
// file stays small enough that one pass per /api/series call is cheap).
//
// /api/series?metrics=online,queued,matches,kills&range=24h answers every
// requested series at once: gauges from the local file, counters proxied to
// mmogbrain's /admin/api/series with the same window. Ranges: 2m 1h 24h 7d
// 14d 30d 1y all. recorded_since tells the UI how far back the gauge lines
// honestly reach (fresh installs: "an hour", not "a year").

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/mux"
)

var gaugeMetrics = map[string]bool{
	"online": true, "queued": true, "instances": true, "servers": true, "matches": true,
}

var counterMetrics = map[string]bool{
	"matches_total": true, "results": true, "kills": true,
	"credits": true, "spending": true, "accounts": true, "reports": true,
}

// counterUpstream maps a dashboard counter metric to mmogbrain's series metric.
var counterUpstream = map[string]string{
	"matches_total": "matches",
	"results":       "results",
	"kills":         "kills",
	"credits":       "credits",
	"spending":      "spending",
	"accounts":      "accounts",
	"reports":       "reports",
}

var rangeDurations = map[string]time.Duration{
	"2m":  2 * time.Minute,
	"1h":  time.Hour,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"14d": 14 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"1y":  365 * 24 * time.Hour,
}

const (
	sampleInterval = time.Minute
	// sampleMaxLines caps the file (~370 days of minutes); oldest tenth
	// goes on overflow. A var (not const) so tests can shrink it.
	samplePruneFrac = 10 // drop oldest 10% on overflow
	seriesBuckets   = 120
)

// sampleMaxLines caps the sample file.
var sampleMaxLines = 532800

type gaugeSample struct {
	T         int64          `json:"t"`
	Online    int            `json:"online"`
	Queued    int            `json:"queued"`
	Instances int            `json:"instances"`
	Servers   int            `json:"servers"`
	Matches   int            `json:"matches"`
	Services  map[string]bool `json:"services,omitempty"`
}

func (s *server) samplePath() string {
	if v := os.Getenv("DASH_HIST_FILE"); v != "" {
		return v
	}
	return filepath.Join(s.cfg.runDir, "web-dashboard-samples.jsonl")
}

// gaugeSnapshot reads the same five live numbers apiStatus reports, for the
// sampler. Failures stay zero: a down service must not stop the recording.
// Per-service up/down rides along for the SLA panel.
func (s *server) gaugeSnapshot() gaugeSample {
	snap := gaugeSample{T: time.Now().UTC().Unix(), Services: map[string]bool{}}
	for _, t := range s.serviceTargets() {
		code, _ := s.upstreamGet(t.url, "/health", "", nil)
		snap.Services[t.name] = code == http.StatusOK
	}
	if code, doc := s.upstreamGet(s.cfg.mmogURL, "/health", "", nil); code == http.StatusOK {
		snap.Queued = int(jsonNumber(doc["queued_players"]))
		snap.Matches = int(jsonNumber(doc["active_matches"]))
	}
	if code, doc := s.upstreamGet(s.cfg.gameMgrURL, "/instances", "", nil); code == http.StatusOK {
		snap.Instances = int(jsonNumber(doc["count"]))
	}
	if code, doc := s.upstreamGet(s.cfg.masterURL, "/servers", "", nil); code == http.StatusOK {
		snap.Servers = int(jsonNumber(doc["count"]))
	}
	if code, doc := s.upstreamGet(s.cfg.mmogURL, "/admin/online", s.cfg.adminKey, nil); code == http.StatusOK {
		snap.Online = int(jsonNumber(doc["count"]))
	}
	return snap
}

func (s *server) recordSample(snap gaugeSample) {
	raw, err := json.Marshal(snap)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.samplePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		s.log.WithError(err).Warn("dashboard history: cannot open sample file")
		return
	}
	_, _ = f.Write(append(raw, '\n'))
	_ = f.Close()
	s.pruneSamples()
}

// pruneSamples caps the file: over the limit, the oldest tenth goes.
func (s *server) pruneSamples() {
	raw, err := os.ReadFile(s.samplePath())
	if err != nil {
		return
	}
	lines := 0
	for _, c := range raw {
		if c == '\n' {
			lines++
		}
	}
	if lines <= sampleMaxLines {
		return
	}
	drop := lines / samplePruneFrac
	idx, seen := 0, 0
	for idx < len(raw) && seen < drop {
		if raw[idx] == '\n' {
			seen++
		}
		idx++
	}
	if err := os.WriteFile(s.samplePath(), raw[idx:], 0o600); err != nil {
		s.log.WithError(err).Warn("dashboard history: cannot prune sample file")
	}
}

// startSampler records one gauge line per minute until ctx... runs forever;
// it stops with the process. The first sample lands a minute after start;
// until then gauge graphs show "collecting".
func (s *server) startSampler() {
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for range ticker.C {
			s.recordSample(s.gaugeSnapshot())
		}
	}()
}

func gaugeValue(snap gaugeSample, metric string) float64 {
	switch metric {
	case "online":
		return float64(snap.Online)
	case "queued":
		return float64(snap.Queued)
	case "instances":
		return float64(snap.Instances)
	case "servers":
		return float64(snap.Servers)
	case "matches":
		return float64(snap.Matches)
	}
	return 0
}

// apiSeries serves every requested graph series in one pass. Gauges come
// from the local sample file, counters are proxied to mmogbrain with the
// same window. Unknown metrics are 400; a missing sample file or a down
// mmogbrain yields empty series, never 500 — one dead source must not kill
// every graph.
func (s *server) apiSeries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metrics := []string{}
	for _, m := range strings.Split(q.Get("metrics"), ",") {
		if m = strings.TrimSpace(m); m != "" {
			metrics = append(metrics, m)
		}
	}
	if len(metrics) == 0 {
		metrics = []string{"online", "queued", "matches"}
	}
	for _, m := range metrics {
		if !gaugeMetrics[m] && !counterMetrics[m] {
			writeError(w, http.StatusBadRequest, "unknown metric "+m)
			return
		}
	}
	rng := strings.TrimSpace(q.Get("range"))
	if rng == "" {
		rng = "all"
	}
	var from time.Time
	now := time.Now().UTC()
	if d, ok := rangeDurations[rng]; ok {
		from = now.Add(-d)
	} else if rng != "all" {
		writeError(w, http.StatusBadRequest, "unknown range (2m 1h 24h 7d 14d 30d 1y all)")
		return
	}

	series := make(map[string]any, len(metrics))
	recordedSince := ""
	gauges := map[string][][2]any{}
	if s.anyGauge(metrics) {
		gauges, recordedSince = s.readGaugeSeries(from, now)
	}
	for _, m := range metrics {
		if gaugeMetrics[m] {
			pts := gauges[m]
			if pts == nil {
				pts = [][2]any{}
			}
			series[m] = map[string]any{"points": pts, "total": lastValue(pts)}
			continue
		}
		series[m] = s.proxyCounterSeries(m, from, now)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"series": series, "range": rng,
		"from": timeOrEmpty(from), "to": now.Format(time.RFC3339),
		"recorded_since": recordedSince,
	})
}

func (s *server) anyGauge(metrics []string) bool {
	for _, m := range metrics {
		if gaugeMetrics[m] {
			return true
		}
	}
	return false
}

func lastValue(pts [][2]any) float64 {
	if len(pts) == 0 {
		return 0
	}
	if v, ok := pts[len(pts)-1][1].(float64); ok {
		return v
	}
	return 0
}

func timeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// readGaugeSeries scans the sample file once and buckets every gauge.
func (s *server) readGaugeSeries(from, to time.Time) (map[string][][2]any, string) {
	out := map[string][][2]any{}
	f, err := os.Open(s.samplePath())
	if err != nil {
		return out, ""
	}
	defer func() { _ = f.Close() }()
	fromUnix := int64(0)
	if !from.IsZero() {
		fromUnix = from.Unix()
	}
	toUnix := to.Unix()
	var earliest int64
	read := 0
	sums := map[string][]float64{}
	counts := map[string][]int{}
	span := toUnix - fromUnix
	if span <= 0 {
		span = 1
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var snap gaugeSample
		if err := json.Unmarshal([]byte(line), &snap); err != nil || snap.T <= 0 {
			continue
		}
		if earliest == 0 || snap.T < earliest {
			earliest = snap.T
		}
		if snap.T < fromUnix || snap.T > toUnix {
			continue
		}
		read++
		i := int((snap.T - fromUnix) * seriesBuckets / span)
		if i < 0 {
			i = 0
		}
		if i >= seriesBuckets {
			i = seriesBuckets - 1
		}
		for m := range gaugeMetrics {
			if sums[m] == nil {
				sums[m] = make([]float64, seriesBuckets)
				counts[m] = make([]int, seriesBuckets)
			}
			sums[m][i] += gaugeValue(snap, m)
			counts[m][i]++
		}
	}
	if read == 0 {
		if earliest > 0 {
			return out, time.Unix(earliest, 0).UTC().Format(time.RFC3339)
		}
		return out, ""
	}
	// Only non-empty buckets are emitted (sparse ranges stay honest instead
	// of zero-filled); total is the newest known value, not a possibly
	// empty trailing bucket.
	for m := range gaugeMetrics {
		pts := [][2]any{}
		for i := 0; i < seriesBuckets; i++ {
			if counts[m] == nil || counts[m][i] == 0 {
				continue
			}
			pts = append(pts, [2]any{fromUnix + span*int64(i)/seriesBuckets,
				sums[m][i] / float64(counts[m][i])})
		}
		out[m] = pts
	}
	since := ""
	if earliest > 0 {
		since = time.Unix(earliest, 0).UTC().Format(time.RFC3339)
	}
	return out, since
}

// apiSLA serves per-service uptime percent over a range, from the sampler's
// service flags: GET /api/sla?range=24h → {range, services:{name:pct},
// samples:n, recorded_since}. Same ranges as /api/series.
func (s *server) apiSLA(w http.ResponseWriter, r *http.Request) {
	rng := strings.TrimSpace(r.URL.Query().Get("range"))
	if rng == "" {
		rng = "24h"
	}
	var from time.Time
	now := time.Now().UTC()
	if d, ok := rangeDurations[rng]; ok {
		from = now.Add(-d)
	} else if rng != "all" {
		writeError(w, http.StatusBadRequest, "unknown range (2m 1h 24h 7d 14d 30d 1y all)")
		return
	}
	fromUnix := int64(0)
	if !from.IsZero() {
		fromUnix = from.Unix()
	}
	up := map[string]int{}
	total := map[string]int{}
	samples := 0
	var earliest int64
	if f, err := os.Open(s.samplePath()); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var snap gaugeSample
			if err := json.Unmarshal([]byte(line), &snap); err != nil || snap.T <= 0 {
				continue
			}
			if earliest == 0 || snap.T < earliest {
				earliest = snap.T
			}
			if snap.T < fromUnix || snap.T > now.Unix() || len(snap.Services) == 0 {
				continue
			}
			samples++
			for name, isUp := range snap.Services {
				total[name]++
				if isUp {
					up[name]++
				}
			}
		}
		_ = f.Close()
	}
	pct := map[string]float64{}
	for _, t := range s.serviceTargets() {
		if total[t.name] > 0 {
			pct[t.name] = float64(up[t.name]) * 100 / float64(total[t.name])
		}
	}
	since := ""
	if earliest > 0 {
		since = time.Unix(earliest, 0).UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": rng, "services": pct, "samples": samples, "recorded_since": since,
	})
}

// ---------------------------------------------------------------- events
// Operator markers drawn onto the overview graphs (restarts, deploys,
// events): one JSON object per line in run/web-dashboard-events.jsonl.
// GET /api/events lists, POST {t?, label} adds (t defaults to now),
// DELETE /api/events/{id} removes. ids are millisecond timestamps + counter,
// unique enough for an operator tool.

type dashEvent struct {
	ID    string `json:"id"`
	T     int64  `json:"t"`
	Label string `json:"label"`
}

func (s *server) eventsPath() string {
	if v := os.Getenv("DASH_EVENTS_FILE"); v != "" {
		return v
	}
	return filepath.Join(s.cfg.runDir, "web-dashboard-events.jsonl")
}

func (s *server) readEvents() []dashEvent {
	out := []dashEvent{}
	raw, err := os.ReadFile(s.eventsPath())
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e dashEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil || e.ID == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (s *server) apiEvents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		events := s.readEvents()
		writeJSON(w, http.StatusOK, map[string]any{"events": events, "count": len(events)})
	case http.MethodPost:
		var req struct {
			T     int64  `json:"t"`
			Label string `json:"label"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid body")
			return
		}
		req.Label = strings.TrimSpace(req.Label)
		if req.Label == "" || len([]rune(req.Label)) > 120 {
			writeError(w, http.StatusBadRequest, "label required (max 120 chars)")
			return
		}
		if req.T <= 0 {
			req.T = time.Now().UTC().Unix()
		}
		e := dashEvent{
			ID:    fmt.Sprintf("%d-%d", req.T, time.Now().UTC().UnixNano()%1000000),
			T:     req.T,
			Label: req.Label,
		}
		raw, _ := json.Marshal(e)
		f, err := os.OpenFile(s.eventsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "cannot store event")
			return
		}
		_, _ = f.Write(append(raw, '\n'))
		_ = f.Close()
		s.audit("event-marker", req.Label)
		writeJSON(w, http.StatusOK, map[string]any{"event": e})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *server) apiDeleteEvent(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if strings.TrimSpace(id) == "" || strings.ContainsAny(id, "/\\. ") {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	kept := []dashEvent{}
	found := false
	for _, e := range s.readEvents() {
		if e.ID == id {
			found = true
			continue
		}
		kept = append(kept, e)
	}
	if !found {
		writeError(w, http.StatusNotFound, "event not found")
		return
	}
	var sb strings.Builder
	for _, e := range kept {
		raw, _ := json.Marshal(e)
		sb.Write(raw)
		sb.WriteByte('\n')
	}
	if err := os.WriteFile(s.eventsPath(), []byte(sb.String()), 0o600); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot store events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// proxyCounterSeries forwards one counter metric to mmogbrain's series
// endpoint with the same window.
func (s *server) proxyCounterSeries(metric string, from, to time.Time) map[string]any {
	query := map[string]string{"metric": counterUpstream[metric], "buckets": "120"}
	if !from.IsZero() {
		query["from"] = from.Format(time.RFC3339)
	}
	query["to"] = to.Format(time.RFC3339)
	code, doc := s.upstreamGet(s.cfg.mmogURL, "/admin/api/series", s.cfg.adminKey, query)
	if code != http.StatusOK {
		return map[string]any{"points": [][2]any{}, "total": 0}
	}
	pts := [][2]any{}
	if raw, ok := doc["points"].([]any); ok {
		for _, p := range raw {
			pair, ok := p.([]any)
			if !ok || len(pair) != 2 {
				continue
			}
			pts = append(pts, [2]any{pair[0], jsonNumber(pair[1])})
		}
	}
	return map[string]any{"points": pts, "total": jsonNumber(doc["total"])}
}
