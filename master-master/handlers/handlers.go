package handlers

import (
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

const fieldStatus = "status"

type Handler struct {
	DB  *sql.DB
	Log *logrus.Logger
}

// staleAfter is how long a cluster may go without a heartbeat before it is
// marked offline and disappears from the browser. Clusters heartbeat every
// 30 seconds; four missed beats means gone, not lagging.
const staleAfter = 120 * time.Second

// StartCleanup runs a background goroutine that marks stale clusters offline
// (recording the transition for the flap timeline) and prunes heartbeat
// events older than 30 days.
func (h *Handler) StartCleanup() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			var ids []string
			if rows, err := h.DB.Query(`SELECT id FROM clusters WHERE status='online'
				AND datetime(last_heartbeat,'+120 seconds') < datetime('now')`); err == nil {
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err == nil {
						ids = append(ids, id)
					}
				}
				_ = rows.Close()
			} else {
				h.Log.WithError(err).Warn("stale cluster lookup")
			}
			for _, id := range ids {
				if _, err := h.DB.Exec(`UPDATE clusters SET status='offline' WHERE id=?`, id); err != nil {
					h.Log.WithError(err).Warn("stale cluster cleanup")
					continue
				}
				_, _ = h.DB.Exec(`INSERT INTO heartbeat_events(cluster_id,event) VALUES(?,'offline')`, id)
			}
			if _, err := h.DB.Exec(`DELETE FROM heartbeat_events WHERE datetime(time,'+30 days') < datetime('now')`); err != nil {
				h.Log.WithError(err).Warn("heartbeat events prune")
			}
			// Lapsed MOTDs clear themselves.
			if _, err := h.DB.Exec(`UPDATE clusters SET motd='',motd_until='' WHERE motd_until!=''
				AND datetime(motd_until)<=datetime('now')`); err != nil {
				h.Log.WithError(err).Warn("motd expiry cleanup")
			}
		}
	}()
}

// heartbeatEvent records a transition for the flap timeline.
func (h *Handler) heartbeatEvent(clusterID, event string) {
	_, _ = h.DB.Exec(`INSERT INTO heartbeat_events(cluster_id,event) VALUES(?,?)`, clusterID, event)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

var clusterNamePattern = regexp.MustCompile(`^[A-Za-z0-9 _.\-]{1,64}$`)

// validBattleAddress accepts what a player must dial for battle traffic: an
// IP literal or a DNS name. It must be reachable and, for the web URL, covered
// by the cluster's certificates — but that is the operator's job to get right
// (gen-certs.sh takes SERVER_IP / PUBLIC_HOST for exactly this), not something
// this directory can verify without joining every cluster itself.
func validBattleAddress(addr string) bool {
	if len(addr) < 1 || len(addr) > 253 {
		return false
	}
	for _, c := range addr {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// caFingerprint parses the cluster's CA certificate and returns the hex
// SHA-256 of its DER bytes — the TOFU anchor the browser shows the player
// before first join. Computed server-side so every browser sees the same
// fingerprint for the same certificate.
func caFingerprint(pemData string) (string, error) {
	// Empty is allowed: clusters with publicly trusted certificates have no
	// CA to upload, and their browsers verify through system roots. An empty
	// fingerprint simply means "no TOFU anchor".
	if strings.TrimSpace(pemData) == "" {
		return "", nil
	}
	if len(pemData) > 16384 {
		return "", errors.New("not a CA certificate")
	}
	rest := []byte(pemData)
	found := false
	var first []byte
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", errors.New("not a CA certificate")
		}
		// The uploaded file must be the cluster's CA, not its server
		// certificate: browsers install exactly this into the player's
		// trust store, and a leaf cert there is both wrong and confusing.
		// (A server.crt upload fails here with a clear message instead of
		// dying later in the browser with "certificate data is invalid".)
		if !cert.IsCA {
			return "", errors.New("not a CA certificate (did you upload server.crt instead of ca.crt?)")
		}
		if !found {
			first = block.Bytes
			found = true
		}
	}
	if !found {
		return "", errors.New("not a PEM certificate")
	}
	sum := sha256.Sum256(first)
	return hex.EncodeToString(sum[:]), nil
}

type Cluster struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	WebURL        string `json:"web_url"`
	BattleIP      string `json:"battle_ip"`
	Version       string `json:"version"`
	MOTD          string `json:"motd"`
	CACert        string `json:"ca_cert"`
	CAFingerprint string `json:"ca_fingerprint"`
	Players       int    `json:"players"`
	Servers       int    `json:"servers"`
	Status        string `json:"status"`
	LastHeartbeat string `json:"last_heartbeat"`
	RegisteredAt  string `json:"registered_at"`
}

// Register handles POST /clusters/register — upsert by name, so a cluster
// that restarts keeps its identity (and players keep their saved servers)
// instead of littering the directory with a new row per boot.
// expireBlocks lifts elapsed temporary blocks: a cluster whose blocked_until
// passed may list again without operator action. Runs on the write paths
// (register, heartbeat); the public list uses a predicate instead so reads
// never write.
func (h *Handler) expireBlocks() {
	_, err := h.DB.Exec(`UPDATE clusters SET blocked=0,blocked_until='',blocked_reason=''
		WHERE blocked=1 AND blocked_until!='' AND datetime(blocked_until)<=datetime('now')`)
	if err != nil {
		h.Log.WithError(err).Warn("expire blocks")
	}
}

// blockActive reports whether a block row still holds (indefinite or future).
func blockActive(blocked int, until string) bool {
	if blocked == 0 {
		return false
	}
	if strings.TrimSpace(until) == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return true
	}
	return time.Now().UTC().Before(t)
}

func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		WebURL       string `json:"web_url"`
		BattleIP     string `json:"battle_ip"`
		Version      string `json:"version"`
		MOTD         string `json:"motd"`
		CACert       string `json:"ca_cert"`
		ContactEmail string `json:"contact_email"`
		AgentURL     string `json:"agent_url"`
		Players      int    `json:"players"`
		Servers      int    `json:"servers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.WebURL = strings.TrimSpace(strings.TrimSuffix(req.WebURL, "/"))
	req.BattleIP = strings.TrimSpace(req.BattleIP)
	if !clusterNamePattern.MatchString(req.Name) {
		writeError(w, http.StatusBadRequest, "name must be 1-64 chars of letters, digits, space, _ . -")
		return
	}
	parsed, err := url.Parse(req.WebURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, "web_url must be an http(s) URL with a host")
		return
	}
	if !validBattleAddress(req.BattleIP) {
		writeError(w, http.StatusBadRequest, "battle_ip must be an IP or DNS name")
		return
	}
	if len(req.Version) > 32 {
		writeError(w, http.StatusBadRequest, "version too long (max 32 chars)")
		return
	}
	if len([]rune(req.MOTD)) > 500 {
		writeError(w, http.StatusBadRequest, "motd too long (max 500 chars)")
		return
	}
	// Contact email is required: it is the only channel over which the
	// operator hands out the account-sync secret (by hand, by mail).
	req.ContactEmail = strings.TrimSpace(req.ContactEmail)
	if !emailPattern.MatchString(req.ContactEmail) {
		writeError(w, http.StatusBadRequest, "contact_email must be a reachable mail address")
		return
	}
	// The agent URL carries a fresh secret on operator request, so it must
	// be https — checked again at send time, but refused here already.
	req.AgentURL = strings.TrimSpace(req.AgentURL)
	if req.AgentURL != "" {
		parsedAgent, err := url.Parse(req.AgentURL)
		if err != nil || parsedAgent.Scheme != "https" || parsedAgent.Host == "" {
			writeError(w, http.StatusBadRequest, "agent_url must be an https URL")
			return
		}
	}
	fingerprint, err := caFingerprint(req.CACert)
	if err != nil {
		// The error texts are static, operator-facing hints (no internals),
		// so they go out verbatim instead of a generic message.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Players < 0 {
		req.Players = 0
	}
	if req.Servers < 0 {
		req.Servers = 0
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var id string
	var blocked int
	var blockedUntil, blockedReason, prevStatus string
	h.expireBlocks()
	err = h.DB.QueryRow(`SELECT id,blocked,blocked_until,blocked_reason,COALESCE(status,'') FROM clusters WHERE name=?`, req.Name).Scan(&id, &blocked, &blockedUntil, &blockedReason, &prevStatus)
	if err != nil && err != sql.ErrNoRows {
		h.Log.WithError(err).Error("register cluster: lookup")
		writeError(w, http.StatusInternalServerError, "registration failed")
		return
	}
	if err == nil && blockActive(blocked, blockedUntil) {
		msg := "this cluster is blocked from the directory"
		if strings.TrimSpace(blockedReason) != "" {
			msg += ": " + blockedReason
		}
		writeError(w, http.StatusForbidden, msg)
		return
	}
	if err == sql.ErrNoRows {
		id = uuid.New().String()
		if _, err := h.DB.Exec(`INSERT INTO clusters(id,name,web_url,battle_ip,version,motd,ca_cert,
			ca_fingerprint,contact_email,agent_url,players,servers,last_heartbeat,registered_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, req.Name, req.WebURL, req.BattleIP, req.Version, req.MOTD,
			req.CACert, fingerprint, req.ContactEmail, req.AgentURL,
			req.Players, req.Servers, now, now); err != nil {
			h.Log.WithError(err).Error("register cluster: db insert")
			writeError(w, http.StatusInternalServerError, "registration failed")
			return
		}
		h.heartbeatEvent(id, "online")
	} else if _, err := h.DB.Exec(`UPDATE clusters SET web_url=?,battle_ip=?,version=?,motd=?,
		ca_cert=?,ca_fingerprint=?,contact_email=?,agent_url=?,players=?,servers=?,
		status='online',last_heartbeat=? WHERE id=?`, req.WebURL, req.BattleIP, req.Version, req.MOTD,
		req.CACert, fingerprint, req.ContactEmail, req.AgentURL,
		req.Players, req.Servers, now, id); err != nil {
		h.Log.WithError(err).Error("register cluster: db update")
		writeError(w, http.StatusInternalServerError, "registration failed")
		return
	}
	// Fresh inserts already recorded their first online event above; only a
	// returning cluster (update path, previously not online) records again.
	if err != sql.ErrNoRows && prevStatus != "online" {
		h.heartbeatEvent(id, "online")
	}
	h.Log.WithFields(logrus.Fields{"cluster_id": id, "name": req.Name}).Info("cluster registered")
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, fieldStatus: "registered"})
}

// Deregister handles DELETE /clusters/{id} — graceful goodbye (opt-out file,
// shutdown). Stale rows age out on their own, so a lost cluster vanishes
// without this; this is for the clean case.
func (h *Handler) Deregister(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	res, err := h.DB.Exec(`DELETE FROM clusters WHERE id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "deregistration failed")
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		h.Log.WithError(err).Warn("deregister rows affected")
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	h.Log.WithField("cluster_id", id).Info("cluster deregistered")
	writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "deregistered"})
}

// Heartbeat handles POST /clusters/{id}/heartbeat — player/server counts.
// Anything else about the cluster (addresses, CA) changes via re-register,
// which is an upsert by name and keeps the same id.
func (h *Handler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var req struct {
		Players int `json:"players"`
		Servers int `json:"servers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Players < 0 {
		req.Players = 0
	}
	if req.Servers < 0 {
		req.Servers = 0
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h.expireBlocks()
	var prevStatus string
	_ = h.DB.QueryRow(`SELECT status FROM clusters WHERE id=?`, id).Scan(&prevStatus)
	res, err := h.DB.Exec(`UPDATE clusters SET last_heartbeat=?,players=?,servers=?,status='online'
		WHERE id=? AND blocked=0`, now, req.Players, req.Servers, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	n, err := res.RowsAffected()
	if err != nil {
		h.Log.WithError(err).Warn("heartbeat rows affected")
	}
	if n == 0 {
		writeError(w, http.StatusNotFound, "cluster not found (re-register)")
		return
	}
	if prevStatus != "online" {
		h.heartbeatEvent(id, "online")
	}
	writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "ok"})
}

// motdLive blanks an expired MOTD for display (the sweeper clears it for
// real on its next pass).
func motdLive(motd, until string) string {
	if strings.TrimSpace(until) == "" {
		return motd
	}
	t, err := time.Parse(time.RFC3339, until)
	if err != nil || time.Now().UTC().Before(t) {
		return motd
	}
	return ""
}

// List handles GET /clusters — the public server browser: online clusters
// only, newest heartbeat first. No auth: this is public directory data
// (names, addresses, a CA certificate), never secrets. Actively blocked
// clusters are excluded directly (not just via going stale); elapsed
// temporary blocks read as unblocked through the predicate.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.Query(`SELECT id,name,web_url,battle_ip,version,motd,motd_until,ca_cert,ca_fingerprint,
		players,servers,status,last_heartbeat,registered_at FROM clusters
		WHERE status='online' AND (blocked=0 OR (blocked_until!='' AND datetime(blocked_until)<=datetime('now')))
		ORDER BY last_heartbeat DESC`)
	if err != nil {
		h.Log.WithError(err).Error("list clusters: db query")
		writeError(w, http.StatusInternalServerError, "failed to fetch cluster list")
		return
	}
	defer func() {
		_ = rows.Close()
	}()
	clusters := []Cluster{}
	for rows.Next() {
		var c Cluster
		var motdUntil string
		if err := rows.Scan(&c.ID, &c.Name, &c.WebURL, &c.BattleIP, &c.Version, &c.MOTD, &motdUntil,
			&c.CACert, &c.CAFingerprint, &c.Players, &c.Servers, &c.Status,
			&c.LastHeartbeat, &c.RegisteredAt); err != nil {
			h.Log.WithError(err).Error("list clusters: scan cluster")
			writeError(w, http.StatusInternalServerError, "failed to fetch cluster list")
			return
		}
		c.MOTD = motdLive(c.MOTD, motdUntil)
		clusters = append(clusters, c)
	}
	if err := rows.Err(); err != nil {
		h.Log.WithError(err).Error("list clusters: iterate rows")
		writeError(w, http.StatusInternalServerError, "failed to fetch cluster list")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": clusters, "count": len(clusters)})
}

// Health handles GET /health
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	var count int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM clusters WHERE status='online'`).Scan(&count); err != nil {
		h.Log.WithError(err).Warn("health: count online clusters")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		fieldStatus:       "ok",
		"service":         "master-master",
		"clusters_online": count,
	})
}

// AdminListAll handles GET /admin/api/clusters — every cluster including
// offline and blocked ones, newest heartbeat first. Operator eyes only.
// mirrored counts accounts the mirror holds under this cluster's NAME
// (source_cluster is the pushing cluster's name).
func (h *Handler) AdminListAll(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.Query(`SELECT id,name,web_url,battle_ip,version,motd,motd_until,note,
		players,servers,status,blocked,blocked_until,blocked_reason,contact_email,agent_url,secret_hash!='',secret_set_at,
		COALESCE((SELECT MAX(time) FROM sync_log WHERE cluster_id=clusters.id),''),
		COALESCE((SELECT COUNT(*) FROM sync_users WHERE source_cluster=clusters.name),0),
		last_heartbeat,registered_at FROM clusters
		ORDER BY last_heartbeat DESC`)
	if err != nil {
		h.Log.WithError(err).Error("admin list clusters: db query")
		writeError(w, http.StatusInternalServerError, "failed to fetch cluster list")
		return
	}
	defer func() {
		_ = rows.Close()
	}()
	type cluster struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		WebURL        string `json:"web_url"`
		BattleIP      string `json:"battle_ip"`
		Version       string `json:"version"`
		MOTD          string `json:"motd"`
		MOTDUntil     string `json:"motd_until"`
		Note          string `json:"note"`
		Players       int    `json:"players"`
		Servers       int    `json:"servers"`
		Status        string `json:"status"`
		Blocked       bool   `json:"blocked"`
		BlockedUntil  string `json:"blocked_until"`
		BlockedReason string `json:"blocked_reason"`
		Email         string `json:"contact_email"`
		AgentURL      string `json:"agent_url"`
		HasSecret     bool   `json:"has_secret"`
		SecretSetAt   string `json:"secret_set_at"`
		LastSync      string `json:"last_sync"`
		MirroredUsers int    `json:"mirrored_users"`
		LastHeartbeat string `json:"last_heartbeat"`
		RegisteredAt  string `json:"registered_at"`
	}
	out := []cluster{}
	for rows.Next() {
		var c cluster
		var blocked, hasSecret int
		if err := rows.Scan(&c.ID, &c.Name, &c.WebURL, &c.BattleIP, &c.Version, &c.MOTD, &c.MOTDUntil, &c.Note,
			&c.Players, &c.Servers, &c.Status, &blocked, &c.BlockedUntil, &c.BlockedReason,
			&c.Email, &c.AgentURL, &hasSecret, &c.SecretSetAt, &c.LastSync, &c.MirroredUsers,
			&c.LastHeartbeat, &c.RegisteredAt); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to fetch cluster list")
			return
		}
		c.Blocked = blocked != 0
		c.HasSecret = hasSecret != 0
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to fetch cluster list")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clusters": out, "count": len(out)})
}

// AdminSetMOTD handles POST /admin/api/clusters/{id}/motd — change a
// cluster's message of the day without touching the cluster. Optional
// {"minutes":N} expires it automatically (0/empty = permanent).
func (h *Handler) AdminSetMOTD(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var req struct {
		MOTD    string `json:"motd"`
		Minutes int    `json:"minutes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len([]rune(req.MOTD)) > 500 {
		writeError(w, http.StatusBadRequest, "motd too long (max 500 chars)")
		return
	}
	if req.Minutes < 0 || req.Minutes > 525600 {
		writeError(w, http.StatusBadRequest, "minutes must be 0 (permanent) to 525600")
		return
	}
	until := ""
	if req.Minutes > 0 {
		until = time.Now().UTC().Add(time.Duration(req.Minutes) * time.Minute).Format(time.RFC3339)
	}
	res, err := h.DB.Exec(`UPDATE clusters SET motd=?,motd_until=? WHERE id=?`, req.MOTD, until, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	h.Log.WithField("cluster_id", id).Info("operator changed a cluster MOTD")
	writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "ok", "until": until})
}

// AdminMotdAll handles POST /admin/api/motd-all — one message of the day
// for every cluster at once (event notices, maintenance windows). Same
// 500-rune limit as the single-cluster endpoint; optional {"minutes":N}
// expires it everywhere at once.
func (h *Handler) AdminMotdAll(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MOTD    string `json:"motd"`
		Minutes int    `json:"minutes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len([]rune(req.MOTD)) > 500 {
		writeError(w, http.StatusBadRequest, "motd too long (max 500 chars)")
		return
	}
	if req.Minutes < 0 || req.Minutes > 525600 {
		writeError(w, http.StatusBadRequest, "minutes must be 0 (permanent) to 525600")
		return
	}
	until := ""
	if req.Minutes > 0 {
		until = time.Now().UTC().Add(time.Duration(req.Minutes) * time.Minute).Format(time.RFC3339)
	}
	res, err := h.DB.Exec(`UPDATE clusters SET motd=?,motd_until=?`, req.MOTD, until)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	n, _ := res.RowsAffected()
	h.syncLog("", "admin", "motd-all", 0, "ok", req.MOTD)
	h.Log.WithField("clusters", n).Warn("operator broadcast a MOTD to all clusters")
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "updated": n, "until": until})
}

// AdminBlock handles POST /admin/api/clusters/{id}/block — kick a cluster
// out of the browser AND refuse its re-registration (the row stays as the
// block record). Optional body {"reason":"…","minutes":60}: with minutes the
// block lapses on its own (reason shown to the next refusal and in the
// dashboard); without it the block is indefinite, the old behaviour.
// Unblock (or expiry) lets it back with the next heartbeat.
func (h *Handler) AdminBlock(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var req struct {
		Reason  string `json:"reason"`
		Minutes int    `json:"minutes"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	req.Reason = strings.TrimSpace(req.Reason)
	if len([]rune(req.Reason)) > 200 {
		writeError(w, http.StatusBadRequest, "reason too long (max 200 chars)")
		return
	}
	if req.Minutes < 0 || req.Minutes > 525600 {
		writeError(w, http.StatusBadRequest, "minutes must be 0 (indefinite) to 525600")
		return
	}
	until := ""
	if req.Minutes > 0 {
		until = time.Now().UTC().Add(time.Duration(req.Minutes) * time.Minute).Format(time.RFC3339)
	}
	res, err := h.DB.Exec(`UPDATE clusters SET blocked=1,status='offline',blocked_until=?,blocked_reason=? WHERE id=?`,
		until, req.Reason, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	h.Log.WithFields(logrus.Fields{"cluster_id": id, "reason": req.Reason, "until": until}).Warn("operator blocked a cluster")
	writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "blocked", "until": until})
}

// AdminUnblock handles POST /admin/api/clusters/{id}/unblock — clear a block
// (including reason and expiry); the cluster reappears with its next
// heartbeat.
func (h *Handler) AdminUnblock(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	res, err := h.DB.Exec(`UPDATE clusters SET blocked=0,blocked_until='',blocked_reason='' WHERE id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	h.Log.WithField("cluster_id", id).Info("operator unblocked a cluster")
	writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "unblocked"})
}

// AdminSetNote handles POST /admin/api/clusters/{id}/note — free operator
// note per cluster (owner contact, maintenance window, quirks). Max 500
// runes, like MOTD. Empty clears it.
func (h *Handler) AdminSetNote(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var req struct {
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len([]rune(req.Note)) > 500 {
		writeError(w, http.StatusBadRequest, "note too long (max 500 chars)")
		return
	}
	res, err := h.DB.Exec(`UPDATE clusters SET note=? WHERE id=?`, strings.TrimSpace(req.Note), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "ok"})
}

// AdminDelete handles DELETE /admin/api/clusters/{id} — remove the row
// entirely (same as the public deregister route, from the dashboard).
func (h *Handler) AdminDelete(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	res, err := h.DB.Exec(`DELETE FROM clusters WHERE id=?`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "cluster not found")
		return
	}
	h.Log.WithField("cluster_id", id).Warn("operator deleted a cluster")
	writeJSON(w, http.StatusOK, map[string]string{fieldStatus: "deleted"})
}
