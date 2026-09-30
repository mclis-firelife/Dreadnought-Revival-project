//go:build windows

package main

// Per-cluster accounts. Unlike dn-launcher's single global account, the
// browser talks to many clusters with independent user databases, so
// credentials are stored per cluster id (DPAPI-encrypted, this Windows user
// only). The password is never kept -- only the token.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// ---- DPAPI (Windows CryptProtectData) --------------------------------

var (
	crypt32dll         = syscall.NewLazyDLL("crypt32.dll")
	kernel32dll        = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtect   = crypt32dll.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32dll.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32dll.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func dpapiEncrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	if len(data) > int(^uint32(0)) {
		return nil, fmt.Errorf("data too large")
	}
	inBlob := dataBlob{
		//nolint:gosec // Length is bounded above and DPAPI requires a uint32 byte count.
		cbData: uint32(len(data)),
		pbData: &data[0],
	}
	var outBlob dataBlob
	const CRYPTPROTECT_UI_FORBIDDEN = 1
	ret, _, _ := procCryptProtect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0,
		CRYPTPROTECT_UI_FORBIDDEN,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", syscall.GetLastError())
	}
	result := make([]byte, outBlob.cbData)
	copy(result, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))
	return result, nil
}

func dpapiDecrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	if len(data) > int(^uint32(0)) {
		return nil, fmt.Errorf("data too large")
	}
	inBlob := dataBlob{
		//nolint:gosec // Length is bounded above and DPAPI requires a uint32 byte count.
		cbData: uint32(len(data)),
		pbData: &data[0],
	}
	var outBlob dataBlob
	const CRYPTPROTECT_UI_FORBIDDEN = 1
	ret, _, _ := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0,
		CRYPTPROTECT_UI_FORBIDDEN,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", syscall.GetLastError())
	}
	result := make([]byte, outBlob.cbData)
	copy(result, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))
	return result, nil
}

// ---- per-cluster credentials ------------------------------------------

type storedCredentials struct {
	Identifier string `json:"identifier"`
	Username   string `json:"username"`
	UserID     string `json:"user_id"`
	Token      string `json:"token"`
	IssuedAt   string `json:"issued_at"`
}

// credentialFileName confines one file per cluster key (see clusterTrustKey):
// filesystem-safe by construction (letters, digits, -, _, :).
func credentialFileName(clusterKey string) string {
	var sb strings.Builder
	for _, c := range clusterKey {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			sb.WriteRune(c)
		} else {
			sb.WriteRune('_')
		}
	}
	name := sb.String()
	if len(name) > 64 {
		name = name[:64]
	}
	return "account-" + name + ".json"
}

func credentialPath(clusterKey string) (string, error) {
	appData := os.Getenv("LOCALAPPDATA")
	if appData == "" {
		appData = os.TempDir()
	}
	dir := filepath.Join(appData, "DreadnoughtPS")
	//nolint:gosec // Confined to this user's private browser directory.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create account directory: %w", err)
	}
	return filepath.Join(dir, credentialFileName(clusterKey)), nil
}

func loadCredentials(clusterKey string) (storedCredentials, bool) {
	path, err := credentialPath(clusterKey)
	if err != nil {
		return storedCredentials{}, false
	}
	//nolint:gosec // Path is the browser's own private credential file.
	blob, err := os.ReadFile(path)
	if err != nil {
		return storedCredentials{}, false
	}
	plain, err := dpapiDecrypt(blob)
	if err != nil {
		return storedCredentials{}, false
	}
	var creds storedCredentials
	if json.Unmarshal(plain, &creds) != nil || creds.Token == "" {
		return storedCredentials{}, false
	}
	return creds, true
}

func saveCredentials(clusterKey string, creds storedCredentials) error {
	path, err := credentialPath(clusterKey)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}
	blob, err := dpapiEncrypt(plain)
	if err != nil {
		return fmt.Errorf("DPAPI encrypt credentials: %w", err)
	}
	//nolint:gosec // 0600 under the user's own LOCALAPPDATA.
	return os.WriteFile(path, blob, 0o600)
}

func clearCredentials(clusterKey string) {
	if path, err := credentialPath(clusterKey); err == nil {
		_ = os.Remove(path)
	}
}

// ---- auth against one cluster ------------------------------------------

type userObj struct {
	JWT         string `json:"jwt"`
	AccessToken string `json:"access_token"`
	Token       string `json:"token"`
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
}

type authResponse struct {
	ID      string            `json:"id"`
	JSONRPC string            `json:"jsonrpc"`
	Result  []json.RawMessage `json:"result"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func serverMessage(payload []byte, fallback string) string {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &body) == nil {
		if body.Error.Message != "" {
			return body.Error.Message
		}
		if body.Message != "" {
			return body.Message
		}
	}
	return fallback
}

// registerAccount creates an account on the selected cluster (username +
// email + 6-char password, like dn-launcher).
func registerAccount(authURL, username, email, password string) error {
	body, err := json.Marshal(map[string]string{
		"username": username,
		"email":    email,
		"password": password,
	})
	if err != nil {
		return fmt.Errorf("marshal registration: %w", err)
	}
	endpoint := strings.TrimSuffix(strings.TrimSpace(authURL), "/") + "/register"
	resp, err := browserHTTPClient().Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("contact %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode == http.StatusCreated:
		return nil
	case resp.StatusCode == http.StatusConflict:
		return fmt.Errorf("that username or email is already registered")
	case resp.StatusCode >= 400:
		return fmt.Errorf("%s", serverMessage(payload, fmt.Sprintf("registration failed (HTTP %d)", resp.StatusCode)))
	}
	return nil
}

// loginAccount exchanges an identifier and password for a JWT on the
// selected cluster.
func loginAccount(authURL, identifier, password string) (storedCredentials, error) {
	form := url.Values{}
	form.Set("grant_type", "password")
	form.Set("username", identifier)
	form.Set("password", password)

	endpoint := strings.TrimSuffix(strings.TrimSpace(authURL), "/") + "/"
	resp, err := browserHTTPClient().Post(endpoint, "application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()))
	if err != nil {
		return storedCredentials{}, fmt.Errorf("contact %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return storedCredentials{}, fmt.Errorf("%s", serverMessage(payload, "incorrect email or password"))
	}
	return parseLoginPayload(payload, identifier)
}

// parseLoginPayload reads a sign-in shaped response (plain or JSON-RPC
// enveloped) into stored credentials. Shared by password login and roam
// redeem so both accept exactly the same server shapes.
func parseLoginPayload(payload []byte, identifier string) (storedCredentials, error) {
	var user userObj
	if err := json.Unmarshal(payload, &user); err != nil {
		return storedCredentials{}, fmt.Errorf("could not read the sign-in response: %w", err)
	}
	token := firstNonEmpty(user.JWT, user.AccessToken, user.Token)
	if token == "" {
		var enveloped authResponse
		if json.Unmarshal(payload, &enveloped) == nil && len(enveloped.Result) >= 2 {
			var inner userObj
			if json.Unmarshal(enveloped.Result[1], &inner) == nil {
				token = firstNonEmpty(inner.JWT, inner.AccessToken, inner.Token)
				user = inner
			}
		}
	}
	if token == "" {
		return storedCredentials{}, fmt.Errorf("the server did not return a session token")
	}
	return storedCredentials{
		Identifier: identifier,
		Username:   user.Username,
		UserID:     user.UserID,
		Token:      token,
		IssuedAt:   time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// browserTokenExpired reports whether a stored JWT is past (or within a
// minute of) its expiry. Unreadable tokens count as expired: re-authenticating
// is the safe answer.
func browserTokenExpired(token string) bool {
	const skew = time.Minute
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return true
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return true
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return true
	}
	return time.Now().Add(skew).After(time.Unix(claims.Exp, 0))
}

// roamGrant asks the signed-in cluster for a roaming ticket to take
// elsewhere (Authorization: the cluster JWT). The ticket is short-lived;
// the browser redeems it immediately and never stores it.
func roamGrant(authURL, token string) (ticket, username string, err error) {
	endpoint := strings.TrimSuffix(strings.TrimSpace(authURL), "/") + "/roam/grant"
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{}`))
	if err != nil {
		return "", "", fmt.Errorf("contact %s: %w", endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := browserHTTPClient().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("contact %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%s", serverMessage(payload, "roaming not available here"))
	}
	var doc struct {
		Ticket   string `json:"ticket"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(payload, &doc); err != nil || len(doc.Ticket) < 16 {
		return "", "", fmt.Errorf("the server did not return a roaming ticket")
	}
	return doc.Ticket, doc.Username, nil
}

// roamRedeem trades a roaming ticket for a normal session on the selected
// cluster. The answer parses like a password login (same shapes accepted).
func roamRedeem(authURL, ticket, identifier string) (storedCredentials, error) {
	body, err := json.Marshal(map[string]string{"ticket": ticket})
	if err != nil {
		return storedCredentials{}, fmt.Errorf("marshal roaming ticket: %w", err)
	}
	endpoint := strings.TrimSuffix(strings.TrimSpace(authURL), "/") + "/roam/redeem"
	resp, err := browserHTTPClient().Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return storedCredentials{}, fmt.Errorf("contact %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return storedCredentials{}, fmt.Errorf("%s", serverMessage(payload, "roaming sign-in failed"))
	}
	return parseLoginPayload(payload, identifier)
}
