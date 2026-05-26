package bgg

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// cookieJarFileName is the on-disk basename for the persisted
// session-cookie snapshot inside the per-instance data dir. The
// file holds the bggo session cookies + the username they
// authenticated under so the next daemon process can resume the
// session via bggo.WithCookies without a fresh login.
const cookieJarFileName = "session.json"

// cookieJarFileMode pins the on-disk permission for the cookie
// jar — secrets-grade per ADR-0028 §1 #284 amendment + #256 env-
// file model. Owner-readable / writable, nothing else.
const cookieJarFileMode = 0o600

// cookieJar is the persisted shape of a logged-in session. The
// expiration mirrors the BGG SessionID cookie expiry so the
// daemon can short-circuit a load when the persisted session is
// stale.
type cookieJar struct {
	Username string         `json:"username"`
	Cookies  []cookieRecord `json:"cookies"`
	// SavedAt records when the jar was written. Operators can
	// inspect the file to see when the last login fired.
	SavedAt time.Time `json:"saved_at"`
}

// cookieRecord is the JSON-friendly shape for a single
// http.Cookie. Only the fields BGG's session-auth path actually
// sets are persisted; the others stay at their zero values on
// restore (the http.Client adds defaults for Domain/Path at
// request time).
type cookieRecord struct {
	Name    string    `json:"name"`
	Value   string    `json:"value"`
	Path    string    `json:"path,omitempty"`
	Domain  string    `json:"domain,omitempty"`
	Expires time.Time `json:"expires,omitempty"`
	Secure  bool      `json:"secure,omitempty"`
}

// errCookieJarAbsent is the sentinel for "no jar file at the
// configured data dir" — distinct from a parse error so callers
// can branch cleanly (absent → fresh login; corrupt → log + treat
// as absent).
var errCookieJarAbsent = errors.New("bgg: cookie jar absent")

// loadCookieJar reads the persisted session-cookie snapshot at
// `<dataDir>/session.json`. Returns errCookieJarAbsent when the
// file doesn't exist. Returns a wrapped parse error when the file
// exists but is malformed; the caller (the Plugin's lazy-login
// path) treats that as absent + WARNs.
//
// Expired sessions (any cookie whose Expires is in the past)
// resolve to errCookieJarAbsent so the caller forces a re-login —
// the daemon never tries to use an expired cookie against BGG.
func loadCookieJar(dataDir string) (username string, cookies []*http.Cookie, err error) {
	if dataDir == "" {
		return "", nil, errCookieJarAbsent
	}
	path := filepath.Join(dataDir, cookieJarFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, errCookieJarAbsent
		}
		return "", nil, fmt.Errorf("read cookie jar %s: %w", path, err)
	}
	var jar cookieJar
	if err := json.Unmarshal(raw, &jar); err != nil {
		return "", nil, fmt.Errorf("parse cookie jar %s: %w", path, err)
	}
	now := time.Now()
	for _, rec := range jar.Cookies {
		if !rec.Expires.IsZero() && rec.Expires.Before(now) {
			return "", nil, errCookieJarAbsent
		}
		cookies = append(cookies, &http.Cookie{
			Name:    rec.Name,
			Value:   rec.Value,
			Path:    rec.Path,
			Domain:  rec.Domain,
			Expires: rec.Expires,
			Secure:  rec.Secure,
		})
	}
	return jar.Username, cookies, nil
}

// saveCookieJar persists the post-login session-cookie snapshot
// to `<dataDir>/session.json` at 0600 perms. Empty dataDir
// returns nil — the daemon-managed dir is unavailable so the
// session is process-lifetime-only (no error, but the next
// daemon process will re-login on first authenticated fetch).
//
// The write is atomic via a tempfile + rename so a daemon SIGKILL
// mid-write can't leave a corrupt jar. tempfiles land in dataDir
// itself (not /tmp) so the rename stays on the same filesystem.
func saveCookieJar(dataDir, username string, cookies []*http.Cookie) error {
	if dataDir == "" {
		return nil
	}
	records := make([]cookieRecord, 0, len(cookies))
	for _, c := range cookies {
		records = append(records, cookieRecord{
			Name:    c.Name,
			Value:   c.Value,
			Path:    c.Path,
			Domain:  c.Domain,
			Expires: c.Expires,
			Secure:  c.Secure,
		})
	}
	payload := cookieJar{
		Username: username,
		Cookies:  records,
		SavedAt:  time.Now(),
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal cookie jar: %w", err)
	}
	target := filepath.Join(dataDir, cookieJarFileName)
	tmp, err := os.CreateTemp(dataDir, ".session.json.*")
	if err != nil {
		return fmt.Errorf("create cookie jar tmpfile in %s: %w", dataDir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write cookie jar tmpfile %s: %w", tmpPath, err)
	}
	if err := tmp.Chmod(cookieJarFileMode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod cookie jar tmpfile %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close cookie jar tmpfile %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename cookie jar tmpfile %s -> %s: %w", tmpPath, target, err)
	}
	return nil
}
