package attachments

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidationTable exercises every input shape the role + extension
// + local-id regexes filter at attachment-write time. The matrix
// covers ADR-0014 §5's path-traversal-blocking guarantee end-to-end:
// every sample below either matches the strict shape OR it gets
// rejected before any filesystem access.
func TestValidationTable(t *testing.T) {
	t.Parallel()

	roles := []struct {
		in string
		valid bool
	}{
		{"thumb", true},
		{"cover", true},
		{"screenshot-01", true},
		{"a", true},
		{"123", true},
		{"the-very-long-but-still-valid-rolex", false}, // 35 chars, cap is 32
		{"thumb-12345678901234567890123456", true}, // 32 chars exactly
		{"-thumb", false}, // can't start with dash
		{"thumb_role", false}, // underscore disallowed
		{"thumb.jpg", false}, // dot disallowed
		{"thumb/cover", false}, // slash disallowed (path-traversal)
		{"../../etc/passwd", false}, // path-traversal attempt
		{"THUMB", false}, // uppercase rejected
		{"thumb space", false}, // whitespace rejected
		{"thüü", false}, // non-ASCII rejected
		{"", false},
	}
	for _, r := range roles {
		t.Run("role/"+r.in, func(t *testing.T) {
			t.Parallel()
			err := validateRole(r.in)
			if (err == nil) != r.valid {
				t.Errorf("validateRole(%q): valid=%v, got err=%v", r.in, r.valid, err)
			}
		})
	}

	exts := []struct {
		in string
		valid bool
	}{
		{"jpg", true},
		{"png", true},
		{"jpeg", true},
		{"webm", true},
		{"flac", true},
		{"mp3", true},
		{"a", true},
		{"3", true},
		{"jpegjpeg2", true}, // 9 chars
		{"abcdefghij", true}, // 10 chars (cap)
		{"abcdefghijk", false}, // 11 chars (over cap)
		{"jpg/../../", false}, // path-traversal attempt
		{"JPG", false}, // uppercase rejected
		{"jp.g", false}, // dot rejected
		{"jp-g", false}, // dash rejected (stricter than role)
		{"jp_g", false}, // underscore rejected
		{"", false},
	}
	for _, e := range exts {
		t.Run("ext/"+e.in, func(t *testing.T) {
			t.Parallel()
			err := validateExtension(e.in)
			if (err == nil) != e.valid {
				t.Errorf("validateExtension(%q): valid=%v, got err=%v", e.in, e.valid, err)
			}
		})
	}

	localIDs := []struct {
		in string
		valid bool
	}{
		{"130680", true},
		{"susanna-clarke", true},
		{"go_programming_language", true},
		{"a-1_b-2", true},
		{"susanna.clarke", false}, // dot disallowed
		{"susanna/clarke", false}, // slash (path-traversal)
		{"../etc", false},
		{"", false},
		{"UPPER", false},
	}
	for _, l := range localIDs {
		t.Run("local-id/"+l.in, func(t *testing.T) {
			t.Parallel()
			err := validateLocalID(l.in)
			if (err == nil) != l.valid {
				t.Errorf("validateLocalID(%q): valid=%v, got err=%v", l.in, l.valid, err)
			}
		})
	}
}

// TestDestPath_TraversalGuard exercises ADR-0014 §5c's defense-in-depth
// re-check on the constructed destination path. Even if a malicious
// caller bypassed the role / extension regex (defense-in-depth means
// the inner check is the last line of defense), destPath must reject
// any path that doesn't sit in <vaultRoot>/<kind> as a direct child.
func TestDestPath_TraversalGuard(t *testing.T) {
	t.Parallel()

	vaultRoot := t.TempDir()
	good, err := destPath(vaultRoot, "boardgame", "130680", "thumb", "jpg")
	if err != nil {
		t.Fatalf("destPath(happy): %v", err)
	}
	want := filepath.Join(vaultRoot, "boardgame", "130680", "attachments", "thumb.jpg")
	if good != want {
		t.Errorf("destPath: want %q, got %q", want, good)
	}

	// All-bad inputs that the regexes reject directly.
	badCases := []struct {
		name string
		kind, localID, role, ext string
	}{
		{"role-with-slash", "boardgame", "130680", "../../etc", "jpg"},
		{"ext-with-slash", "boardgame", "130680", "thumb", "jpg/x"},
		{"localid-with-slash", "boardgame", "../../etc/passwd", "thumb", "jpg"},
		{"kind-with-slash", "../etc", "130680", "thumb", "jpg"},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := destPath(vaultRoot, tc.kind, tc.localID, tc.role, tc.ext); err == nil {
				t.Errorf("destPath(%s): want error, got nil", tc.name)
			}
		})
	}
}

// TestDispatch_FileScheme covers the file:// happy path: stage a
// source, dispatch, verify the dest exists with the right contents
// AND the source is removed (per ADR-0014 §2.file's tmp-cleanup
// rule).
func TestDispatch_FileScheme(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()

	src := filepath.Join(stagingDir, "thumb-123.jpg")
	wantBytes := []byte("\xff\xd8\xff\xe0fake jpeg")
	if err := os.WriteFile(src, wantBytes, 0o644); err != nil {
		t.Fatalf("write staging src: %v", err)
	}

	d, err := New(stagingDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "thumb", URI: "file://" + src, Extension: "jpg"},
		},
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "130680",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.FetchAttachments) != 1 {
		t.Fatalf("FetchAttachments: want 1, got %d", len(res.FetchAttachments))
	}

	dest := filepath.Join(vaultRoot, "boardgame", "130680", "attachments", "thumb.jpg")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest %q: %v", dest, err)
	}
	if string(got) != string(wantBytes) {
		t.Errorf("dest contents mismatch: want %q, got %q", wantBytes, got)
	}

	// Tmp cleanup: source must be gone (hardlink unlinks src; copy
	// removes after copy success).
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("staging src not cleaned up: stat err = %v", err)
	}
}

// TestDispatch_FileScheme_TraversalAttempt verifies a file:// URI
// pointing OUTSIDE stagingDir is rejected before any read/copy.
func TestDispatch_FileScheme_TraversalAttempt(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	outsideDir := t.TempDir() // separate tempdir, not under stagingDir
	vaultRoot := t.TempDir()

	src := filepath.Join(outsideDir, "thumb-123.jpg")
	if err := os.WriteFile(src, []byte("evil"), 0o644); err != nil {
		t.Fatalf("write evil src: %v", err)
	}

	d, err := New(stagingDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "thumb", URI: "file://" + src, Extension: "jpg"},
		},
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "130680",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	// Per-attachment failure is logged + skipped; result has 0
	// fetch_attachments, dispatch returns no error.
	if len(res.FetchAttachments) != 0 {
		t.Errorf("FetchAttachments: want 0 (traversal rejected), got %d", len(res.FetchAttachments))
	}
	dest := filepath.Join(vaultRoot, "boardgame", "130680", "attachments", "thumb.jpg")
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("traversal attempt landed on disk: %q exists", dest)
	}
	// And the evil src is preserved (we don't delete things outside
	// stagingDir, even if they were named in a malicious URI).
	if _, err := os.Stat(src); err != nil {
		t.Errorf("evil src incorrectly modified: %v", err)
	}
}

// TestDispatch_FileScheme_MissingSource verifies a file:// URI
// pointing at a non-existent path is logged + skipped without
// crashing the whole dispatch.
func TestDispatch_FileScheme_MissingSource(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()

	d, err := New(stagingDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "thumb", URI: "file://" + filepath.Join(stagingDir, "nonexistent.jpg"), Extension: "jpg"},
		},
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "130680",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.FetchAttachments) != 0 {
		t.Errorf("FetchAttachments: want 0 (missing source skipped), got %d", len(res.FetchAttachments))
	}
}

// TestDispatch_HTTPScheme covers the https:// happy path against an
// httptest server. Verifies the dest file exists with the response
// body bytes, default UA was sent, and 4xx/5xx skip the attachment.
func TestDispatch_HTTPScheme(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()

	const wantBody = "fake png bytes"
	var gotUA string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		switch r.URL.Path {
		case "/thumb.png":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, wantBody)
		case "/missing.png":
			http.NotFound(w, r)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	d, err := New(stagingDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "thumb", URI: srv.URL + "/thumb.png", Extension: "png"},
			{Role: "missing", URI: srv.URL + "/missing.png", Extension: "png"},
			{Role: "broken", URI: srv.URL + "/boom.png", Extension: "png"},
		},
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "130680",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Only the happy-path attachment lands.
	if len(res.FetchAttachments) != 1 {
		t.Fatalf("FetchAttachments: want 1, got %d (%+v)", len(res.FetchAttachments), res.FetchAttachments)
	}
	if res.FetchAttachments[0].Role != "thumb" {
		t.Errorf("FetchAttachments[0].Role: want thumb, got %q", res.FetchAttachments[0].Role)
	}
	if !strings.HasPrefix(gotUA, "yaad-index/") {
		t.Errorf("User-Agent: want yaad-index/* prefix, got %q", gotUA)
	}

	dest := filepath.Join(vaultRoot, "boardgame", "130680", "attachments", "thumb.png")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != wantBody {
		t.Errorf("dest body mismatch: want %q, got %q", wantBody, got)
	}
}

// TestDispatch_Base64Scheme covers the base64:// happy path with
// padded + unpadded encodings. Both should decode + write correctly.
func TestDispatch_Base64Scheme(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()

	d, err := New(stagingDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rawA := []byte("payload-A 12345")
	rawB := []byte("payload-B-no-pad")

	encodedA := base64.StdEncoding.EncodeToString(rawA)
	encodedB := base64.RawStdEncoding.EncodeToString(rawB)

	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "padded", URI: "base64://" + encodedA, Extension: "bin"},
			{Role: "unpadded", URI: "base64://" + encodedB, Extension: "bin"},
			{Role: "malformed", URI: "base64://!!! not base64 !!!", Extension: "bin"},
		},
		VaultRoot: vaultRoot,
		Kind: "fixture",
		LocalID: "id1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(res.FetchAttachments) != 2 {
		t.Fatalf("FetchAttachments: want 2 (malformed skipped), got %d", len(res.FetchAttachments))
	}

	gotA, _ := os.ReadFile(filepath.Join(vaultRoot, "fixture", "id1", "attachments", "padded.bin"))
	if string(gotA) != string(rawA) {
		t.Errorf("padded body: want %q, got %q", rawA, gotA)
	}
	gotB, _ := os.ReadFile(filepath.Join(vaultRoot, "fixture", "id1", "attachments", "unpadded.bin"))
	if string(gotB) != string(rawB) {
		t.Errorf("unpadded body: want %q, got %q", rawB, gotB)
	}
	// Malformed dest must NOT exist.
	if _, err := os.Stat(filepath.Join(vaultRoot, "fixture", "id1", "attachments", "malformed.bin")); !os.IsNotExist(err) {
		t.Errorf("malformed base64 landed on disk: %v", err)
	}
}

// TestDispatch_RefetchSkipped verifies the (role, uri) string-compare
// short-circuit per ADR-0014 §4: when the new emission's (role, uri)
// matches Previous, the dispatcher skips the fetch and the on-disk
// file is left untouched.
func TestDispatch_RefetchSkipped(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()
	d, _ := New(stagingDir)

	dest := filepath.Join(vaultRoot, "boardgame", "1", "attachments", "thumb.png")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const sentinel = "ALREADY-ON-DISK"
	if err := os.WriteFile(dest, []byte(sentinel), 0o644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	// Previous (role, uri) matches the next emission. Even though the
	// URI points at a non-existent path, dispatch should NOT touch
	// the dest.
	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "thumb", URI: "https://example.invalid/thumb.png", Extension: "png"},
		},
		Previous: []PreviousAttachment{
			{Role: "thumb", URI: "https://example.invalid/thumb.png"},
		},
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.FetchAttachments) != 1 {
		t.Errorf("FetchAttachments: want 1 (re-fetch-skipped still counts as 'present'), got %d",
			len(res.FetchAttachments))
	}

	got, _ := os.ReadFile(dest)
	if string(got) != sentinel {
		t.Errorf("dest was overwritten on a re-fetch-skipped path: got %q, want %q", got, sentinel)
	}
}

// TestDispatch_ForceRefetchBypassesComparison verifies the
// ForceRefetch flag bypasses the (role, uri) comparison and
// unconditionally fetches per ADR-0014 §4.
func TestDispatch_ForceRefetchBypassesComparison(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()

	src := filepath.Join(stagingDir, "fresh.jpg")
	if err := os.WriteFile(src, []byte("FRESH"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	dest := filepath.Join(vaultRoot, "boardgame", "1", "attachments", "thumb.jpg")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(dest, []byte("STALE"), 0o644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	d, _ := New(stagingDir)

	_, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "thumb", URI: "file://" + src, Extension: "jpg"},
		},
		Previous: []PreviousAttachment{
			{Role: "thumb", URI: "file://" + src}, // would skip without ForceRefetch
		},
		ForceRefetch: true,
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	got, _ := os.ReadFile(dest)
	if string(got) != "FRESH" {
		t.Errorf("force-refetch did not overwrite: got %q, want FRESH", got)
	}
}

// TestDispatch_DuplicateRole verifies ADR-0014 §1's duplicate-role
// rule: the daemon SHOULD log + use the last one. Two emissions for
// the same role on a single FetchResult result in the last one
// landing.
func TestDispatch_DuplicateRole(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()
	d, _ := New(stagingDir)

	srcA := filepath.Join(stagingDir, "a.png")
	srcB := filepath.Join(stagingDir, "b.png")
	_ = os.WriteFile(srcA, []byte("A"), 0o644)
	_ = os.WriteFile(srcB, []byte("B"), 0o644)

	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "thumb", URI: "file://" + srcA, Extension: "png"},
			{Role: "thumb", URI: "file://" + srcB, Extension: "png"},
		},
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(res.FetchAttachments) != 1 {
		t.Errorf("FetchAttachments: want 1 (duplicate role collapsed), got %d", len(res.FetchAttachments))
	}
	got, _ := os.ReadFile(filepath.Join(vaultRoot, "boardgame", "1", "attachments", "thumb.png"))
	if string(got) != "B" {
		t.Errorf("duplicate-role last-wins: got %q, want B", got)
	}
}

// TestDispatch_BadRoleSkipped verifies an invalid role on one
// attachment skips that one and lets the rest proceed.
func TestDispatch_BadRoleSkipped(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()
	d, _ := New(stagingDir)

	src := filepath.Join(stagingDir, "good.png")
	_ = os.WriteFile(src, []byte("G"), 0o644)

	res, err := d.Dispatch(context.Background(), DispatchInput{
		Attachments: []Attachment{
			{Role: "../etc/evil", URI: "file://" + src, Extension: "png"},
			{Role: "good", URI: "file://" + src, Extension: "png"},
		},
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.FetchAttachments) != 1 || res.FetchAttachments[0].Role != "good" {
		t.Errorf("FetchAttachments: want one good, got %+v", res.FetchAttachments)
	}
}

// TestDispatch_RejectsBadInput exercises whole-input validation:
// empty / non-absolute VaultRoot, malformed Kind / LocalID. These
// produce errors at Dispatch entry — the whole entity write should
// know to bail.
func TestDispatch_RejectsBadInput(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	d, _ := New(stagingDir)

	cases := []struct {
		name string
		in DispatchInput
		wantInErr string
	}{
		{
			name: "empty-vaultroot",
			in: DispatchInput{Kind: "k", LocalID: "1"},
			wantInErr: "VaultRoot",
		},
		{
			name: "relative-vaultroot",
			in: DispatchInput{VaultRoot: "rel/path", Kind: "k", LocalID: "1"},
			wantInErr: "absolute",
		},
		{
			name: "bad-localid",
			in: DispatchInput{VaultRoot: "/tmp", Kind: "k", LocalID: "../etc"},
			wantInErr: "local-id",
		},
		{
			name: "bad-kind",
			in: DispatchInput{VaultRoot: "/tmp", Kind: "../etc", LocalID: "1"},
			wantInErr: "kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := d.Dispatch(context.Background(), tc.in)
			if err == nil {
				t.Fatalf("Dispatch(%s): want error, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("Dispatch(%s): want err containing %q, got %q",
					tc.name, tc.wantInErr, err.Error())
			}
		})
	}
}

// TestNew_RejectsBadStagingDir verifies the staging-dir validation at
// constructor time: must be absolute + must exist + must be a
// directory.
func TestNew_RejectsBadStagingDir(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		dir string
	}{
		{"empty", ""},
		{"relative", "rel/path"},
		{"nonexistent", "/this/path/almost/certainly/does/not/exist/alice2-test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tc.dir); err == nil {
				t.Errorf("New(%q): want error, got nil", tc.dir)
			}
		})
	}
}

// TestDispatch_PopulatesManifest_FileScheme: ADR-0018 step 6 §Attachments
// — DispatchResult.Manifest carries one entry per successfully-
// dispatched attachment with name/kind/path/bytes. The Path is
// relative to the entity subdir (`attachments/<role>.<ext>`); MIME
// type comes from extension lookup.
func TestDispatch_PopulatesManifest_FileScheme(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()

	src := filepath.Join(stagingDir, "thumb-130680.jpg")
	body := []byte("\xff\xd8\xff fixture jpg bytes")
	if err := os.WriteFile(src, body, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	d, err := New(stagingDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := d.Dispatch(context.Background(), DispatchInput{
		VaultRoot: vaultRoot,
		Kind: "boardgame",
		LocalID: "130680",
		Attachments: []Attachment{
			{Role: "thumb", URI: "file://" + src, Extension: "jpg"},
		},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got, want := len(res.Manifest), 1; got != want {
		t.Fatalf("Manifest len: got %d, want %d", got, want)
	}
	m := res.Manifest[0]
	if m.Name != "thumb.jpg" {
		t.Errorf("Manifest[0].Name: got %q, want %q", m.Name, "thumb.jpg")
	}
	if m.Path != filepath.Join("attachments", "thumb.jpg") {
		t.Errorf("Manifest[0].Path: got %q, want %q", m.Path, filepath.Join("attachments", "thumb.jpg"))
	}
	if m.Bytes != int64(len(body)) {
		t.Errorf("Manifest[0].Bytes: got %d, want %d", m.Bytes, len(body))
	}
	// MIME detection from extension; "image/jpeg" is OS-stable for jpg.
	if m.Kind == "" {
		t.Errorf("Manifest[0].Kind: got empty (want extension-based MIME)")
	}
}

// TestDispatch_ManifestEntryOnReFetchSkip: even when a re-fetch is
// short-circuited (URI unchanged), the manifest must still carry an
// entry — the file is still on disk, the frontmatter must reflect it.
func TestDispatch_ManifestEntryOnReFetchSkip(t *testing.T) {
	t.Parallel()

	stagingDir := t.TempDir()
	vaultRoot := t.TempDir()
	src := filepath.Join(stagingDir, "thumb-1.jpg")
	if err := os.WriteFile(src, []byte("first-write"), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	d, err := New(stagingDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	uri := "file://" + src
	// First dispatch lands the file.
	if _, err = d.Dispatch(context.Background(), DispatchInput{
		VaultRoot: vaultRoot, Kind: "boardgame", LocalID: "1",
		Attachments: []Attachment{{Role: "thumb", URI: uri, Extension: "jpg"}},
	}); err != nil {
		t.Fatalf("first Dispatch: %v", err)
	}

	// Stage source again (the writer cleans up file:// sources on
	// success); previous dispatch unlinked it.
	if err := os.WriteFile(src, []byte("second-write-ignored"), 0o644); err != nil {
		t.Fatalf("re-write src: %v", err)
	}

	// Second dispatch with same URI in Previous list — re-fetch
	// short-circuits; manifest still surfaces.
	res, err := d.Dispatch(context.Background(), DispatchInput{
		VaultRoot: vaultRoot, Kind: "boardgame", LocalID: "1",
		Previous: []PreviousAttachment{{Role: "thumb", URI: uri}},
		Attachments: []Attachment{{Role: "thumb", URI: uri, Extension: "jpg"}},
	})
	if err != nil {
		t.Fatalf("second Dispatch: %v", err)
	}
	if got, want := len(res.Manifest), 1; got != want {
		t.Fatalf("Manifest after re-fetch skip: got %d entries, want %d", got, want)
	}
	if res.Manifest[0].Name != "thumb.jpg" {
		t.Errorf("re-fetch manifest: got name %q, want thumb.jpg", res.Manifest[0].Name)
	}
}
