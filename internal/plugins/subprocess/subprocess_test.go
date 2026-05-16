package subprocess

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/plugins"
)

// The fake-plugin pattern: the test binary itself doubles as the
// plugin executable. TestMain checks YAAD_TEST_FAKE_PLUGIN — if set,
// the binary behaves as a fake plugin (writes a canned --init or
// fetch response to stdout, exits with the controlled code) instead
// of running the test entrypoint.
//
// This avoids needing to compile a separate binary into testdata,
// keeps the test self-contained, and works on every OS go test runs
// on. Tests that need a fake plugin set the env var, set a "mode"
// var that controls the canned response, then exec.Command os.Args[0].

const fakePluginEnv = "YAAD_TEST_FAKE_PLUGIN"

// fakePluginMode names the various canned behaviours the test binary
// can adopt when re-invoked as a fake plugin.
const (
	fakeModeInitOK = "init-ok"
	fakeModeInitWithSearchSupport = "init-with-search-support"
	fakeModeInitMalformedJSON = "init-malformed-json"
	fakeModeInitNonZero = "init-nonzero-exit"
	fakeModeInitSlow = "init-slow"
	fakeModeFetchOKComplete = "fetch-ok-complete"
	fakeModeFetchOKNeedsFill = "fetch-ok-needs-fill"
	fakeModeFetchPluginErrorOK = "fetch-plugin-error-ok-false"
	fakeModeFetchInvalidEntity = "fetch-invalid-entity"
	fakeModeFetchNonZero = "fetch-nonzero-exit"
	fakeModeFetchMalformedJSON = "fetch-malformed-json"
	fakeModeFetchSlow = "fetch-slow"
	// fakeModeFetchBufferedThenHangOnSigterm exercises the #106
	// partial-commit-on-timeout contract: plugin writes two envelopes
	// through a bufio.Writer (the default 4KB capacity holds them —
	// they don't auto-flush) then blocks until SIGTERM. The SIGTERM
	// handler flushes + exits 0, so the bytes reach the daemon's
	// pipe-reader during the FetchTimeoutGrace window the daemon
	// allows between cancel and SIGKILL. Pre-#106 the daemon sent
	// SIGKILL immediately and these envelopes were lost.
	fakeModeFetchBufferedThenHangOnSigterm = "fetch-buffered-hang-on-sigterm"
	fakeModeVersionOKBare = "version-ok-bare"
	fakeModeVersionOKJSON = "version-ok-json"
	fakeModeVersionNonZero = "version-nonzero-exit"
	// fakeModeFetchWithEdgesAndCanonical exercises the wire-shape →
	// FetchResult mapping for `edges`, `canonical_entities`, and
	// `canonical_edges` (closes the source issue). All three were declared on
	// the wire shape (in some form) but only `edges` was orphaned;
	// canonical_* didn't exist at all. Test pins the round-trip.
	fakeModeFetchWithEdgesAndCanonical = "fetch-with-edges-and-canonical"
	// fakeModeFetchOKWithStderr writes a diagnostic line to stderr
	// before emitting a normal success response. Pins the issue
	// contract: success-path stderr must surface through the
	// yaad-index logger (otherwise's canonical-
	// axis instrumentation is dead-letter).
	fakeModeFetchOKWithStderr = "fetch-ok-with-stderr"
	// fakeModeFetchWithNotations exercises the the source issue a prior PRv2
	// `notations` wire-shape decode. Plugin emits all forms it
	// knows resolve to the same entity; the orchestrator (a prior PR)
	// will write them to entity_notations after Fetch.
	fakeModeFetchWithNotations = "fetch-with-notations"
	// fakeModeFetchWithAliases exercises the the source issue a prior PR
	// `aliases` wire-shape decode. Plugin emits a flat list
	// containing both bare-label and typed-prefixed aliases; the
	// orchestrator threads them through to Entity.Aliases for
	// Marshal-time merge with the ADR-0011 synthesized one.
	fakeModeFetchWithAliases = "fetch-with-aliases"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakePluginEnv); mode != "" {
		runFakePlugin(mode)
		return
	}
	os.Exit(m.Run())
}

// runFakePlugin is the test-binary's "act as a plugin" mode. It reads
// the mode from the env var, dispatches on os.Args[1] (--init vs
// fetch), writes the appropriate stdout / exits the appropriate code.
func runFakePlugin(mode string) {
	isInit := len(os.Args) >= 2 && os.Args[1] == "--init"

	switch mode {
	case fakeModeInitOK:
		if !isInit {
			os.Exit(64) // misuse — should only be invoked with --init
		}
		caps := Capabilities{
			Name: "fake",
			Version: "0.1.0",
			URLPatterns: []string{`^https?://example\.test/.*`},
			EntityKinds: []KindSpec{{Name: "fake-entity", DefaultTTLDays: 7}},
		}
		_ = json.NewEncoder(os.Stdout).Encode(caps)
		os.Exit(0)

	case fakeModeInitWithSearchSupport:
		if !isInit {
			os.Exit(64)
		}
		caps := Capabilities{
			Name: "fake",
			Version: "0.1.0",
			URLPatterns: []string{`^https?://example\.test/.*`},
			EntityKinds: []KindSpec{{Name: "fake-entity", DefaultTTLDays: 7}},
			SupportsSearch: true,
		}
		_ = json.NewEncoder(os.Stdout).Encode(caps)
		os.Exit(0)

	case fakeModeInitMalformedJSON:
		_, _ = fmt.Fprint(os.Stdout, `{not-actually-json`)
		os.Exit(0)

	case fakeModeInitNonZero:
		_, _ = fmt.Fprint(os.Stderr, "fake plugin: init failure")
		os.Exit(7)

	case fakeModeInitSlow:
		time.Sleep(2 * time.Second)
		os.Exit(0)

	case fakeModeFetchOKComplete:
		if isInit {
			caps := Capabilities{
				Name: "fake",
				URLPatterns: []string{`.*`},
				EntityKinds: []KindSpec{{Name: "source"}},
				SourceNamespace: "fake",
			}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		// drain stdin so the plugin doesn't crash on broken pipe
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = fmt.Fprint(os.Stdout, `{
			"ok": true,
			"structured": {
				"kind": "source",
				"name": "Complete",
				"data": {"title": "Complete"},
				"provenance": [{
					"source": "fake:fetch",
					"fetched_at": "2026-04-30T08:00:00Z",
					"ok": true
				}]
			}
		}`)
		os.Exit(0)

	case fakeModeFetchOKNeedsFill:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "fake"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = fmt.Fprint(os.Stdout, `{
			"ok": true,
			"structured": {
				"kind": "source",
				"name": "Partial",
				"data": {"title": "Partial"},
				"provenance": [{
					"source": "fake:fetch",
					"fetched_at": "2026-04-30T08:00:00Z",
					"ok": true
				}]
			},
			"raw_content": "<cleaned-content>",
			"raw_content_truncated": false,
			"gaps": {
				"summary": "one paragraph summary",
				"tags": "topic tags relevant to this entry"
			}
		}`)
		os.Exit(0)

	case fakeModeFetchPluginErrorOK:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "fake"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = fmt.Fprint(os.Stdout, `{"ok": false}`)
		os.Exit(0)

	case fakeModeFetchInvalidEntity:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "fake"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		// Empty kind triggers validateStructured's missing-kind path
		// (legacy shape with bare id + kind is rejected post-).
		_, _ = fmt.Fprint(os.Stdout, `{"ok": true, "structured": {"kind": ""}}`)
		os.Exit(0)

	case fakeModeFetchNonZero:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "fake"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = fmt.Fprint(os.Stderr, "fake plugin: upstream unreachable")
		os.Exit(3)

	case fakeModeFetchMalformedJSON:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "fake"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = fmt.Fprint(os.Stdout, `not actually json`)
		os.Exit(0)

	case fakeModeFetchSlow:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "fake"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		time.Sleep(3 * time.Second)
		os.Exit(0)

	case fakeModeFetchBufferedThenHangOnSigterm:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "fake"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		w := bufio.NewWriter(os.Stdout)
		_, _ = fmt.Fprint(w, `{"ok":true,"structured":{"kind":"source","name":"first","data":{},"provenance":[{"source":"fake","ok":true}]}}`+"\n")
		_, _ = fmt.Fprint(w, `{"ok":true,"structured":{"kind":"source","name":"second","data":{},"provenance":[{"source":"fake","ok":true}]}}`+"\n")
		// Bytes are sitting in the bufio.Writer; the kernel pipe is
		// still empty. Block on SIGTERM. Daemon's cmd.Cancel sends
		// SIGTERM on context deadline; the handler flushes + exits
		// before WaitDelay escalates to SIGKILL.
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM)
		<-sigCh
		_ = w.Flush()
		os.Exit(0)

	case fakeModeFetchWithEdgesAndCanonical:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "wikipedia"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		// ADR-0021 source-shape with the polymorphic edges block.
		// Daemon-side toFetchResult derives the source-node ID
		// (`wikipedia:martin-wallace`) and resolves each edges-block
		// target to a canonical-label endpoint
		// (`person:martin-wallace`) via slug.Slug.
		_, _ = fmt.Fprint(os.Stdout, `{
			"ok": true,
			"structured": {
				"kind": "source",
				"name": "Martin Wallace",
				"data": {"title": "Martin Wallace"},
				"edges": {
					"is_about": {"name": "Martin Wallace", "kind": "person"}
				},
				"provenance": [{
					"source": "fake:fetch",
					"fetched_at": "2026-04-30T08:00:00Z",
					"ok": true
				}]
			}
		}`)
		os.Exit(0)

	case fakeModeFetchWithNotations:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "wikipedia"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = fmt.Fprint(os.Stdout, `{
			"ok": true,
			"structured": {
				"kind": "source",
				"name": "Susanna Clarke",
				"data": {"title": "Susanna Clarke"},
				"provenance": [{"source": "fake:fetch", "fetched_at": "2026-04-30T08:00:00Z", "ok": true}]
			},
			"notations": [
				"https://en.wikipedia.org/wiki/Susanna_Clarke",
				"wikipedia: Susanna Clarke",
				"https://en.m.wikipedia.org/wiki/Susanna_Clarke"
			]
		}`)
		os.Exit(0)

	case fakeModeFetchWithAliases:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "wikipedia"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = fmt.Fprint(os.Stdout, `{
			"ok": true,
			"structured": {
				"kind": "source",
				"name": "Susanna Clarke",
				"data": {"title": "Susanna Clarke"},
				"provenance": [{"source": "fake:fetch", "fetched_at": "2026-04-30T08:00:00Z", "ok": true}]
			},
			"aliases": [
				"S. Clarke",
				"author_of: Jonathan Strange & Mr Norrell"
			]
		}`)
		os.Exit(0)

	case fakeModeFetchOKWithStderr:
		if isInit {
			caps := Capabilities{Name: "fake", URLPatterns: []string{`.*`}, SourceNamespace: "fake"}
			_ = json.NewEncoder(os.Stdout).Encode(caps)
			os.Exit(0)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		_, _ = fmt.Fprintln(os.Stderr, "fake plugin: canonical-axis: diag line one")
		_, _ = fmt.Fprintln(os.Stderr, "fake plugin: canonical-axis: diag line two")
		_, _ = fmt.Fprint(os.Stdout, `{
			"ok": true,
			"structured": {
				"kind": "source",
				"name": "Diag",
				"data": {"title": "Diag"},
				"provenance": [{"source": "fake:fetch", "fetched_at": "2026-04-30T08:00:00Z", "ok": true}]
			}
		}`)
		os.Exit(0)

	case fakeModeVersionOKBare:
		isVersion := len(os.Args) >= 2 && os.Args[1] == "--version"
		if !isVersion {
			os.Exit(64)
		}
		_, _ = fmt.Fprintln(os.Stdout, "0.1.0")
		os.Exit(0)

	case fakeModeVersionOKJSON:
		isVersion := len(os.Args) >= 2 && os.Args[1] == "--version"
		if !isVersion {
			os.Exit(64)
		}
		_, _ = fmt.Fprint(os.Stdout, `{"version":"0.1.0"}`)
		os.Exit(0)

	case fakeModeVersionNonZero:
		_, _ = fmt.Fprint(os.Stderr, "fake plugin: --version not supported")
		os.Exit(2)

	default:
		_, _ = fmt.Fprintf(os.Stderr, "fake plugin: unknown mode %q", mode)
		os.Exit(99)
	}
}

// newFakePlugin constructs a subprocess.Plugin pointing at the test
// binary (os.Args[0]) with the given fake-plugin mode set in the
// environment. The plugin's --init runs immediately; the test mode
// controls what --init returns.
func newFakePlugin(t *testing.T, mode string, opts ...Option) (*Plugin, error) {
	t.Helper()
	exe, err := os.Executable()
	require.NoError(t, err, "os.Executable")
	t.Setenv(fakePluginEnv, mode)
	return New("fake", exe, opts...)
}

func TestNew_RunsInitAndCachesCapabilities(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeInitOK)
	require.NoError(t, err, "New")

	caps := p.Capabilities()
	assert.Equal(t, "fake", caps.Name)
	assert.Equal(t, "0.1.0", caps.Version)
	require.Len(t, caps.URLPatterns, 1)
	require.Len(t, caps.EntityKinds, 1)
	assert.Equal(t, "fake-entity", caps.EntityKinds[0].Name)
	assert.False(t, caps.SupportsSearch,
		"SupportsSearch must default to false when --init omits the field ( a prior PR backward-compat)")
}

// TestPlugin_InitDecodesSupportsSearch pins the the source issue a prior PR
// wire-decode contract: a plugin emitting `"supports_search": true`
// in its --init capabilities document surfaces the flag on the
// parsed Capabilities. Plugins not opting in (TestNew_RunsInitAnd
// CachesCapabilities above) decode as zero-value false — the
// orchestrator (a prior PR) treats that as "skip this plugin on
// upstream-search fan-out."
func TestPlugin_InitDecodesSupportsSearch(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeInitWithSearchSupport)
	require.NoError(t, err, "New")
	caps := p.Capabilities()
	assert.True(t, caps.SupportsSearch,
		"SupportsSearch must reflect the --init-emitted value when the plugin opts in")
	// Sanity: rest of the capabilities still decode as expected.
	assert.Equal(t, "fake", caps.Name)
	assert.Equal(t, "0.1.0", caps.Version)
}

func TestNew_FailsOnMalformedInitJSON(t *testing.T) {
	_, err := newFakePlugin(t, fakeModeInitMalformedJSON)
	require.Error(t, err, "New: want error on malformed --init JSON")
	assert.Contains(t, err.Error(), "decode --init")
}

func TestNew_FailsOnNonZeroInitExit(t *testing.T) {
	_, err := newFakePlugin(t, fakeModeInitNonZero)
	require.Error(t, err, "New: want error on non-zero --init exit")
	// stderr peek surfaced in the error message.
	assert.Contains(t, err.Error(), "init failure",
		"error should surface stderr peek 'init failure'")
}

func TestNew_FailsOnSlowInit(t *testing.T) {
	_, err := newFakePlugin(t, fakeModeInitSlow, WithInitTimeout(500*time.Millisecond))
	// exec.Cmd kill on context-cancel surfaces in various ways across
	// Go versions; just confirm it's an error and not a spurious
	// success.
	assert.Error(t, err, "New: want timeout error")
}

func TestPlugin_MatchUsesPrecompiledPatterns(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeInitOK)
	require.NoError(t, err, "New")

	cases := map[string]bool{
		"https://example.test/foo": true,
		"http://example.test/bar": true,
		"https://example.test/wiki/Foo": true,
		"https://other.test/foo": false,
		"not a url at all": false,
	}
	for url, want := range cases {
		assert.Equal(t, want, p.Match(url), "Match(%q)", url)
	}
}

func TestPlugin_FetchHappyPathComplete(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchOKComplete)
	require.NoError(t, err, "New")

	res, err := p.Fetch(context.Background(), "https://example.test/anything")
	require.NoError(t, err, "Fetch")

	// ADR-0021 source-shape: daemon derives ID from
	// `<source_namespace>:<slug.Slug(name)>`. Fixture
	// SourceNamespace=="fake" + name=="Complete" → "fake:complete".
	assert.Equal(t, "fake:complete", res.Entity.ID)
	assert.Equal(t, "fake", res.Entity.Kind)
	require.Len(t, res.Provenance, 1)
	assert.Equal(t, "fake:fetch", res.Provenance[0].Source)
	assert.NotNil(t, res.Provenance[0].FetchedAt, "provenance[0].FetchedAt")
	assert.Empty(t, res.Gaps, "gaps: want empty (complete path)")
}

func TestPlugin_FetchHappyPathNeedsFill(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchOKNeedsFill)
	require.NoError(t, err, "New")

	res, err := p.Fetch(context.Background(), "https://example.test/anything")
	require.NoError(t, err, "Fetch")

	assert.Equal(t, "<cleaned-content>", res.RawContent)
	assert.False(t, res.RawContentTruncated)
	// Gaps is now a {field → description} map (per ADR-0002 universal-
	// state amendment). The fake plugin emits two gaps; assert the
	// keys round-tripped, descriptions are advisory and not asserted.
	require.Len(t, res.Gaps, 2)
	assert.Contains(t, res.Gaps, "summary")
	assert.Contains(t, res.Gaps, "tags")
}

// TestPlugin_FetchWiresEdgesAndCanonicalFields pins the wire-shape →
// FetchResult mapping introduced in. The fake plugin emits one
// row each in `edges`, `canonical_entities`, and `canonical_edges`;
// all three must round-trip onto FetchResult. Legacy, `edges` was
// orphaned (declared on the wire struct, never copied) and
// canonical_* didn't exist at all — yaad-wikipedia's a prior PR work
// blocked on this gap.
func TestPlugin_FetchWiresEdgesAndCanonicalFields(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchWithEdgesAndCanonical)
	require.NoError(t, err, "New")

	res, err := p.Fetch(context.Background(), "https://example.test/martin-wallace")
	require.NoError(t, err, "Fetch")

	// ADR-0021 source-shape: source-node ID derived from
	// `<source_namespace>:<slug.Slug(name)>`. Plugin's
	// SourceNamespace=="wikipedia" + name=="Martin Wallace".
	require.NotNil(t, res.Entity)
	assert.Equal(t, "wikipedia:martin-wallace", res.Entity.ID)
	assert.Equal(t, "wikipedia", res.Entity.Kind)
	assert.Equal(t, "Martin Wallace", res.SourceName)

	// Edges-block round-trip: the `edges` map populates SourceEdges
	// (descriptive {name, kind}) AND CanonicalEdges (resolved
	// `<kind>:<slug>` endpoints).
	require.Len(t, res.SourceEdges, 1)
	require.Len(t, res.SourceEdges["is_about"], 1)
	assert.Equal(t, "Martin Wallace", res.SourceEdges["is_about"][0].Name)
	assert.Equal(t, "person", res.SourceEdges["is_about"][0].Kind)

	require.Len(t, res.CanonicalEdges, 1)
	assert.Equal(t, "is_about", res.CanonicalEdges[0].Type)
	assert.Equal(t, "wikipedia:martin-wallace", res.CanonicalEdges[0].From)
	assert.Equal(t, "person:martin-wallace", res.CanonicalEdges[0].To)
}

// TestToFetchResult_AttachmentsWireRoundTrip pins the ADR-0014
// `attachments[]` JSON → plugins.FetchResult.Attachments mapping in
// fetchResponse.toFetchResult. Drives without spawning a real
// subprocess — toFetchResult is the only translation point so a
// targeted unit test is the cheapest regression guard.
func TestToFetchResult_AttachmentsWireRoundTrip(t *testing.T) {
	t.Parallel()

	r := &fetchResponse{
		OK: true,
		Structured: &structuredResponse{
			Kind: "source",
			Name: "Brass Birmingham",
		},
		Attachments: []attachmentResponse{
			{Role: "thumb", URI: "https://cf.geekdo-images.com/thumb.jpg", Extension: "jpg"},
			{Role: "cover", URI: "file:///tmp/staging/cover-130680.png", Extension: "png"},
		},
	}
	out := r.toFetchResult(Capabilities{SourceNamespace: "bgg"})
	require.Len(t, out.Attachments, 2)
	assert.Equal(t, "thumb", out.Attachments[0].Role)
	assert.Equal(t, "https://cf.geekdo-images.com/thumb.jpg", out.Attachments[0].URI)
	assert.Equal(t, "jpg", out.Attachments[0].Extension)
	assert.Equal(t, "cover", out.Attachments[1].Role)
	assert.Equal(t, "png", out.Attachments[1].Extension)
}

// TestToFetchResult_AttachmentsAbsentByDefault pins the empty-case:
// a fetchResponse without an `attachments` field on the wire
// produces a FetchResult with nil Attachments. Pre-ADR-0014 plugins
// emit no attachments field at all and must continue to work
// unchanged.
func TestToFetchResult_AttachmentsAbsentByDefault(t *testing.T) {
	t.Parallel()

	r := &fetchResponse{
		OK: true,
		Structured: &structuredResponse{
			Kind: "source",
			Name: "Brass Birmingham",
		},
	}
	out := r.toFetchResult(Capabilities{SourceNamespace: "bgg"})
	assert.Empty(t, out.Attachments,
		"Attachments: want nil when wire `attachments` field absent")
}

// TestPlugin_FetchEdgesAndCanonicalAbsentByDefault pins the negative
// case: a plugin response without `edges`, `canonical_entities`, or
// `canonical_edges` produces a FetchResult where those slices are
// nil/empty. Closes the regression-guard gap — a future change that
// inadvertently populates these with non-empty defaults would fail
// here. Reuses the existing happy-path-complete fixture which
// emits none of the three.
func TestPlugin_FetchEdgesAndCanonicalAbsentByDefault(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchOKComplete)
	require.NoError(t, err, "New")

	res, err := p.Fetch(context.Background(), "https://example.test/anything")
	require.NoError(t, err, "Fetch")

	assert.Empty(t, res.Edges, "Edges: want empty when wire field absent")
	assert.Empty(t, res.SourceEdges, "SourceEdges: want empty when wire `edges` block absent")
	assert.Empty(t, res.CanonicalEdges, "CanonicalEdges: want empty when wire `edges` block absent")
}

func TestPlugin_FetchOnPluginErrorReturnsError(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchPluginErrorOK)
	require.NoError(t, err, "New")
	_, err = p.Fetch(context.Background(), "https://example.test/x")
	require.Error(t, err, "Fetch with ok=false")
	assert.Contains(t, err.Error(), "ok=false")
}

func TestPlugin_FetchOnInvalidEntityReturnsError(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchInvalidEntity)
	require.NoError(t, err, "New")
	_, err = p.Fetch(context.Background(), "https://example.test/x")
	require.Error(t, err, "Fetch with empty id/kind")
	assert.Contains(t, err.Error(), "missing kind")
}

func TestPlugin_FetchOnNonZeroExitReturnsError(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchNonZero)
	require.NoError(t, err, "New")
	_, err = p.Fetch(context.Background(), "https://example.test/x")
	require.Error(t, err, "Fetch on non-zero exit")
	// stderr peek surfaced.
	assert.Contains(t, err.Error(), "upstream unreachable", "error should surface stderr peek")
	// exec.ExitError reachable via errors.As.
	var exitErr *exec.ExitError
	assert.True(t, errors.As(err, &exitErr), "error should wrap *exec.ExitError, got %T", err)
}

// TestPlugin_FetchSuccessForwardsStderrToLogger pins the the source issue
// contract: when a plugin writes to stderr but exits successfully,
// the bytes must surface through yaad-index's logger (otherwise
// canonical-axis-style instrumentation in plugins is dead-letter).
//
// The peek-into-error path on non-zero exit is unchanged (covered by
// TestPlugin_FetchOnNonZeroExitReturnsError); this test covers the
// previously-silent success path.
func TestPlugin_FetchSuccessForwardsStderrToLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	p, err := newFakePlugin(t, fakeModeFetchOKWithStderr, WithLogger(logger))
	require.NoError(t, err, "New")
	got, err := p.Fetch(context.Background(), "https://example.test/x")
	require.NoError(t, err, "Fetch on success path with stderr")
	require.NotNil(t, got, "FetchResult should be returned even with stderr")
	require.NotNil(t, got.Entity)
	assert.Equal(t, "fake:diag", got.Entity.ID)

	out := buf.String()
	assert.Contains(t, out, `level=INFO`, "log entry should be info level")
	assert.Contains(t, out, `msg="plugin stderr"`, "log message tag should be 'plugin stderr'")
	assert.Contains(t, out, `plugin=fake`, "log entry should tag plugin name")
	assert.Contains(t, out, "diag line one", "stderr content should be in the log entry")
	assert.Contains(t, out, "diag line two", "all stderr content surfaces, not just first line")
}

// TestPlugin_FetchSuccessQuietPluginNoLog covers the no-noise side:
// a successful Fetch with EMPTY stderr emits no log line.
func TestPlugin_FetchSuccessQuietPluginNoLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	p, err := newFakePlugin(t, fakeModeFetchOKComplete, WithLogger(logger))
	require.NoError(t, err, "New")
	_, err = p.Fetch(context.Background(), "https://example.test/x")
	require.NoError(t, err, "Fetch on quiet success path")

	assert.NotContains(t, buf.String(), `msg="plugin stderr"`,
		"empty stderr should not produce a log entry")
}

// TestPlugin_FetchWiresNotations pins the the source issue a prior PRv2 wire
// contract: a plugin emitting the `notations` array on the JSON
// response surfaces them on FetchResult.Notations in input order.
// Backwards-compat sibling: TestPlugin_FetchHappyPathComplete
// exercises a plugin that emits NO notations field — that case
// surfaces as a nil/empty slice (no panic).
func TestPlugin_FetchWiresNotations(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchWithNotations)
	require.NoError(t, err, "New")
	got, err := p.Fetch(context.Background(), "https://en.wikipedia.org/wiki/Susanna_Clarke")
	require.NoError(t, err, "Fetch")
	require.NotNil(t, got)
	assert.Equal(t, []string{
		"https://en.wikipedia.org/wiki/Susanna_Clarke",
		"wikipedia: Susanna Clarke",
		"https://en.m.wikipedia.org/wiki/Susanna_Clarke",
	}, got.Notations, "Notations: want all wire entries copied through in order")
}

// TestPlugin_FetchNotationsAbsentLandsAsEmpty pins the backward-
// compat path: a plugin's response with no `notations` field at
// all (existing legacy plugins) produces a nil/empty Notations
// slice on FetchResult — no panic.
func TestPlugin_FetchNotationsAbsentLandsAsEmpty(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchOKComplete)
	require.NoError(t, err, "New")
	got, err := p.Fetch(context.Background(), "https://example.test/x")
	require.NoError(t, err, "Fetch")
	require.NotNil(t, got)
	assert.Empty(t, got.Notations,
		"Notations: want nil/empty when plugin emits no `notations` field")
}

// TestPlugin_FetchWiresAliases pins the the source issue a prior PR wire
// contract: a plugin emitting the `aliases` array on the JSON
// response surfaces them on FetchResult.Aliases in input order.
// Bare-string and `<edge-type>: <label>` shapes coexist in the
// flat list.
func TestPlugin_FetchWiresAliases(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchWithAliases)
	require.NoError(t, err, "New")
	got, err := p.Fetch(context.Background(), "https://example.test/x")
	require.NoError(t, err, "Fetch")
	require.NotNil(t, got)
	assert.Equal(t, []string{
		"S. Clarke",
		"author_of: Jonathan Strange & Mr Norrell",
	}, got.Aliases, "Aliases: want all wire entries copied through in order")
}

// TestPlugin_FetchAliasesAbsentLandsAsEmpty pins the backward-
// compat path: a plugin's response with no `aliases` field
// produces a nil/empty Aliases slice on FetchResult — legacy
// plugins see no behavior change.
func TestPlugin_FetchAliasesAbsentLandsAsEmpty(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchOKComplete)
	require.NoError(t, err, "New")
	got, err := p.Fetch(context.Background(), "https://example.test/x")
	require.NoError(t, err, "Fetch")
	require.NotNil(t, got)
	assert.Empty(t, got.Aliases,
		"Aliases: want nil/empty when plugin emits no `aliases` field")
}

func TestPlugin_FetchOnMalformedResponseJSONReturnsError(t *testing.T) {
	p, err := newFakePlugin(t, fakeModeFetchMalformedJSON)
	require.NoError(t, err, "New")
	_, err = p.Fetch(context.Background(), "https://example.test/x")
	require.Error(t, err, "Fetch with malformed JSON")
	assert.Contains(t, err.Error(), "decode response")
}

func TestPlugin_FetchHonoursTimeout(t *testing.T) {
	// Init keeps the default (5s) so the race-instrumented test
	// binary's --init has time to start; the per-Fetch timeout is
	// the one we want to bound (1s). The fake plugin sleeps 3s in
	// the fetch path, so Fetch must time out at ~1s.
	p, err := newFakePlugin(t, fakeModeFetchSlow, WithFetchTimeout(time.Second))
	require.NoError(t, err, "New")
	start := time.Now()
	_, err = p.Fetch(context.Background(), "https://example.test/x")
	elapsed := time.Since(start)
	require.Error(t, err, "Fetch with slow plugin: want timeout error")
	assert.LessOrEqual(t, elapsed, 2500*time.Millisecond,
		"Fetch elapsed %s — want roughly 1s (WithFetchTimeout)", elapsed)
	// Error must name the knob that fired so operators don't have
	// to guess what `signal: killed` means.
	assert.Contains(t, err.Error(), "fetchTimeout=1s exceeded",
		"timeout error should name the knob; got %q", err.Error())
}

// TestPlugin_DefaultFetchTimeoutIsMinute pins the bumped default so
// future refactors don't accidentally regress to the original 5s.
// The 5s value SIGKILLed real-world fetches (yaad-gmail against
// a non-trivial inbox completed in ~10s).
func TestPlugin_DefaultFetchTimeoutIsMinute(t *testing.T) {
	assert.Equal(t, 60*time.Second, DefaultFetchTimeout,
		"DefaultFetchTimeout regressed; see #105")
	assert.Equal(t, 5*time.Second, DefaultInitTimeout,
		"DefaultInitTimeout regressed; see #105")
}

// TestPlugin_StreamDrainsBufferedEnvelopesOnTimeout pins the #106
// contract: when the fetch timeout fires, plugins that trap SIGTERM
// and flush their bufio.Writer on the way out commit those
// previously-buffered envelopes via onEnvelope (write-as-you-go
// partial commit). Pre-fix the daemon SIGKILL'd immediately and
// the buffered bytes never reached our pipe-reader, so
// envelopes_committed=0 even when the plugin had pages of in-flight
// output (the dogfooded gmail-fetch failure mode).
func TestPlugin_StreamDrainsBufferedEnvelopesOnTimeout(t *testing.T) {
	// 500ms is enough to land past the daemon-side wall-clock cancel;
	// FetchTimeoutGrace (2s) covers the SIGTERM handler's flush+exit.
	p, err := newFakePlugin(t, fakeModeFetchBufferedThenHangOnSigterm,
		WithFetchTimeout(500*time.Millisecond))
	require.NoError(t, err, "New")

	var got []*plugins.FetchResult
	streamErr := p.Stream(context.Background(), "https://example.test/x",
		func(env *plugins.FetchResult) error {
			got = append(got, env)
			return nil
		},
		nil,
	)
	require.Error(t, streamErr, "Stream surfaces the timeout")
	assert.Contains(t, streamErr.Error(), "fetchTimeout=500ms exceeded",
		"timeout error names the knob; got %q", streamErr.Error())
	require.Len(t, got, 2,
		"both pre-cancel envelopes must reach onEnvelope (partial commit on SIGTERM-grace)")
	assert.Equal(t, "fake:first", got[0].Entity.ID)
	assert.Equal(t, "fake:second", got[1].Entity.ID)
}

// --- RunVersion + NewWithCapabilities ( capabilities cache) ---

// TestRunVersion_AcceptsBareString covers the simple shape: plugin
// prints `0.1.0\n` on --version, exit 0. This is the lightest possible
// version probe a plugin author can implement.
// TestVersionCacheKey pins the hash-strip rule per yaad-index:
// the cache-key prefix is everything before the first `+`. Any
// post-`+` portion is build metadata (typically a git short hash)
// that the daemon MUST NOT include in the cache key — otherwise
// every rebuild invalidates the cache when the semver tag is
// stable. Bare versions round-trip unchanged for backward-compat
// with plugins that haven't migrated yet.
func TestVersionCacheKey(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in string
		want string
	}{
		// Build-metadata stripping (the load-bearing case).
		{"semver_with_build_hash", "v0.1.0+deadbeef", "v0.1.0"},
		{"semver_with_long_hash", "v0.2.0+abc123def456", "v0.2.0"},
		{"v_prefix_stripped_only_after_plus", "v1.2.3+alpha-1", "v1.2.3"},
		// Bare versions (backward-compat).
		{"bare_semver_v_prefix", "v0.1.0", "v0.1.0"},
		{"bare_semver_no_v_prefix", "0.1.0", "0.1.0"},
		// Pre-release identifier (`-`) is preserved — only `+`
		// strips. Pre-release IS part of the cache key (a
		// `0.1.0-rc1` plugin is semantically distinct from
		// `0.1.0`).
		{"prerelease_dash_preserved", "v0.1.0-rc1", "v0.1.0-rc1"},
		{"prerelease_with_build_hash", "v0.1.0-rc1+deadbeef", "v0.1.0-rc1"},
		// Garbage-input tolerance: function does NOT validate
		// semver shape. Caller compares both sides through this
		// function — whatever shape they agree on works.
		{"empty", "", ""},
		{"just_plus", "+", ""},
		{"plus_at_start", "+deadbeef", ""},
		{"plus_at_end", "v0.1.0+", "v0.1.0"},
		{"non_semver_passes_through", "weird-version", "weird-version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := VersionCacheKey(tc.in); got != tc.want {
				t.Errorf("VersionCacheKey(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRunVersion_AcceptsBareString(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err, "os.Executable")
	t.Setenv(fakePluginEnv, fakeModeVersionOKBare)

	v, err := RunVersion(context.Background(), exe, 5*time.Second)
	require.NoError(t, err, "RunVersion")
	assert.Equal(t, "0.1.0", v)
}

// TestRunVersion_AcceptsJSONShape covers the alternate JSON form:
// `{"version":"0.1.0"}` → "0.1.0". Same semantics as bare-string;
// plugin authors who already serialize JSON for --init can reuse the
// marshaler here.
func TestRunVersion_AcceptsJSONShape(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err, "os.Executable")
	t.Setenv(fakePluginEnv, fakeModeVersionOKJSON)

	v, err := RunVersion(context.Background(), exe, 5*time.Second)
	require.NoError(t, err, "RunVersion")
	assert.Equal(t, "0.1.0", v)
}

// TestRunVersion_NonZeroExitReturnsError is the fall-through guard:
// plugins that don't support --version exit non-zero. RunVersion
// surfaces this so the loader skips the cache lookup and goes
// straight to a full --init.
func TestRunVersion_NonZeroExitReturnsError(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err, "os.Executable")
	t.Setenv(fakePluginEnv, fakeModeVersionNonZero)

	_, err = RunVersion(context.Background(), exe, 5*time.Second)
	assert.Error(t, err, "RunVersion: want error on non-zero exit")
}

// TestNewWithCapabilities_SkipsInit constructs a Plugin from
// pre-loaded caps and asserts it works (Match dispatches via the
// pre-compiled regex; Capabilities returns the supplied document)
// WITHOUT the binary having been invoked for --init. Critical
// property for the cache hot-path: the binary file doesn't even need
// to exist on disk.
func TestNewWithCapabilities_SkipsInit(t *testing.T) {
	t.Parallel()

	const nonExistentPath = "/this/binary/does/not/exist"
	caps := Capabilities{
		Name: "cached",
		Version: "0.1.0",
		URLPatterns: []string{`^https?://example\.test/.+`},
		EntityKinds: []KindSpec{{Name: "fake"}},
	}
	p, err := NewWithCapabilities("cached", nonExistentPath, caps)
	require.NoError(t, err, "NewWithCapabilities")
	assert.Equal(t, "cached", p.Name())
	assert.True(t, p.Match("https://example.test/x"), "precompiled regex should fire on matching URL")
	assert.False(t, p.Match("https://other.invalid/x"), "should not fire on non-matching URL")
	assert.Equal(t, "0.1.0", p.Capabilities().Version)
}

// pluginEnv carries YAAD_TIMEZONE per yaad-index PR-D so
// subprocess plugins can stamp provenance fetched_at in the
// operator's TZ.
//
// NOT t.Parallel — clock.SetLocation is package-level global state.
func TestPluginEnv_CarriesYaadTimezone(t *testing.T) {
	prev := clock.Location()
	t.Cleanup(func() { clock.SetLocation(prev) })

	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	clock.SetLocation(berlin)

	envv := pluginEnv()
	var found string
	for _, kv := range envv {
		if strings.HasPrefix(kv, "YAAD_TIMEZONE=") {
			found = strings.TrimPrefix(kv, "YAAD_TIMEZONE=")
			break
		}
	}
	require.NotEmpty(t, found, "YAAD_TIMEZONE must be set in plugin env")
	assert.Equal(t, "Europe/Berlin", found)

	// Default (no SetLocation) → "UTC".
	clock.SetLocation(nil)
	envv = pluginEnv()
	for _, kv := range envv {
		if strings.HasPrefix(kv, "YAAD_TIMEZONE=") {
			assert.Equal(t, "UTC", strings.TrimPrefix(kv, "YAAD_TIMEZONE="))
			return
		}
	}
	t.Fatal("YAAD_TIMEZONE not in plugin env after clock reset")
}

// TestPluginEnv_CarriesPluginStagingDir pins ADR-0014 §6 PR-B's
// env-propagation contract: the plugin subprocess receives the
// operator-configured plugin_staging_dir under
// YAAD_PLUGIN_STAGING_DIR. SDK helper at pkg/plugin/attach reads
// the same name + falls back to /tmp.
//
// NOT t.Parallel — SetStagingDir is package-level global state
// (mirrors clock.SetLocation's discipline).
func TestPluginEnv_CarriesPluginStagingDir(t *testing.T) {
	t.Cleanup(func() { SetStagingDir("") })

	SetStagingDir("/var/lib/yaad-index/staging")
	envv := pluginEnv()
	var found string
	for _, kv := range envv {
		if strings.HasPrefix(kv, "YAAD_PLUGIN_STAGING_DIR=") {
			found = strings.TrimPrefix(kv, "YAAD_PLUGIN_STAGING_DIR=")
			break
		}
	}
	require.NotEmpty(t, found, "YAAD_PLUGIN_STAGING_DIR must be set in plugin env")
	assert.Equal(t, "/var/lib/yaad-index/staging", found)

	// Reset → falls back to DefaultPluginStagingDir.
	SetStagingDir("")
	envv = pluginEnv()
	for _, kv := range envv {
		if strings.HasPrefix(kv, "YAAD_PLUGIN_STAGING_DIR=") {
			assert.Equal(t, DefaultPluginStagingDir,
				strings.TrimPrefix(kv, "YAAD_PLUGIN_STAGING_DIR="),
				"YAAD_PLUGIN_STAGING_DIR must fall back to default after reset")
			return
		}
	}
	t.Fatal("YAAD_PLUGIN_STAGING_DIR not in plugin env after reset")
}

// TestNewWithCapabilities_RejectsBadRegex covers the only error path
// of the cache-loaded constructor — a malformed url_pattern in a
// stale cache row would fail to compile, and we want fail-fast not
// silent-misdispatch.
func TestNewWithCapabilities_RejectsBadRegex(t *testing.T) {
	t.Parallel()

	caps := Capabilities{
		Name: "cached",
		Version: "0.1.0",
		URLPatterns: []string{`(invalid-unclosed`},
	}
	_, err := NewWithCapabilities("cached", "/some/path", caps)
	assert.Error(t, err, "NewWithCapabilities: want error on malformed regex")
}

// TestSourceEdgesBlock_PolymorphicDecode pins the polymorphic
// decode path: each entry in the `edges` map is either a single
// `{name, kind}` object or a list thereof. The internal storage
// is uniformly `[]sourceEdgeTargetJSON` so downstream code never
// branches on shape (per ADR-0021 §1).
func TestSourceEdgesBlock_PolymorphicDecode(t *testing.T) {
	t.Parallel()

	const wire = `{
		"is_a": {"name": "bgg-record", "kind": "source-type"},
		"designed_by": [{"name": "Martin Wallace", "kind": "person"}, {"name": "Other", "kind": "person"}],
		"is_about": {"name": "Brass Birmingham", "kind": "boardgame"}
	}`
	var b sourceEdgesBlock
	require.NoError(t, json.Unmarshal([]byte(wire), &b))

	require.Len(t, b["is_a"], 1)
	assert.Equal(t, "bgg-record", b["is_a"][0].Name)
	assert.Equal(t, "source-type", b["is_a"][0].Kind)

	require.Len(t, b["designed_by"], 2)
	assert.Equal(t, "Martin Wallace", b["designed_by"][0].Name)
	assert.Equal(t, "Other", b["designed_by"][1].Name)

	require.Len(t, b["is_about"], 1)
	assert.Equal(t, "Brass Birmingham", b["is_about"][0].Name)
	assert.Equal(t, "boardgame", b["is_about"][0].Kind)
}

// TestSourceEdgesBlock_RejectsMalformed enforces the explicit
// shape gate in UnmarshalJSON: anything that isn't an object or a
// list of objects fails decode loudly. Silent fall-through risks
// dropping data — better to surface the malformation at the
// plugin/daemon boundary.
func TestSourceEdgesBlock_RejectsMalformed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		wire string
	}{
		{"scalar_value", `{"is_a": "wikipedia-article"}`},
		{"number_value", `{"is_a": 42}`},
		{"list_of_strings", `{"designed_by": ["Martin Wallace"]}`},
		{"empty_value", `{"is_a": null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b sourceEdgesBlock
			err := json.Unmarshal([]byte(tc.wire), &b)
			assert.Error(t, err, "want decode error on malformed shape: %s", tc.wire)
		})
	}
}

// TestValidateStructured_RejectsLegacyKind pins the post-
// removal: a plugin emitting the pre-ADR-0021 shape (per-kind
// `kind` like `wikipedia-article`) rejects with a clear
// "kind must be `source`" error so the loop fails at the
// boundary rather than silently producing a malformed entity.
func TestValidateStructured_RejectsLegacyKind(t *testing.T) {
	t.Parallel()

	err := validateStructured(&structuredResponse{
		Kind: "wikipedia-article", Name: "Susanna Clarke",
	}, Capabilities{SourceNamespace: "wikipedia"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind=\"wikipedia-article\" not supported")

	err = validateStructured(&structuredResponse{
		Kind: "",
	}, Capabilities{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing kind")
}

// TestValidateStructured_NewShapeRequiresNameAndSourceNamespace
// pins the ADR-0021 shape: `kind: source` requires both a non-
// empty `name` AND a plugin that declared `source_namespace` at
// --init. ID may be omitted (daemon derives via slug).
func TestValidateStructured_NewShapeRequiresNameAndSourceNamespace(t *testing.T) {
	t.Parallel()

	// Happy path: both name + source_namespace present.
	require.NoError(t, validateStructured(&structuredResponse{
		Kind: "source",
		Name: "Brass: Birmingham (2018)",
	}, Capabilities{SourceNamespace: "bgg"}))

	// Missing name.
	err := validateStructured(&structuredResponse{
		Kind: "source",
	}, Capabilities{SourceNamespace: "bgg"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-empty name")

	// Missing source_namespace.
	err = validateStructured(&structuredResponse{
		Kind: "source",
		Name: "Brass: Birmingham (2018)",
	}, Capabilities{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source_namespace")
}

// TestToFetchResult_SourceShapeTranslation walks the Entity
// derivation under the ADR-0021 wire-shape: Kind=="source" +
// Name maps to Entity{Kind=source_namespace, ID=source_namespace+":"+slug.Slug(name)}.
// Edges block translates to CanonicalEdges with `<kind>:<slug>`
// targets and to SourceEdges (the descriptive `{name, kind}`
// pre-slug form). Per the wire contract: "wire-shape source is a
// marker only — the daemon never persists it."
func TestToFetchResult_SourceShapeTranslation(t *testing.T) {
	t.Parallel()

	r := &fetchResponse{
		OK: true,
		Structured: &structuredResponse{
			Kind: "source",
			Name: "Brass: Birmingham (2018)",
			Data: map[string]any{"bgg_id": 224517},
			Edges: sourceEdgesBlock{
				"is_a": {{Name: "bgg-record", Kind: "source-type"}},
				"is_about": {{Name: "Brass Birmingham", Kind: "boardgame"}},
				"designed_by": {{Name: "Martin Wallace", Kind: "person"}},
			},
		},
	}
	out := r.toFetchResult(Capabilities{SourceNamespace: "bgg"})

	require.NotNil(t, out.Entity)
	assert.Equal(t, "bgg", out.Entity.Kind,
		"wire-shape `source` must translate to source_namespace at storage layer")
	assert.Equal(t, "bgg:brass-birmingham-2018", out.Entity.ID,
		"Entity.ID = source_namespace + ':' + slug.Slug(name)")
	assert.Equal(t, "Brass: Birmingham (2018)", out.SourceName)

	// SourceEdges keeps descriptive `{name, kind}` form (pre-slug)
	// for any caller that needs the original names.
	require.Len(t, out.SourceEdges, 3)
	assert.Equal(t, "Martin Wallace", out.SourceEdges["designed_by"][0].Name)

	// CanonicalEdges has the resolved `<kind>:<slug.Slug(name)>`
	// targets the FK + edges-table consume.
	require.Len(t, out.CanonicalEdges, 3)
	targets := make(map[string]string, 3)
	for _, e := range out.CanonicalEdges {
		targets[e.Type] = e.To
	}
	assert.Equal(t, "source-type:bgg-record", targets["is_a"])
	assert.Equal(t, "boardgame:brass-birmingham", targets["is_about"])
	assert.Equal(t, "person:martin-wallace", targets["designed_by"])
}

// TestCapabilitiesSourceNamespaceDecodesFromInit pins the wire
// decode of the new --init field. A plugin declaring
// `source_namespace: "bgg"` round-trips to caps.SourceNamespace
// for the daemon's per-emission slug-derivation pipeline.
func TestCapabilitiesSourceNamespaceDecodesFromInit(t *testing.T) {
	t.Parallel()

	const wire = `{
		"name": "bgg",
		"version": "0.1.0",
		"url_patterns": ["^https://"],
		"entity_kinds": [],
		"edge_kinds": [],
		"source_namespace": "bgg"
	}`
	var caps Capabilities
	require.NoError(t, json.Unmarshal([]byte(wire), &caps))
	assert.Equal(t, "bgg", caps.SourceNamespace)

	// Absence (legacy plugin) decodes to "".
	const legacyWire = `{
		"name": "wikipedia",
		"version": "0.1.0",
		"url_patterns": ["^https://"],
		"entity_kinds": [],
		"edge_kinds": []
	}`
	var legacyCaps Capabilities
	require.NoError(t, json.Unmarshal([]byte(legacyWire), &legacyCaps))
	assert.Empty(t, legacyCaps.SourceNamespace,
		"plugins predating ADR-0021 emit no source_namespace; field decodes to zero value")
}
