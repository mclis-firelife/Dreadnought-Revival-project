package handlers

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	masterdb "github.com/darkace1998/Dreadnought-Revival-project/master-master/db"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
)

func testHandler(t *testing.T) *Handler {
	t.Helper()
	database, err := masterdb.Open(":memory:")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	log := logrus.New()
	log.SetOutput(io.Discard)
	return &Handler{DB: database, Log: log}
}

// testCACert mints a real self-signed CA so fingerprint validation runs
// against actual x509 bytes, not fixtures.
func testCACert(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test cluster CA"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		// Without BasicConstraintsValid Go omits the extension on the wire,
		// and the parsed certificate reports IsCA=false (exactly what the
		// server checks for).
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func registerTestCluster(t *testing.T, h *Handler, name string) string {
	t.Helper()
	ca := testCACert(t)
	body, _ := json.Marshal(map[string]any{
		"name": name, "web_url": "https://play.example.org", "battle_ip": "203.0.113.7",
		"version": "1.0", "motd": "welcome", "ca_cert": ca, "players": 3, "servers": 1,
		"contact_email": "owner@example.org",
	})
	rec := httptest.NewRecorder()
	h.Register(rec, httptest.NewRequest(http.MethodPost, "/clusters/register", strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register %s: status %d, body %s", name, rec.Code, rec.Body.String())
	}
	var doc struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || doc.ID == "" {
		t.Fatalf("register %s: no id back (%s)", name, rec.Body.String())
	}
	return doc.ID
}

func TestRegisterListHeartbeatDeregister(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Test Cluster")

	// Listed with fingerprint and counts.
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	var list struct {
		Clusters []Cluster `json:"clusters"`
		Count    int       `json:"count"`
	}
	if err := json.Unmarshal(recBody(t, rec), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Count != 1 || list.Clusters[0].ID != id {
		t.Fatalf("unexpected list: %s", rec.Body.String())
	}
	if len(list.Clusters[0].CAFingerprint) != 64 || list.Clusters[0].Players != 3 {
		t.Errorf("fingerprint/counts wrong: %+v", list.Clusters[0])
	}
}

func recBody(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func heartbeatReq(id, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/clusters/"+id+"/heartbeat", strings.NewReader(body))
	return mux.SetURLVars(req, map[string]string{"id": id})
}

func TestHeartbeatAndDeregister(t *testing.T) {	h := testHandler(t)
	id := registerTestCluster(t, h, "HB Cluster")

	rec := httptest.NewRecorder()
	h.Heartbeat(rec, heartbeatReq(id, `{"players":9,"servers":2}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat status %d, body %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	var list struct {
		Clusters []Cluster `json:"clusters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Clusters) != 1 || list.Clusters[0].Players != 9 {
		t.Fatalf("heartbeat did not update: %s", rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodDelete, "/clusters/"+id, nil)
	req = mux.SetURLVars(req, map[string]string{"id": id})
	rec = httptest.NewRecorder()
	h.Deregister(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deregister status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Fatalf("cluster still listed: %s", rec.Body.String())
	}
}

func TestRegisterValidation(t *testing.T) {	h := testHandler(t)
	ca := testCACert(t)
	good := map[string]any{
		"name": "ok", "web_url": "https://x.example", "battle_ip": "203.0.113.9", "ca_cert": ca,
		"contact_email": "owner@example.org",
	}
	for _, tc := range []struct {
		name  string
		mutate func(map[string]any)
	}{
		{"bad name", func(m map[string]any) { m["name"] = "a/b" }},
		{"bad url", func(m map[string]any) { m["web_url"] = "ftp://x" }},
		{"bad ip", func(m map[string]any) { m["battle_ip"] = "not a host!" }},
		{"bad ca", func(m map[string]any) { m["ca_cert"] = "garbage" }},
		{"missing email", func(m map[string]any) { m["contact_email"] = "not-an-address" }},
		{"garbage body", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body string
			if tc.mutate == nil {
				body = "not json"
			} else {
				m := map[string]any{}
				for k, v := range good {
					m[k] = v
				}
				tc.mutate(m)
				raw, _ := json.Marshal(m)
				body = string(raw)
			}
			// unique name per case so validation, not uniqueness, is tested
			rec := httptest.NewRecorder()
			h.Register(rec, httptest.NewRequest(http.MethodPost, "/clusters/register", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("got %d, want 400", rec.Code)
			}
		})
	}
}

func adminReq(t *testing.T, h *Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	// route vars for /admin/api/clusters/{id}/...
	if id := pathID(path); id != "" {
		req = mux.SetURLVars(req, map[string]string{"id": id})
	}
	rec := httptest.NewRecorder()
	switch {
	case strings.HasSuffix(path, "/motd"):
		h.AdminSetMOTD(rec, req)
	case strings.HasSuffix(path, "/block"):
		h.AdminBlock(rec, req)
	case strings.HasSuffix(path, "/unblock"):
		h.AdminUnblock(rec, req)
	case method == http.MethodDelete:
		h.AdminDelete(rec, req)
	default:
		t.Fatalf("no admin handler for %s %s", method, path)
	}
	return rec
}

func pathID(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 4 && parts[0] == "admin" && parts[1] == "api" && parts[2] == "clusters" {
		return parts[3]
	}
	return ""
}

func TestAdminMOTDBlockUnblockDelete(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Mod Cluster")

	// MOTD change shows up in the public list.
	if rec := adminReq(t, h, "POST", "/admin/api/clusters/"+id+"/motd", `{"motd":"event tonight"}`); rec.Code != http.StatusOK {
		t.Fatalf("motd status %d, body %s", rec.Code, rec.Body.String())
	}
	rec := httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if !strings.Contains(rec.Body.String(), "event tonight") {
		t.Fatalf("motd not listed: %s", rec.Body.String())
	}
	if rec := adminReq(t, h, "POST", "/admin/api/clusters/"+id+"/motd", `{"motd":"`+strings.Repeat("x", 501)+`"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("overlong motd: got %d, want 400", rec.Code)
	}

	// Block: gone from the public list, re-register refused.
	if rec := adminReq(t, h, "POST", "/admin/api/clusters/"+id+"/block", ""); rec.Code != http.StatusOK {
		t.Fatalf("block status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Fatalf("blocked cluster still listed: %s", rec.Body.String())
	}
	ca := testCACert(t)
	body, _ := json.Marshal(map[string]any{
		"name": "Mod Cluster", "web_url": "https://x.example", "battle_ip": "203.0.113.9", "ca_cert": ca,
		"contact_email": "owner@example.org",
	})
	rec = httptest.NewRecorder()
	h.Register(rec, httptest.NewRequest(http.MethodPost, "/clusters/register", strings.NewReader(string(body))))
	if rec.Code != http.StatusForbidden {
		t.Errorf("re-register of blocked cluster: got %d, want 403", rec.Code)
	}

	// Unblock: next register revives it.
	if rec := adminReq(t, h, "POST", "/admin/api/clusters/"+id+"/unblock", ""); rec.Code != http.StatusOK {
		t.Fatalf("unblock status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.Register(rec, httptest.NewRequest(http.MethodPost, "/clusters/register", strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("re-register after unblock: got %d, body %s", rec.Code, rec.Body.String())
	}

	// Delete removes it entirely.
	if rec := adminReq(t, h, "DELETE", "/admin/api/clusters/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Fatalf("deleted cluster still listed: %s", rec.Body.String())
	}
}

func TestAdminListAllSeesBlocked(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Shadow Cluster")
	if rec := adminReq(t, h, "POST", "/admin/api/clusters/"+id+"/block", ""); rec.Code != http.StatusOK {
		t.Fatalf("block status %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	h.AdminListAll(rec, httptest.NewRequest(http.MethodGet, "/admin/api/clusters", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var doc struct {
		Clusters []struct {
			Name    string `json:"name"`
			Blocked bool   `json:"blocked"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Clusters) != 1 || !doc.Clusters[0].Blocked {
		t.Fatalf("unexpected admin list: %s", rec.Body.String())
	}
}

// testLeafCert mints a non-CA (server) certificate: uploading server.crt
// instead of ca.crt must fail with a clear message, not later in the browser.
func testLeafCert(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "play.example.org"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"play.example.org"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestRegisterRefusesNonCACert(t *testing.T) {
	h := testHandler(t)
	body, _ := json.Marshal(map[string]any{
		"name": "Leaf Cluster", "web_url": "https://x.example", "battle_ip": "203.0.113.9",
		"ca_cert": testLeafCert(t), "contact_email": "owner@example.org",
	})
	rec := httptest.NewRecorder()
	h.Register(rec, httptest.NewRequest(http.MethodPost, "/clusters/register", strings.NewReader(string(body))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for a server cert instead of a CA", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ca.crt") {
		t.Errorf("error should point at ca.crt, got: %s", rec.Body.String())
	}
}

func TestRegisterWithoutCAListsEmptyFingerprint(t *testing.T) {
	h := testHandler(t)
	body, _ := json.Marshal(map[string]any{
		"name": "Public Cluster", "web_url": "https://x.example", "battle_ip": "203.0.113.9",
		"contact_email": "owner@example.org",
	})
	rec := httptest.NewRecorder()
	h.Register(rec, httptest.NewRequest(http.MethodPost, "/clusters/register", strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 without ca_cert (%s)", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	var doc struct {
		Clusters []struct {
			Name          string `json:"name"`
			CAFingerprint string `json:"ca_fingerprint"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Clusters) != 1 || doc.Clusters[0].CAFingerprint != "" {
		t.Fatalf("unexpected list: %s", rec.Body.String())
	}
}

func secretReq(t *testing.T, h *Handler, id, action string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/clusters/"+id+"/secret",
		strings.NewReader(`{"action":"`+action+`"}`))
	req = mux.SetURLVars(req, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	h.AdminSecret(rec, req)
	return rec
}

func TestAdminSecretGenerateSendRevoke(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Secret Cluster")

	// Generate: plaintext returned once.
	rec := secretReq(t, h, id, "generate")
	if rec.Code != http.StatusOK {
		t.Fatalf("generate status %d, body %s", rec.Code, rec.Body.String())
	}
	var gen struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gen); err != nil || len(gen.Secret) != 48 {
		t.Fatalf("no usable secret back: %s", rec.Body.String())
	}
	var setAt string
	if err := h.DB.QueryRow(`SELECT secret_set_at FROM clusters WHERE id=?`, id).Scan(&setAt); err != nil || setAt == "" {
		t.Fatalf("secret_set_at missing: %q (err %v)", setAt, err)
	}

	// Send without an agent URL: refused, never downgraded to plaintext.
	rec = secretReq(t, h, id, "send")
	if rec.Code != http.StatusOK {
		t.Fatalf("send status %d, body %s", rec.Code, rec.Body.String())
	}
	var sent struct {
		Sent      bool   `json:"sent"`
		SendError string `json:"send_error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sent); err != nil || sent.Sent {
		t.Fatalf("send without agent URL must fail safe: %s", rec.Body.String())
	}

	// Revoke: sync auth with the old secret must die. The secret hash is
	// gone, so even a well-formed push is refused.
	if rec := secretReq(t, h, id, "revoke"); rec.Code != http.StatusOK {
		t.Fatalf("revoke status %d", rec.Code)
	}
	pushed, _ := json.Marshal(map[string]any{"users": []any{}, "snapshots": []any{}})
	req := httptest.NewRequest(http.MethodPost, "/sync/push", strings.NewReader(string(pushed)))
	req.Header.Set("X-Sync-Key", gen.Secret)
	rec = httptest.NewRecorder()
	h.SyncPush(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("push with revoked secret: got %d, want 403", rec.Code)
	}

	if rec := secretReq(t, h, id, "generate"); rec.Code != http.StatusOK {
		t.Errorf("re-generate after revoke: got %d", rec.Code)
	}
}

func TestAdminSecretRejectsGarbage(t *testing.T) {
	h := testHandler(t)
	if rec := secretReq(t, h, "00000000-0000-0000-0000-000000000000", "generate"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown cluster: got %d, want 404", rec.Code)
	}
	id := registerTestCluster(t, h, "Action Cluster")
	if rec := secretReq(t, h, id, "explode"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad action: got %d, want 400", rec.Code)
	}
}

func blockReq(t *testing.T, h *Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/admin/api/clusters/"+id+"/block", strings.NewReader(body))
	req = mux.SetURLVars(req, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	h.AdminBlock(rec, req)
	return rec
}

func registerBody(name string) string {
	raw, _ := json.Marshal(map[string]any{
		"name": name, "web_url": "https://play.example.org", "battle_ip": "203.0.113.7",
		"version": "1.0", "contact_email": "owner@example.org",
	})
	return string(raw)
}

func TestAdminBlockTemporaryExpires(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Tempblocked")

	rec := blockReq(t, h, id, `{"reason":"maintenance window","minutes":60}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("block: %d %s", rec.Code, rec.Body.String())
	}
	var blocked struct {
		Until string `json:"until"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &blocked); err != nil || blocked.Until == "" {
		t.Fatalf("no expiry back: %s", rec.Body.String())
	}

	// Blocked: re-register refused with the reason, browser list hides it.
	rec = httptest.NewRecorder()
	h.Register(rec, httptest.NewRequest(http.MethodPost, "/clusters/register",
		strings.NewReader(registerBody("Tempblocked"))))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "maintenance window") {
		t.Fatalf("register while blocked: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if strings.Contains(rec.Body.String(), "Tempblocked") {
		t.Fatalf("blocked cluster listed: %s", rec.Body.String())
	}

	// Admin sees reason and expiry.
	rec = httptest.NewRecorder()
	h.AdminListAll(rec, httptest.NewRequest(http.MethodGet, "/admin/api/clusters", nil))
	if !strings.Contains(rec.Body.String(), "maintenance window") || !strings.Contains(rec.Body.String(), blocked.Until) {
		t.Fatalf("admin list hides block detail: %s", rec.Body.String())
	}

	// Elapse the block: registration works again, list shows it.
	if _, err := h.DB.Exec(`UPDATE clusters SET blocked_until='2020-01-01T00:00:00Z' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.Register(rec, httptest.NewRequest(http.MethodPost, "/clusters/register",
		strings.NewReader(registerBody("Tempblocked"))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register after expiry: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.List(rec, httptest.NewRequest(http.MethodGet, "/clusters", nil))
	if !strings.Contains(rec.Body.String(), "Tempblocked") {
		t.Fatalf("expired block still hides: %s", rec.Body.String())
	}
}

func TestAdminBlockValidation(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Validated")
	if rec := blockReq(t, h, id, `{"minutes":-1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("negative minutes: got %d, want 400", rec.Code)
	}
	if rec := blockReq(t, h, id, `{"reason":"`+strings.Repeat("x", 201)+`"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("long reason: got %d, want 400", rec.Code)
	}
	if rec := blockReq(t, h, "00000000-0000-0000-0000-000000000000", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown cluster: got %d, want 404", rec.Code)
	}
	// Indefinite block (no body fields) still works, then unblock clears all.
	if rec := blockReq(t, h, id, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("indefinite block: %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/clusters/"+id+"/unblock", nil)
	req = mux.SetURLVars(req, map[string]string{"id": id})
	rec := httptest.NewRecorder()
	h.AdminUnblock(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unblock: %d", rec.Code)
	}
	var blocked int
	var until, reason string
	if err := h.DB.QueryRow(`SELECT blocked,blocked_until,blocked_reason FROM clusters WHERE id=?`,
		id).Scan(&blocked, &until, &reason); err != nil || blocked != 0 || until != "" || reason != "" {
		t.Fatalf("unblock residue: %d %q %q (err %v)", blocked, until, reason, err)
	}
}

func TestAdminMotdAll(t *testing.T) {
	h := testHandler(t)
	registerTestCluster(t, h, "Motd One")
	registerTestCluster(t, h, "Motd Two")
	rec := httptest.NewRecorder()
	h.AdminMotdAll(rec, httptest.NewRequest(http.MethodPost, "/admin/api/motd-all",
		strings.NewReader(`{"motd":"maintenance tonight"}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"updated":2`) {
		t.Fatalf("motd-all: %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM clusters WHERE motd='maintenance tonight'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("motd rows = %d (err %v)", n, err)
	}
	rec = httptest.NewRecorder()
	h.AdminMotdAll(rec, httptest.NewRequest(http.MethodPost, "/admin/api/motd-all",
		strings.NewReader(`{"motd":"`+strings.Repeat("x", 501)+`"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("long motd: got %d, want 400", rec.Code)
	}
}

func TestAdminNoteRoundtrip(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Noted")
	set := func(body string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/admin/api/clusters/"+id+"/note", strings.NewReader(body))
		req = mux.SetURLVars(req, map[string]string{"id": id})
		rec := httptest.NewRecorder()
		h.AdminSetNote(rec, req)
		return rec.Code
	}
	if code := set(`{"note":"owner on holiday, back Monday"}`); code != http.StatusOK {
		t.Fatalf("set note: %d", code)
	}
	rec := httptest.NewRecorder()
	h.AdminListAll(rec, httptest.NewRequest(http.MethodGet, "/admin/api/clusters", nil))
	if !strings.Contains(rec.Body.String(), "owner on holiday") {
		t.Fatalf("note missing from admin list: %s", rec.Body.String())
	}
	if code := set(`{"note":"` + strings.Repeat("x", 501) + `"}`); code != http.StatusBadRequest {
		t.Fatalf("long note: got %d, want 400", code)
	}
	if code := set(`{"note":""}`); code != http.StatusOK {
		t.Fatalf("clear note: %d", code)
	}
}

func TestHeartbeatHistoryRecordsTransitions(t *testing.T) {
	h := testHandler(t)
	id := registerTestCluster(t, h, "Flappy")
	// Registration records the first online event.
	var n int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM heartbeat_events WHERE cluster_id=? AND event='online'`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("online events = %d (err %v), want 1", n, err)
	}
	// A repeat heartbeat is steady state: no new event.
	hb := func() int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/clusters/"+id+"/heartbeat", strings.NewReader(`{}`))
		req = mux.SetURLVars(req, map[string]string{"id": id})
		rec := httptest.NewRecorder()
		h.Heartbeat(rec, req)
		return rec.Code
	}
	if code := hb(); code != http.StatusOK {
		t.Fatalf("heartbeat: %d", code)
	}
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM heartbeat_events WHERE cluster_id=?`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("events after steady heartbeat = %d, want still 1", n)
	}
	// Offline interval then return: second online event.
	if _, err := h.DB.Exec(`UPDATE clusters SET status='offline',last_heartbeat='2020-01-01T00:00:00Z' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if code := hb(); code != http.StatusOK {
		t.Fatalf("return heartbeat: %d", code)
	}
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM heartbeat_events WHERE cluster_id=? AND event='online'`, id).Scan(&n); err != nil || n != 2 {
		t.Fatalf("online events after return = %d, want 2", n)
	}
	// History endpoint serves them newest-first.
	rec := httptest.NewRecorder()
	h.AdminHeartbeatHistory(rec, httptest.NewRequest(http.MethodGet, "/admin/api/heartbeat-history?cluster="+id, nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "online") {
		t.Fatalf("history: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminBackupDownloads(t *testing.T) {
	h := testHandler(t)
	registerTestCluster(t, h, "Backup Me")
	rec := httptest.NewRecorder()
	h.AdminBackup(rec, httptest.NewRequest(http.MethodGet, "/admin/api/backup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("backup: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "master-master-") || !strings.Contains(cd, ".db") {
		t.Fatalf("content-disposition = %q", cd)
	}
	if rec.Body.Len() < 1024 {
		t.Fatalf("backup suspiciously small: %d bytes", rec.Body.Len())
	}
}
