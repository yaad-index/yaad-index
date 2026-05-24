// Package subprocess wraps a plugin binary on disk in the
// plugins.Plugin interface (per ADR-0005's invocation model + ADR-0006's
// config allowlist).
//
// Lifecycle:
// - New(name, path) calls `<path> --init` once, parses the JSON
// capabilities document, and returns a *Plugin ready to dispatch.
// - Match consults the precompiled url_patterns from the
// capabilities document.
// - Fetch spawns `<path>` once per call, writes a JSON request to
// stdin, reads the JSON response from stdout (errors via
// stderr+exit-code), and translates into a plugins.FetchResult.
//
// The binary is invoked subprocess-per-request — no pooling, no
// long-lived stdio (per ADR-0005). A wall-clock timeout caps each
// invocation: 5s for --init, 60s for fetch (operator-overridable via
// PluginEntry.FetchTimeout).
package subprocess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/plugins"
	"github.com/yaad-index/yaad-index/internal/slug"
	"github.com/yaad-index/yaad-index/internal/store"
)

// DefaultInitTimeout caps the `<path> --init` handshake. Plugins
// only print their capabilities document at --init, so the budget
// stays small.
const DefaultInitTimeout = 5 * time.Second

// DefaultFetchTimeout caps each per-request fetch invocation when
// the operator hasn't set `fetch_timeout` on the PluginEntry.
// Sized to cover real upstream work (mailbox crawls, paginated API
// calls) rather than just the synthetic happy-path fixtures. A
// dogfood run of `yaad-gmail fetch` against a 137-envelope inbox
// completed in ~10s, so 60s holds ~6× headroom.
const DefaultFetchTimeout = 60 * time.Second

// FetchTimeoutGrace is the wall-clock window between context-cancel
// (SIGTERM to the plugin) and SIGKILL escalation. Plugins that
// trap SIGTERM use this window to flush bufio.Writers + emit a
// final `_summary` packet so any envelopes mid-write survive the
// daemon-imposed timeout. Plugins that ignore the signal get
// SIGKILL'd after this delay so a hung plugin can't park the
// orchestrator forever.
const FetchTimeoutGrace = 2 * time.Second

// Capabilities is the canonical type from the plugins package, re-
// exposed under this name so existing subprocess-internal callers
// don't need rewriting. The Plugin interface (in plugins/) returns
// the same type, so /v1/kinds can walk every plugin's capabilities
// uniformly without an import cycle through subprocess.
type Capabilities = plugins.Capabilities

// KindSpec is the per-kind metadata. Defined on the plugins package
// alongside Capabilities; aliased here for the same reason.
type KindSpec = plugins.KindSpec

// Plugin is the runtime wrapper. Construct via New (which performs
// the --init handshake); never zero-value-construct.
type Plugin struct {
	name string
	path string
	capabilities Capabilities
	patterns []*regexp.Regexp
	initTimeout time.Duration
	fetchTimeout time.Duration
	logger *slog.Logger
	// configEnv carries pre-formatted "KEY=VALUE" entries from the
	// operator's per-plugin `config:` sub-block per yaad-index #7.
	// Appended to pluginEnv() on every subprocess spawn (--version,
	// --init, fetch, command). Nil when the operator didn't supply
	// a config block for this plugin.
	configEnv []string
}

// Option configures a Plugin at construction.
type Option func(*Plugin)

// WithTimeout sets BOTH the --init and per-Fetch wall-clock timeouts
// to d. Tests use this to pin a uniform short window; production
// code reaches for the split defaults (DefaultInitTimeout /
// DefaultFetchTimeout) via WithInitTimeout / WithFetchTimeout
// instead, since real fetches need more headroom than --init.
func WithTimeout(d time.Duration) Option {
	return func(p *Plugin) {
		p.initTimeout = d
		p.fetchTimeout = d
	}
}

// WithInitTimeout overrides only the --init handshake timeout.
func WithInitTimeout(d time.Duration) Option {
	return func(p *Plugin) { p.initTimeout = d }
}

// WithFetchTimeout overrides only the per-Fetch timeout.
func WithFetchTimeout(d time.Duration) Option {
	return func(p *Plugin) { p.fetchTimeout = d }
}

// WithLogger sets the slog.Logger used to forward plugin stderr on
// the success path (per the source issue). Without this option, stderr on
// success is forwarded via slog.Default(); plugin authors writing
// diagnostic stderr's canonical-axis traces, e.g.)
// surface either way. Passing a logger lets the caller tag plugin
// output with handler context (request id, etc.).
func WithLogger(l *slog.Logger) Option {
	return func(p *Plugin) {
		if l != nil {
			p.logger = l
		}
	}
}

// WithConfigEnv sets pre-formatted "KEY=VALUE" env-var entries the
// daemon derived from the operator's per-plugin `config:` sub-block
// per yaad-index #7. The caller is responsible for shape (use
// config.PluginConfigEnvVars to build the slice); this option
// trusts the input and appends it to every subprocess spawn.
//
// Empty / nil slice → no per-plugin env vars are added (the
// pre-#7 behavior). Calling repeatedly REPLACES the prior config
// rather than appending — daemon construction sets this once per
// plugin from the operator yaml.
func WithConfigEnv(env []string) Option {
	return func(p *Plugin) {
		p.configEnv = append([]string(nil), env...)
	}
}

// VersionCacheKey returns the cache-key prefix of a plugin's
// `--version` output per yaad-index: anything before the
// first `+` separator. The post-`+` portion (typically a git
// short hash like `+deadbeef`) is build metadata that the daemon
// MUST NOT include in the cache key — otherwise every rebuild
// invalidates the cache even when Capabilities haven't changed.
//
// Examples:
//
//	"v0.1.0+deadbeef" → "v0.1.0"
//	"v0.1.0" → "v0.1.0" (bare version, backward-compat)
//	"0.1.0" → "0.1.0" (legacy plugins)
//	"" → "" (defensive)
//
// The function does NOT validate semver shape: a non-conformant
// prefix passes through unchanged so a plugin that emits an
// unusual version string isn't blocked from cache participation.
// Cache compare is a string-equal between two outputs of this
// function; whatever shape both sides agree on works.
func VersionCacheKey(v string) string {
	if idx := strings.Index(v, "+"); idx >= 0 {
		return v[:idx]
	}
	return v
}

// RunVersion executes `<path> --version` with a short wall-clock
// timeout and returns the plugin's version string. Used by the
// cache-aware loader to decide whether a cached capabilities document
// is still valid before deciding to skip --init.
//
// Plugins SHOULD support --version as a cheap mode: just print the
// version (either as a bare string `0.1.0\n` or as a JSON document
// `{"version":"0.1.0"}`) and exit. Both shapes are accepted to keep
// plugin authors flexible. Plugins without --version trip the
// non-zero-exit fall-through, and callers fall through to a full
// --init load.
func RunVersion(ctx context.Context, path string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultInitTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = pluginEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		const peek = 256
		stderrPeek := bytes.TrimSpace(stderr.Bytes())
		if len(stderrPeek) > peek {
			stderrPeek = stderrPeek[:peek]
		}
		return "", fmt.Errorf("--version: %w: %s", err, string(stderrPeek))
	}
	out := bytes.TrimSpace(stdout.Bytes())
	// JSON `{"version": "..."}` first; bare-string fallback. The
	// JSON shape mirrors the rest of the protocol (--init also emits
	// JSON), so plugin authors who already serialize JSON can reuse
	// the marshaler.
	if len(out) > 0 && out[0] == '{' {
		var doc struct {
			Version string `json:"version"`
		}
		if err := json.Unmarshal(out, &doc); err == nil && doc.Version != "" {
			return doc.Version, nil
		}
	}
	return string(out), nil
}

// NewWithCapabilities builds a Plugin from pre-loaded capabilities
// (typically pulled from the store-backed cache). Skips the --init
// handshake — the caller has already vouched that `caps` is current.
// Pre-compiles url_patterns and constructs the Plugin the same way
// New does.
func NewWithCapabilities(name, path string, caps plugins.Capabilities, opts ...Option) (*Plugin, error) {
	if name == "" {
		return nil, errors.New("subprocess.NewWithCapabilities: empty name")
	}
	if path == "" {
		return nil, errors.New("subprocess.NewWithCapabilities: empty path")
	}
	p := &Plugin{
		name: name,
		path: path,
		capabilities: caps,
		initTimeout: DefaultInitTimeout,
		fetchTimeout: DefaultFetchTimeout,
		logger: slog.Default(),
	}
	for _, o := range opts {
		o(p)
	}
	for _, pat := range caps.URLPatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("subprocess(%s): compile url_pattern %q: %w", name, pat, err)
		}
		p.patterns = append(p.patterns, re)
	}
	return p, nil
}

// New runs the binary's --init handshake, parses its capabilities,
// pre-compiles the url_patterns into regexes, and returns the wrapper.
// Returns an error (server fail-fast per ADR-0006) when --init exits
// non-zero, the JSON is malformed, or any pattern fails to compile.
func New(name, path string, opts ...Option) (*Plugin, error) {
	if name == "" {
		return nil, errors.New("subprocess.New: empty name")
	}
	if path == "" {
		return nil, errors.New("subprocess.New: empty path")
	}

	p := &Plugin{
		name: name,
		path: path,
		initTimeout: DefaultInitTimeout,
		fetchTimeout: DefaultFetchTimeout,
		logger: slog.Default(),
	}
	for _, o := range opts {
		o(p)
	}

	caps, err := p.runInit()
	if err != nil {
		return nil, fmt.Errorf("subprocess(%s): --init: %w", name, err)
	}
	p.capabilities = caps

	for _, pat := range caps.URLPatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, fmt.Errorf("subprocess(%s): compile url_pattern %q: %w", name, pat, err)
		}
		p.patterns = append(p.patterns, re)
	}

	return p, nil
}

// Name implements plugins.Plugin.
func (p *Plugin) Name() string { return p.name }

// Capabilities returns the parsed --init document. Used by
// /v1/kinds to advertise the union of entity / edge kinds across all
// registered plugins.
func (p *Plugin) Capabilities() Capabilities { return p.capabilities }

// Match implements plugins.Plugin. Returns true if any precompiled
// url_pattern matches rawURL.
func (p *Plugin) Match(rawURL string) bool {
	for _, re := range p.patterns {
		if re.MatchString(rawURL) {
			return true
		}
	}
	return false
}

// Fetch implements plugins.Plugin. Single-envelope contract: returns
// the first source emission from the plugin's stdout stream and
// drains the rest. Subsequent emissions land on a stop-sentinel and
// are recognized as "extra envelope past the first" in Stream's
// per-envelope callback. Per ADR-0023 this is the URL-shape
// compatibility surface; the N-envelope consumer is Stream.
func (p *Plugin) Fetch(ctx context.Context, rawURL string) (*plugins.FetchResult, error) {
	var first *plugins.FetchResult
	err := p.Stream(ctx, rawURL,
		func(env *plugins.FetchResult) error {
			if first == nil {
				first = env
				return nil
			}
			// Surplus envelopes past the first: a prior PR/a prior PR's
			// drain-and-discard contract preserved at the Fetch
			// surface. The N-envelope consumer (Stream) is the
			// single-envelope wrapper's caller alternative.
			p.logger.Info("plugin emitted extra source envelope past the first; dropped (Plugin.Fetch single-envelope contract)",
				"plugin", p.name,
			)
			return nil
		},
		nil, // onControl: nil → Stream logs control packets internally
	)
	if err != nil {
		return nil, err
	}
	if first == nil {
		// Zero-envelope invocation (silent exit / control-packets-
		// only): surface as nil-entity FetchResult so the tracker's
		// all-empty branch maps it to 404 not_found.
		return &plugins.FetchResult{}, nil
	}
	return first, nil
}

// Stream implements plugins.Plugin. Spawns the binary subprocess-
// per-request, parses its stdout as a stream of JSON values per
// ADR-0023, and dispatches each value through onEnvelope (source
// emissions) or onControl (`_error` / `_summary` packets). When
// onControl is nil, control packets are logged through the plugin's
// logger and the stream continues (the existing a prior PR logging
// behavior).
//
// Stream returns nil on a clean stream close (zero or N envelopes,
// plugin exited zero). A non-nil return means either the subprocess
// exited non-zero, the per-callback signaled abort, or the bytes
// were so opaquely malformed that no envelope was recovered (the
// pre-ADR-0023 "totally garbled response" hard-fail contract).
func (p *Plugin) Stream(ctx context.Context, rawURL string, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
	ctx, cancel := context.WithTimeout(ctx, p.fetchTimeout)
	defer cancel()

	reqBody, err := json.Marshal(fetchRequest{Operation: "ingest", URL: rawURL})
	if err != nil {
		return fmt.Errorf("subprocess(%s): marshal request: %w", p.name, err)
	}

	cmd := exec.CommandContext(ctx, p.path)
	cmd.Env = p.envFor(ctx)
	cmd.Stdin = bytes.NewReader(reqBody)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// On ctx cancel/timeout, ask the plugin to wind down (SIGTERM)
	// instead of the os/exec default (SIGKILL). Plugins that emit
	// envelopes through a bufio.Writer need a moment to flush
	// before their pre-kill stdout reaches our pipe — without the
	// grace, mid-stream timeouts surface `envelopes_committed=0`
	// even when the plugin had pages of in-flight output. WaitDelay
	// caps the grace so a plugin that ignores SIGTERM can't park
	// the daemon: after FetchTimeoutGrace elapses the runtime
	// escalates to SIGKILL and reaps the I/O goroutines.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = FetchTimeoutGrace

	runErr := cmd.Run()

	// Success-path stderr is plugin diagnostic output (per
	//'s canonical-axis traces, e.g.). Forward it
	// through the yaad-index logger so operators can see it via
	// docker logs (the source issue). Emitted regardless of run outcome —
	// non-zero exit's stderr is also valuable for debugging.
	if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
		p.logger.Info("plugin stderr",
			"plugin", p.name,
			"stderr", trimmed)
	}

	// Process the stdout we DID collect even on non-zero exit —
	// per ADR-0023 §recovery, write-as-you-go means envelopes
	// emitted before the crash MUST land. Stream's contract is to
	// dispatch them via onEnvelope before returning the failure
	// error.
	streamErr := p.streamStdout(stdout.Bytes(), onEnvelope, onControl)

	if runErr != nil {
		// Subprocess failure: surface as Stream error AFTER
		// dispatching any pre-crash envelopes through onEnvelope.
		// The caller (tracker) sees committed envelopes via the
		// callback + the failure surface here, and decides whether
		// to mark the ingest as failed or continue.
		const peek = 512
		stderrPeek := bytes.TrimSpace(stderr.Bytes())
		if len(stderrPeek) > peek {
			stderrPeek = stderrPeek[:peek]
		}
		// Distinguish "wall-clock budget exceeded" (operator knob)
		// from generic non-zero exit so the operator-facing error
		// names the knob that fired instead of just "signal: killed".
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("subprocess(%s): fetchTimeout=%s exceeded: %w: %s",
				p.name, p.fetchTimeout, runErr, string(stderrPeek))
		}
		return fmt.Errorf("subprocess(%s): %w: %s", p.name, runErr, string(stderrPeek))
	}

	if streamErr != nil {
		return fmt.Errorf("subprocess(%s): %w", p.name, streamErr)
	}
	return nil
}

// Search implements plugins.Plugin per yaad-index #2. Dispatches a
// subprocess invocation with `{"operation":"search","query":"...",
// "limit":N}` on stdin; expects a single JSON object on stdout with
// `{"ok":bool, "candidates":[{id,label,summary}], "error_message"}`.
//
// Plugins with SupportsSearch=false are never dispatched here by
// the federation handler; if a caller invokes this method anyway
// the plugin's binary may handle the request, return an unsupported
// error, or any other outcome — the daemon doesn't guard the call
// site because the gate lives in the handler.
//
// Errors:
//   - Subprocess failure (non-zero exit, stderr peek wrapped).
//   - Malformed response JSON.
//   - response.ok=false → error wraps response.error_message verbatim.
//
// Context cancellation (the federation handler's per-plugin
// timeout) propagates via exec.CommandContext; the subprocess
// receives SIGKILL on context expiry.
func (p *Plugin) Search(ctx context.Context, query string, limit int) ([]plugins.SearchCandidate, error) {
	reqBody, err := json.Marshal(searchRequest{
		Operation: "search",
		Query:     query,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("subprocess(%s): marshal search request: %w", p.name, err)
	}

	cmd := exec.CommandContext(ctx, p.path)
	cmd.Env = p.envFor(ctx)
	cmd.Stdin = bytes.NewReader(reqBody)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
		p.logger.Info("plugin stderr (search)",
			"plugin", p.name,
			"stderr", trimmed)
	}
	if runErr != nil {
		const peek = 512
		stderrPeek := bytes.TrimSpace(stderr.Bytes())
		if len(stderrPeek) > peek {
			stderrPeek = stderrPeek[:peek]
		}
		return nil, fmt.Errorf("subprocess(%s): search exit non-zero: %w: %s",
			p.name, runErr, string(stderrPeek))
	}

	var resp searchResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		const peek = 256
		out := bytes.TrimSpace(stdout.Bytes())
		if len(out) > peek {
			out = out[:peek]
		}
		return nil, fmt.Errorf("subprocess(%s): parse search response: %w: %s",
			p.name, err, string(out))
	}
	if !resp.OK {
		msg := resp.ErrorMessage
		if msg == "" {
			msg = "(no error_message)"
		}
		return nil, fmt.Errorf("subprocess(%s): search ok=false: %s", p.name, msg)
	}
	return resp.Candidates, nil
}

// streamStdout walks the plugin's stdout buffer per ADR-0023,
// dispatching each JSON value to the right callback. See
// scanResponse's godoc for the line-shape and resilience contracts;
// streamStdout is the multi-envelope generalization that delivers
// envelopes via callback instead of buffering the first.
func (p *Plugin) streamStdout(stdoutBytes []byte, onEnvelope plugins.EnvelopeFunc, onControl plugins.ControlFunc) error {
	dec := json.NewDecoder(bytes.NewReader(stdoutBytes))

	var (
		anyValueSeen bool
		valueIndex int
	)

	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if anyValueSeen {
				p.logger.Warn("plugin stdout decode error past first value; returning values collected so far",
					"plugin", p.name,
					"value_index", valueIndex+1,
					"err", err,
				)
				break
			}
			return fmt.Errorf("decode response: %w", err)
		}
		valueIndex++
		anyValueSeen = true

		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil {
			p.logger.Warn("plugin emitted non-object JSON value; skipped",
				"plugin", p.name,
				"value_index", valueIndex,
				"err", err,
				"value_peek", peekBytes(raw, 200),
			)
			continue
		}

		if errPayload, ok := top["_error"]; ok {
			if cbErr := p.dispatchErrorPacket(valueIndex, errPayload, onControl); cbErr != nil {
				return cbErr
			}
			continue
		}
		if sumPayload, ok := top["_summary"]; ok {
			if cbErr := p.dispatchSummaryPacket(valueIndex, sumPayload, onControl); cbErr != nil {
				return cbErr
			}
			continue
		}

		// Source emission: decode into the existing fetchResponse
		// shape, validate, translate, deliver to onEnvelope.
		var resp fetchResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			p.logger.Warn("plugin emitted source value that failed envelope decode; skipped",
				"plugin", p.name,
				"value_index", valueIndex,
				"err", err,
			)
			continue
		}
		if !resp.OK {
			// ok=false on the FIRST envelope is a hard failure
			// (existing pre-ADR-0023 contract). Per-envelope
			// ok=false on a later envelope is treated the same —
			// the plugin emits a control packet for that case.
			return fmt.Errorf("plugin reported ok=false")
		}
		if resp.Structured != nil {
			if err := validateStructured(resp.Structured, p.capabilities); err != nil {
				// Validation failure on the wire shape: same
				// as pre-a prior PR — surface as an invocation
				// error.
				return err
			}
		}
		if onEnvelope != nil {
			if cbErr := onEnvelope(resp.toFetchResult(p.capabilities)); cbErr != nil {
				return cbErr
			}
		}
	}
	return nil
}

// dispatchErrorPacket decodes an `_error` payload and either passes
// it to onControl (when the caller wired a handler) or logs it via
// the plugin's logger (preserving a prior PR's default behavior).
func (p *Plugin) dispatchErrorPacket(valueIndex int, payload json.RawMessage, onControl plugins.ControlFunc) error {
	var ep errorPacket
	if err := json.Unmarshal(payload, &ep); err != nil {
		p.logger.Warn("plugin emitted _error value with non-decodable payload",
			"plugin", p.name,
			"value_index", valueIndex,
			"err", err,
			"payload_peek", peekBytes(payload, 200),
		)
		return nil
	}
	if onControl != nil {
		return onControl(plugins.ControlPacket{
			Kind: plugins.ControlPacketError,
			ErrorSlug: ep.Slug,
			ErrorKind: ep.Kind,
			ErrorMessage: ep.Message,
		})
	}
	p.logger.Warn("plugin emitted _error value",
		"plugin", p.name,
		"value_index", valueIndex,
		"slug", ep.Slug,
		"kind", ep.Kind,
		"message", ep.Message,
	)
	return nil
}

// dispatchSummaryPacket decodes a `_summary` payload and either
// passes it to onControl or logs it. Same shape as
// dispatchErrorPacket.
func (p *Plugin) dispatchSummaryPacket(valueIndex int, payload json.RawMessage, onControl plugins.ControlFunc) error {
	var sp summaryPacket
	if err := json.Unmarshal(payload, &sp); err != nil {
		p.logger.Warn("plugin emitted _summary value with non-decodable payload",
			"plugin", p.name,
			"value_index", valueIndex,
			"err", err,
			"payload_peek", peekBytes(payload, 200),
		)
		return nil
	}
	if onControl != nil {
		return onControl(plugins.ControlPacket{
			Kind: plugins.ControlPacketSummary,
			Ingested: sp.Ingested,
			Errors: sp.Errors,
			DurationMs: sp.DurationMs,
		})
	}
	p.logger.Info("plugin emitted _summary value",
		"plugin", p.name,
		"value_index", valueIndex,
		"ingested", sp.Ingested,
		"errors", sp.Errors,
		"duration_ms", sp.DurationMs,
	)
	return nil
}

// validateStructured enforces the ADR-0021 response-shape contract
// before translation. Every plugin emits `kind: "source"` + a
// non-empty `name`, and must declare `source_namespace` in its
// capabilities so the daemon can derive
// `<source_namespace>:<slug.Slug(name)>` for the source-node ID.
//
// The pre-ADR-0021 shape (plugin-formed `id` + per-kind `kind`)
// was removed in; a plugin still emitting that shape rejects
// here with a clear "kind must be `source`" error so the loop
// breaks at the boundary rather than silently producing a
// malformed entity.
func validateStructured(s *structuredResponse, caps Capabilities) error {
	if s.Kind == "" {
		return fmt.Errorf("response.structured missing kind")
	}
	if s.Kind != universalSourceKind {
		return fmt.Errorf("response.structured.kind=%q not supported; plugins must emit `kind: %q` per ADR-0021 (legacy ADR-0008 shape removed in)",
			s.Kind, universalSourceKind)
	}
	if s.Name == "" {
		return fmt.Errorf("response.structured.kind=%q requires non-empty name (ADR-0021)", universalSourceKind)
	}
	if caps.SourceNamespace == "" {
		return fmt.Errorf("response.structured.kind=%q requires plugin to declare source_namespace in capabilities (ADR-0021)", universalSourceKind)
	}
	return nil
}

// universalSourceKind is the system-reserved entity kind plugins
// emit for source-shape nodes under ADR-0021. The daemon treats
// any node with this kind as a source-shape emission and routes
// vault path / ID derivation via the plugin's source_namespace
// capability.
const universalSourceKind = "source"

// runInit executes the binary with `--init`, reads its JSON
// capabilities document from stdout. Subject to the same wall-clock
// timeout as Fetch.
func (p *Plugin) runInit() (Capabilities, error) {
	ctx, cancel := context.WithTimeout(context.Background(), p.initTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.path, "--init")
	// --init runs ONCE per plugin binary per ADR-0028 §2 (cache is
	// plugin-scoped); the per-call ExtraEnv path is for per-instance
	// invocations (Stream/Search). envFor with a background ctx
	// strips any caller-side ExtraEnv that might be in scope.
	cmd.Env = p.envFor(context.Background())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		stderrPeek := bytes.TrimSpace(stderr.Bytes())
		const peek = 512
		if len(stderrPeek) > peek {
			stderrPeek = stderrPeek[:peek]
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Capabilities{}, fmt.Errorf("initTimeout=%s exceeded: %w: %s",
				p.initTimeout, err, string(stderrPeek))
		}
		return Capabilities{}, fmt.Errorf("%w: %s", err, string(stderrPeek))
	}

	// Forward --init success-path stderr (the source issue) — same rule
	// as Fetch: plugin diagnostic output deserves visibility, quiet
	// plugins stay quiet.
	if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
		p.logger.Info("plugin stderr",
			"plugin", p.name,
			"phase", "init",
			"stderr", trimmed)
	}

	var caps Capabilities
	if err := json.Unmarshal(stdout.Bytes(), &caps); err != nil {
		return Capabilities{}, fmt.Errorf("decode --init: %w", err)
	}

	// ADR-0016 §2 forbids plugins from declaring `instruction` in
	// canonical_kinds_extras (instruction is operator-only end-to-
	// end). Go's JSON decoder silently drops unknown fields, so
	// our typed Capabilities struct never sees an instruction
	// field anyway — but per the ADR we WARN explicitly when a
	// plugin attempts the forbidden field, naming the plugin and
	// kind so the plugin author can fix their bug.
	//
	// Detection: scan the raw JSON for any
	// `canonical_kinds_extras.<kind>.instruction` keys.
	rejectPluginInstructionFields(p.logger, p.name, stdout.Bytes())

	// ADR-0019 step 3: typed-gap validation. Mirror the operator-
	// config validation from yaad-index — a plugin emitting an
	// invalid gap shape (bad fill_strategy, malformed range, enum
	// without values) fails server start so the plugin author sees
	// the typo before any agent or operator hits it.
	for kind, extras := range caps.CanonicalKindsExtras {
		for field, spec := range extras.Gaps {
			gapPath := fmt.Sprintf("%s.canonical_kinds_extras.%s.gaps.%s",
				p.name, kind, field)
			if err := spec.Validate(gapPath); err != nil {
				return Capabilities{}, fmt.Errorf("plugin %s --init: %w", p.name, err)
			}
		}
	}

	return caps, nil
}

// errorPacket is the wire shape for a `_error` control packet per
// ADR-0023 §2. `slug` is optional (the plugin may not have a slug
// to attribute the error to — e.g. a fetch that failed before any
// envelope was constructed).
type errorPacket struct {
	Slug string `json:"slug,omitempty"`
	Kind string `json:"kind"`
	Message string `json:"message"`
}

// summaryPacket is the wire shape for a `_summary` control packet
// per ADR-0023 §2.
type summaryPacket struct {
	Ingested int `json:"ingested"`
	Errors int `json:"errors"`
	DurationMs int `json:"duration_ms"`
}

// peekBytes returns up to n leading bytes of b, plus a trailing
// "…(N more)" annotation when truncated. Used in WARN logs so the
// operator can grep for malformed-line contents without filling
// logs with multi-MiB envelopes.
func peekBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return fmt.Sprintf("%s…(%d more bytes)", string(b[:n]), len(b)-n)
}

// rejectPluginInstructionFields scans the plugin's --init JSON for
// any `canonical_kinds_extras.<kind>.instruction` field per
// ADR-0016 §2 and emits a WARN naming the plugin + kind. The
// field is silently dropped by the typed-struct decode path; this
// scanner exists only to surface the forbidden attempt to the
// operator's logs so the plugin author can be told.
func rejectPluginInstructionFields(logger *slog.Logger, pluginName string, body []byte) {
	if logger == nil {
		logger = slog.Default()
	}
	var raw struct {
		CanonicalKindsExtras map[string]map[string]json.RawMessage `json:"canonical_kinds_extras"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		// Decode error — the strict decode in runInit's first pass
		// already errored out; we won't reach here in practice.
		return
	}
	for kind, fields := range raw.CanonicalKindsExtras {
		if _, hasInstr := fields["instruction"]; hasInstr {
			logger.Warn("plugin attempted to set instruction in canonical_kinds_extras; ignored — instruction is operator-only per ADR-0016 §2",
				"plugin", pluginName, "kind", kind)
		}
	}
}

// --- request / response wire shapes ---

type fetchRequest struct {
	Operation string `json:"operation"`
	URL string `json:"url"`
}

// searchRequest is the subprocess-stdin shape per yaad-index #2:
// operator/agent query → plugin candidate list. Sent on the same
// stdin channel as fetchRequest; plugins dispatch on the
// `operation` field.
type searchRequest struct {
	Operation string `json:"operation"`
	Query     string `json:"query"`
	Limit     int    `json:"limit"`
}

// searchResponse is the subprocess-stdout shape returned by a
// search-supporting plugin. Single JSON object (no NDJSON wrap —
// search isn't streaming-shaped). On failure ok=false +
// error_message carries the plugin-side reason; the daemon
// surfaces it in the per_plugin_status block of the federated
// /v1/search/upstream response.
type searchResponse struct {
	OK           bool                       `json:"ok"`
	Candidates   []plugins.SearchCandidate  `json:"candidates,omitempty"`
	ErrorMessage string                     `json:"error_message,omitempty"`
}

// fetchResponse mirrors the ADR-0005 plugin response (lines 119–139).
// Optional fields use pointers / omitempty so we can distinguish
// "absent" from "empty".
type fetchResponse struct {
	OK bool `json:"ok"`
	Structured *structuredResponse `json:"structured,omitempty"`
	Edges []edgeResponse `json:"edges,omitempty"`
	RawContent string `json:"raw_content,omitempty"`
	// raw_content_truncated is plugin-side optional; subprocess.Fetch
	// surfaces it on the FetchResult but doesn't error if absent.
	RawContentTruncated bool `json:"raw_content_truncated,omitempty"`
	// Gaps is a {field-name → description} object (per ADR-0002
	// universal-state amendment). The description is for the agent's
	// AI doing the fill; the field name is what gets validated when
	// the agent POSTs /v1/entities/{id}/fill.
	Gaps map[string]string `json:"gaps,omitempty"`
	// Options is the disambiguation candidate map, keyed by the
	// plugin's canonical id (per ADR-0006). Mutually expected with
	// `structured`: a plugin emits one or the other, never both. The
	// tracker decides the wire-level `state` from which is populated.
	Options map[string]optionJSON `json:"options,omitempty"`
	// Notations is the list of every input form the plugin knows
	// resolves to this entity (per yaad-index the source issue a prior PR).
	// The orchestrator (a prior PR) writes these to entity_notations
	// after a successful Fetch so subsequent ingests of an
	// equivalent form short-circuit on the cache. Empty / absent
	// surfaces as a nil slice on FetchResult; existing plugins
	// predating the source issue emit no `notations` field at all and
	// land here as nil — backward compatible.
	Notations []string `json:"notations,omitempty"`
	// Aliases is the list of alternative labels the plugin knows
	// for this entity (per yaad-index the source issue). Two shapes
	// coexist in the flat list — bare strings for wikilink
	// rendering, `<edge-type>: <label>` prefixes for typed
	// reverse-lookup. The orchestrator dedupes against ADR-0011's
	// title-synthesized alias at vault-write time. Empty / absent
	// → nil slice on FetchResult; plugins predating the source issue
	// emit no `aliases` field at all (current behavior preserved).
	Aliases []string `json:"aliases,omitempty"`
	// CacheTTLSeconds is the optional per-fetch cache TTL override
	// (per yaad-index). Pointer-shape on the wire so absent /
	// explicit-zero / positive / negative are all distinguishable.
	// JSON null (or omitted) → no override; the orchestrator's
	// resolveCacheTTL falls through to plugin-level / global-level.
	CacheTTLSeconds *int `json:"cache_ttl_seconds,omitempty"`
	// Attachments is the optional list of binary attachments the
	// plugin emits alongside the structured Entity (per ADR-0014).
	// Each entry is a `{role, uri, extension}` triple; the
	// orchestrator dispatches on URI scheme and writes resolved
	// bytes to `<vault>/<kind>/<local-id>.<role>.<extension>`.
	// Empty / absent → preserves any existing on-disk attachments
	// per ADR-0014 §4 (UGC-shape silent-preserves contract).
	Attachments []attachmentResponse `json:"attachments,omitempty"`
}

// attachmentResponse is the wire shape for a single ADR-0014
// attachment emission. Mirrors plugins.Attachment field-for-field
// (kept separate so the JSON tags don't bleed into the public type).
type attachmentResponse struct {
	Role string `json:"role"`
	URI string `json:"uri"`
	Extension string `json:"extension"`
}

type optionJSON struct {
	Label string `json:"label"`
	Summary string `json:"summary,omitempty"`
}

// structuredResponse is the ADR-0021 wire shape for a plugin's
// fetched-entity response. Per the post- contract, every
// plugin emits `kind: "source"` + descriptive `name` + `data` +
// the polymorphic `edges` block; the daemon's slug utility
// derives the source-node ID at toFetchResult time
// (`<source_namespace>:<slug.Slug(name)>`).
//
// The pre-ADR-0021 shape (plugin-formed `id` + per-kind `kind` +
// top-level `canonical_entities` / `canonical_edges` lists) was
// retired in once the in-fleet plugins migrated.
type structuredResponse struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	Data map[string]any `json:"data,omitempty"`
	Edges sourceEdgesBlock `json:"edges,omitempty"`
	Provenance []provenanceJSONEntry `json:"provenance,omitempty"`
}

// sourceEdgeTargetJSON is the wire shape for one entry in
// structuredResponse.Edges (per ADR-0021). Plugins emit `{name,
// kind}` only — the daemon does the slug derivation.
type sourceEdgeTargetJSON struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// sourceEdgesBlock decodes the polymorphic `edges` map: each
// value is either a single sourceEdgeTargetJSON object OR a list
// of them. Stored uniformly as `[]sourceEdgeTargetJSON` so
// downstream code never branches on shape.
//
// The shape branch is taken by inspecting the first non-whitespace
// byte of the raw value — `{` → object, `[` → list. No fallback
// "try object then list" because that risks accepting malformed
// shapes silently (an empty array `[]` would round-trip through
// the object decode without error and lose data).
type sourceEdgesBlock map[string][]sourceEdgeTargetJSON

func (b *sourceEdgesBlock) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode edges block: %w", err)
	}
	out := make(sourceEdgesBlock, len(raw))
	for k, v := range raw {
		trimmed := bytes.TrimSpace(v)
		if len(trimmed) == 0 {
			return fmt.Errorf("edges[%q]: empty value", k)
		}
		switch trimmed[0] {
		case '{':
			var single sourceEdgeTargetJSON
			if err := json.Unmarshal(v, &single); err != nil {
				return fmt.Errorf("edges[%q]: decode object: %w", k, err)
			}
			out[k] = []sourceEdgeTargetJSON{single}
		case '[':
			var list []sourceEdgeTargetJSON
			if err := json.Unmarshal(v, &list); err != nil {
				return fmt.Errorf("edges[%q]: decode list: %w", k, err)
			}
			out[k] = list
		default:
			return fmt.Errorf("edges[%q]: must be {name, kind} object or list thereof", k)
		}
	}
	*b = out
	return nil
}

type provenanceJSONEntry struct {
	Source string `json:"source"`
	FetchedAt string `json:"fetched_at,omitempty"`
	FilledAt string `json:"filled_at,omitempty"`
	OK bool `json:"ok"`
	Error string `json:"error,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

type edgeResponse struct {
	Type string `json:"type"`
	From string `json:"from"`
	To string `json:"to"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// toFetchResult translates the wire-shape response into the
// plugins.FetchResult the registry surfaces. Time strings parse as
// RFC3339 (with RFC3339Nano fallback) — matches the store's parser.
//
// State is implicit per ADR-0006: a plugin emits ONE of structured /
// options (or nothing, which the tracker maps to 404 not_found). When
// options is set, no entity is constructed — the tracker emits the
// 200 disambiguation response shape with the options list.
//
// The capabilities argument is consulted only for the ADR-0021
// universal-source-shape path (Kind=="source") — daemon needs the
// plugin's `source_namespace` to derive `<namespace>:<slug>` IDs.
// Validation already enforced at the Fetch boundary that this
// shape is paired with a non-empty source_namespace.
func (r *fetchResponse) toFetchResult(caps Capabilities) *plugins.FetchResult {
	out := &plugins.FetchResult{
		RawContent: r.RawContent,
		RawContentTruncated: r.RawContentTruncated,
	}
	if len(r.Gaps) > 0 {
		out.Gaps = make(map[string]string, len(r.Gaps))
		for k, v := range r.Gaps {
			out.Gaps[k] = v
		}
	}

	// Source-shape edges — typed relationships alongside the entity
	// (e.g. boardgame → designer). Today the tracker doesn't persist
	// these (per docs/plugin-flow.md §4); copying onto the FetchResult
	// surfaces them to the registry consumer that DOES — when a future
	// PR wires plugin-emitted source-shape edges into ingest, the
	// data is already on FetchResult.Edges. Wire-shape was orphaned
	// legacy (declared, never copied).
	if len(r.Edges) > 0 {
		out.Edges = make([]store.Edge, len(r.Edges))
		for i, e := range r.Edges {
			out.Edges[i] = store.Edge{
				Type: e.Type,
				From: e.From,
				To: e.To,
				Metadata: e.Metadata,
			}
		}
	}

	// Notations (per yaad-index the source issue a prior PR). Copy through; the
	// orchestrator (a prior PR) decides what to do with them. Empty/absent
	// surfaces as nil — backwards compatible with plugins that don't
	// emit the field yet.
	if len(r.Notations) > 0 {
		out.Notations = append([]string(nil), r.Notations...)
	}

	// Aliases (per yaad-index the source issue a prior PR). Same defensive copy
	// shape as Notations — orchestrator dedupes against the ADR-
	// 0011 title-synthesized alias at vault-write time. Empty
	// surfaces as nil; plugins not yet emitting aliases see no
	// behavior change.
	if len(r.Aliases) > 0 {
		out.Aliases = append([]string(nil), r.Aliases...)
	}

	// Disambiguation: copy options across; no Entity, no Provenance
	// (the fetch is ephemeral — the caller re-invokes ingest with one
	// option id via the plugin's shorthand input, which produces real
	// provenance on that round).
	if len(r.Options) > 0 {
		out.Options = make(map[string]plugins.DisambiguationOption, len(r.Options))
		for id, o := range r.Options {
			out.Options[id] = plugins.DisambiguationOption{
				Label: o.Label,
				Summary: o.Summary,
			}
		}
		return out
	}

	// Structured (complete or needs_fill): build the entity + provenance.
	// Plugins that emit neither structured nor options produce an
	// all-empty FetchResult, which the tracker maps to 404 not_found.
	if r.Structured == nil {
		return out
	}

	// ADR-0021 universal-source-shape translation. validateStructured
	// already enforced Kind=="source" + non-empty Name + plugin-
	// declared SourceNamespace. The daemon's slug utility derives
	// both the source-node ID and the canonical-label edge targets
	// in the `edges` block. Wire-shape "source" is a marker only —
	// at storage layer Entity.Kind becomes the plugin's
	// source_namespace, so multi-plugin source emissions don't
	// collide in a single `source/` directory.
	nameSlug := slug.Slug(r.Structured.Name)
	out.Entity = &store.Entity{
		ID: caps.SourceNamespace + ":" + nameSlug,
		Kind: caps.SourceNamespace,
		Data: r.Structured.Data,
	}
	out.SourceName = r.Structured.Name
	if len(r.Structured.Edges) > 0 {
		out.SourceEdges = make(map[string][]plugins.SourceEdgeTarget, len(r.Structured.Edges))
		out.CanonicalEdges = make([]*store.Edge, 0, len(r.Structured.Edges))
		for edgeType, targets := range r.Structured.Edges {
			translated := make([]plugins.SourceEdgeTarget, 0, len(targets))
			for _, t := range targets {
				translated = append(translated, plugins.SourceEdgeTarget{
					Name: t.Name,
					Kind: t.Kind,
				})
				out.CanonicalEdges = append(out.CanonicalEdges, &store.Edge{
					Type: edgeType,
					From: out.Entity.ID,
					To: t.Kind + ":" + slug.Slug(t.Name),
				})
			}
			out.SourceEdges[edgeType] = translated
		}
	}
	out.Provenance = make([]store.ProvenanceEntry, 0, len(r.Structured.Provenance))
	for _, p := range r.Structured.Provenance {
		entry := store.ProvenanceEntry{
			Source: p.Source,
			OK: p.OK,
			Error: p.Error,
			ErrorMessage: p.ErrorMessage,
		}
		if p.FetchedAt != "" {
			if t, err := parsePluginTime(p.FetchedAt); err == nil {
				entry.FetchedAt = &t
			}
		}
		if p.FilledAt != "" {
			if t, err := parsePluginTime(p.FilledAt); err == nil {
				entry.FilledAt = &t
			}
		}
		out.Provenance = append(out.Provenance, entry)
	}
	if len(out.Provenance) == 0 {
		// Defensive: even if a plugin omits provenance, the tracker
		// expects at least one entry to record the fetch attempt.
		// Synthesize one with a now-stamp; the source field is
		// derived from the plugin name (caller's responsibility, but
		// unreachable in practice without the structured.provenance
		// section the protocol requires).
		now := time.Now().UTC()
		out.Provenance = []store.ProvenanceEntry{{
			Source: "subprocess:unknown",
			FetchedAt: &now,
			OK: true,
		}}
	}
	// Per-fetch cache TTL override (per yaad-index). Pointer
	// passed verbatim — the orchestrator interprets nil vs *=0 vs
	// non-zero per the sentinel rules.
	out.CacheTTLSeconds = r.CacheTTLSeconds

	// Attachments (per ADR-0014). Defensive copy onto a stable
	// plugins.Attachment slice — the orchestrator's attachment
	// dispatcher is what validates role / extension / scheme; this
	// layer just translates the wire shape.
	if len(r.Attachments) > 0 {
		out.Attachments = make([]plugins.Attachment, len(r.Attachments))
		for i, a := range r.Attachments {
			out.Attachments[i] = plugins.Attachment{
				Role: a.Role,
				URI: a.URI,
				Extension: a.Extension,
			}
		}
	}
	return out
}

func parsePluginTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// stagingDir is the operator-configured plugin_staging_dir
// (per ADR-0014 §6) propagated to subprocess plugins via
// YAAD_PLUGIN_STAGING_DIR. Set once at server boot from main.go;
// readers acquire under atomic.Value's load semantics so the
// pluginEnv path is lock-free.
//
// Same shape as clock.loc: pre-seed the variable to avoid an
// init() ordering race; default returned by stagingDirOrDefault
// when unset.
var stagingDir atomic.Value // string

// SetStagingDir pins the operator-configured plugin staging dir.
// Called once at server startup from main.go after loading +
// validating the config; idempotent across re-calls. Tests may
// call directly to exercise non-default paths. Mirrors
// clock.SetLocation's contract.
//
// Empty string is treated as "reset to /tmp" — the next plugin
// spawn carries the package's DefaultPluginStagingDir.
func SetStagingDir(d string) {
	stagingDir.Store(d)
}

// DefaultPluginStagingDir is the fallback YAAD_PLUGIN_STAGING_DIR
// value emitted when SetStagingDir hasn't been called (or was
// called with empty). `/tmp` matches both the daemon-side default
// and the SDK-side `attach.DefaultStagingDir`. Tests + dev
// binaries that don't configure a vault see this.
const DefaultPluginStagingDir = "/tmp"

func stagingDirOrDefault() string {
	if v, ok := stagingDir.Load().(string); ok && v != "" {
		return v
	}
	return DefaultPluginStagingDir
}

// envFor returns this Plugin's subprocess env for a per-call
// invocation: the global pluginEnv() (parent env + YAAD_TIMEZONE
// + YAAD_PLUGIN_STAGING_DIR) extended with the per-plugin
// configEnv from registry-build time and then by any per-call
// extra env stamped via plugins.WithExtraEnv per ADR-0028 §3 +
// §4 (Cut 4). Precedence (last-wins) — parent env, then
// registered configEnv, then per-call ExtraEnv — so per-call
// values override registered values that override shell.
//
// Per-call ExtraEnv is the surface multi-instance dispatch uses
// to pass the active instance's YAAD_PLUGIN_CONFIG +
// InstanceEntry.Env entries into the subprocess. Pre-Cut-4
// configEnv stays for plugins that don't carry per-instance
// config (the registration path still stamps the legacy config
// at build time as a default; per-call ExtraEnv overrides per
// invocation).
func (p *Plugin) envFor(ctx context.Context) []string {
	base := pluginEnv()
	extra := plugins.ExtraEnvFromContext(ctx)
	if len(p.configEnv) == 0 && len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(p.configEnv)+len(extra))
	out = append(out, base...)
	out = append(out, p.configEnv...)
	out = append(out, extra...)
	return out
}

// pluginEnv returns the parent process environment extended with
// yaad-index-specific variables that subprocess plugins read at
// startup:
//
// - YAAD_TIMEZONE — the operator-configured IANA timezone (per
// yaad-index PR-D). Plugins use this when stamping
// `provenance.fetched_at` so the operator-TZ surface stays
// end-to-end consistent.
//
// - YAAD_PLUGIN_STAGING_DIR — the operator-configured directory
// plugins MUST stage `file://` attachments under (per ADR-0014
// §6). The daemon's path-traversal guard rejects file URIs
// whose resolved path isn't a strict descendant of this dir;
// plugins read it via the SDK helper at
// pkg/plugin/attach/StagingDir().
//
// Plugins predating either var ignore them — they're append-only
// extensions to the parent env, so plugins that never read them
// continue working unchanged.
func pluginEnv() []string {
	return append(os.Environ(),
		"YAAD_TIMEZONE="+clock.Location().String(),
		"YAAD_PLUGIN_STAGING_DIR="+stagingDirOrDefault(),
	)
}

// Compile-time assertion that *Plugin satisfies plugins.Plugin.
var _ plugins.Plugin = (*Plugin)(nil)
