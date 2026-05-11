package attach

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// TestStagingDir_DefaultAndOverride pins both branches of the env-
// var resolution.
func TestStagingDir_DefaultAndOverride(t *testing.T) {
	// Don't use t.Parallel — we're mutating process env.

	t.Run("default-when-unset", func(t *testing.T) {
		t.Setenv(EnvStagingDir, "")
		got := StagingDir()
		if got != DefaultStagingDir {
			t.Errorf("StagingDir() with %s unset: want %q, got %q",
				EnvStagingDir, DefaultStagingDir, got)
		}
	})

	t.Run("override-when-set", func(t *testing.T) {
		t.Setenv(EnvStagingDir, "/var/lib/alice2-index/staging")
		got := StagingDir()
		if got != "/var/lib/alice2-index/staging" {
			t.Errorf("StagingDir() with override: want %q, got %q",
				"/var/lib/alice2-index/staging", got)
		}
	})
}

// TestFile pins the file:// URI shape.
func TestFile(t *testing.T) {
	t.Parallel()

	got := File("thumb", "/tmp/yaad-bgg/130680-thumb.jpg", "jpg")
	want := Attachment{
		Role: "thumb",
		URI: "file:///tmp/yaad-bgg/130680-thumb.jpg",
		Extension: "jpg",
	}
	if got != want {
		t.Errorf("File: want %+v, got %+v", want, got)
	}
}

// TestURL pins that the URL is passed verbatim (no scheme munging).
func TestURL(t *testing.T) {
	t.Parallel()

	got := URL("thumb", "https://cf.geekdo-images.com/thumb.jpg", "jpg")
	want := Attachment{
		Role: "thumb",
		URI: "https://cf.geekdo-images.com/thumb.jpg",
		Extension: "jpg",
	}
	if got != want {
		t.Errorf("URL: want %+v, got %+v", want, got)
	}
}

// TestBytes pins the base64:// URI shape with a round-trip decode
// on the encoded payload.
func TestBytes(t *testing.T) {
	t.Parallel()

	raw := []byte("\xff\xd8\xff\xe0fake jpeg bytes 123")
	got := Bytes("thumb", raw, "jpg")
	if got.Role != "thumb" || got.Extension != "jpg" {
		t.Errorf("Bytes: role/extension wrong: %+v", got)
	}
	const prefix = "base64://"
	if !strings.HasPrefix(got.URI, prefix) {
		t.Fatalf("Bytes URI: want %q prefix, got %q", prefix, got.URI)
	}
	encoded := strings.TrimPrefix(got.URI, prefix)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("Bytes URI: decode encoded payload: %v (uri=%q)", err, got.URI)
	}
	if string(decoded) != string(raw) {
		t.Errorf("Bytes URI: decoded %q != original %q", decoded, raw)
	}
}

// TestAttachment_JSONShape pins the wire-tag JSON shape ADR-0014 §1
// describes — `{role, uri, extension}`. A regression in the struct
// tags would make plugin emissions undecodable on the daemon side.
func TestAttachment_JSONShape(t *testing.T) {
	t.Parallel()

	a := File("thumb", "/tmp/x.jpg", "jpg")
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got := string(b)
	want := `{"role":"thumb","uri":"file:///tmp/x.jpg","extension":"jpg"}`
	if got != want {
		t.Errorf("Marshal Attachment:\nwant %s\ngot %s", want, got)
	}
}
