package handlers

// Cross-cluster single sign-on ("roam").
//
// The directory master already mirrors every account (id, username, email,
// password hash), so it can vouch for identity: cluster A asks the master
// for a roaming ticket on behalf of its logged-in user (grant, authed by
// the user's JWT here and by the cluster sync secret there); the browser
// hands that ticket to cluster B, which redeems it for a normal local
// session (redeem). The player types their password once — on the first
// cluster — and never again.
//
// Trust chain, no new shared crypto: user→A by JWT, A→master by sync
// secret, master→opaque ticket (5 min, reusable), B→master by its own sync
// secret (introspection). Tickets are random, hashed at rest, pruned by the
// master's sweeper.
//
// Cluster side needs what the sync agent already has: SYNC_MASTER_URL plus
// the cluster sync secret (SYNC_SECRET env or SYNC_SECRET_FILE, default
// run/sync.env — same host, same file). Without either, both endpoints
// answer "roaming not configured" and password login stays the only way in.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/darkace1998/Dreadnought-Revival-project/auth-server/models"
	"github.com/sirupsen/logrus"
)

// roamMasterURL is the directory this cluster roams with (same meaning as
// the sync agent's SYNC_MASTER_URL).
func roamMasterURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("SYNC_MASTER_URL")), "/")
}

// roamClusterSecret is this cluster's sync secret, read exactly like the
// sync agent reads it (env first, then the shared file). Read-only use.
func roamClusterSecret() string {
	if v := strings.TrimSpace(os.Getenv("SYNC_SECRET")); v != "" {
		return v
	}
	path := strings.TrimSpace(os.Getenv("SYNC_SECRET_FILE"))
	if path == "" {
		path = "run/sync.env"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "SYNC_SECRET=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "SYNC_SECRET="))
		}
	}
	return ""
}

// roamMasterPost calls one master roam endpoint with the cluster secret.
func roamMasterPost(masterURL, endpoint, secret string, payload, out any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, masterURL+endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sync-Key", secret)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("directory answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// RoamGrant handles POST /auth/roam/grant — a logged-in user asks this
// cluster for a roaming ticket to take elsewhere. JWT-authed (user), the
// cluster authenticates to the master with its sync secret.
func (h *Handler) RoamGrant(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		writeGreyboxError(w, http.StatusUnauthorized, -32001, "not authenticated")
		return
	}
	master, secret := roamMasterURL(), roamClusterSecret()
	if master == "" || secret == "" {
		writeGreyboxError(w, http.StatusConflict, -32603, "roaming not configured on this cluster")
		return
	}
	var out struct {
		Ticket    string `json:"ticket"`
		ExpiresIn int    `json:"expires_in"`
		Username  string `json:"username"`
	}
	if err := roamMasterPost(master, "/roam/delegate", secret, map[string]string{"user_id": userID}, &out); err != nil {
		h.Log.WithError(err).Warn("roam grant: directory delegate failed")
		writeGreyboxError(w, http.StatusBadGateway, -32603, "directory unreachable or account not mirrored yet")
		return
	}
	if len(out.Ticket) < 16 {
		writeGreyboxError(w, http.StatusBadGateway, -32603, "directory gave no ticket")
		return
	}
	h.Log.WithField(fieldUserID, userID).Info("roam ticket granted")
	writeJSON(w, http.StatusOK, map[string]any{
		fieldStatus: "ok", "ticket": out.Ticket, "expires_in": out.ExpiresIn, "username": out.Username,
	})
}

// denormRoamID re-adds dashes to a mirrored (undashed) account id for the
// local users table, which keys by dashed UUID.
func denormRoamID(normalized string) string {
	if len(normalized) != 32 {
		return normalized
	}
	return normalized[0:8] + "-" + normalized[8:12] + "-" + normalized[12:16] + "-" +
		normalized[16:20] + "-" + normalized[20:32]
}

// RoamRedeem handles POST /auth/roam/redeem — trade a roaming ticket (got
// from another cluster) for a normal local session here. The account row is
// created as a stub when missing (unusable password hash — password login
// stays impossible until sync brings the real hash); a locally banned
// account is still refused. Answers exactly like a password login.
func (h *Handler) RoamRedeem(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req struct {
		Ticket string `json:"ticket"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Ticket) == "" {
		writeGreyboxError(w, http.StatusBadRequest, -32602, "ticket required")
		return
	}
	master, secret := roamMasterURL(), roamClusterSecret()
	if master == "" || secret == "" {
		writeGreyboxError(w, http.StatusConflict, -32603, "roaming not configured on this cluster")
		return
	}
	var idn struct {
		UserID   string `json:"user_id"`
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	if err := roamMasterPost(master, "/roam/verify", secret,
		map[string]string{"ticket": strings.TrimSpace(req.Ticket)}, &idn); err != nil {
		h.Log.WithError(err).Warn("roam redeem: directory verify failed")
		writeGreyboxError(w, http.StatusUnauthorized, -32006, "unknown or expired ticket")
		return
	}
	id := denormRoamID(idn.UserID)
	var user models.User
	var bannedAt *string
	err := h.DB.QueryRow(`SELECT id,username,email,password_hash,banned_at FROM users WHERE id=?`,
		id).Scan(&user.ID, &user.Username, &user.Email, &user.PasswordHash, &bannedAt)
	switch {
	case err == nil && bannedAt != nil:
		writeGreyboxError(w, http.StatusForbidden, -32007, "account banned")
		return
	case err != nil:
		stubHash := "!roam-" + idn.UserID
		if _, err := h.DB.Exec(`INSERT INTO users(id,username,email,password_hash) VALUES(?,?,?,?)`,
			id, idn.Username, idn.Email, stubHash); err != nil {
			// Almost certainly a UNIQUE collision: this name or address is
			// registered here under a DIFFERENT id (double registration in
			// the sync window). Signing into the other row would be the
			// wrong account — refuse and say so.
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				h.Log.WithField(fieldUserID, id).Warn("roam redeem: local name/address collision")
				writeGreyboxError(w, http.StatusConflict, -32002, "username or email already taken here under another id — sign in with password")
				return
			}
			h.Log.WithError(err).Error("roam redeem: stub account")
			writeGreyboxError(w, http.StatusInternalServerError, -32603, "Login failed")
			return
		}
		user = models.User{ID: id, Username: idn.Username, Email: idn.Email, PasswordHash: stubHash}
		h.Log.WithFields(logrus.Fields{fieldUserID: id, fieldUsername: idn.Username}).Info("roam redeem: stub account created")
	default:
		h.Log.WithFields(logrus.Fields{fieldUserID: id, fieldUsername: user.Username}).Info("roam redeem: session issued")
	}
	h.issueAndReturnJWT(w, user, nil, "dreadnought")
}
