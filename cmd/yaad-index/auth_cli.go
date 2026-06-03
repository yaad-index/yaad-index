// Auth CLI subcommands (per yaad-index a prior PR):
//
// - `yaad-index keygen` — generate the RS256 keypair the
// operational config relies on.
// - `yaad-index issue-token` — sign a pair-claim JWT for an
// operator/agent pair. Prints the token on stdout.
//
// Both subcommands resolve `--keys-dir` (or the
// `YAAD_INDEX_KEYS_DIR` env, or the `auth.keys_dir` config field
// — in that precedence order). The default fallback is
// `/etc/yaad-index/keys/` matching the operational deployment
// shape.

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/yaad-index/yaad-index/internal/auth"
	"github.com/yaad-index/yaad-index/internal/clock"
	"github.com/yaad-index/yaad-index/internal/config"
)

// authDefaultKeysDir is the lowest-priority fallback when
// neither the CLI flag, env var, nor config file specifies a
// keys directory. Matches the documented operational deployment
// shape — sibling of `/etc/yaad-index/config.yaml`.
const authDefaultKeysDir = "/etc/yaad-index/keys"

// authDefaultTTL is the issue-token CLI fallback when neither
// the `--ttl` flag nor `auth.default_ttl` is set.
const authDefaultTTL = 90 * 24 * time.Hour

// KeygenCmd implements `yaad-index keygen`. Generates an RS256
// keypair and writes private.pem + public.pem into the resolved
// keys directory. Refuses to overwrite existing files unless
// --force is passed.
type KeygenCmd struct {
	KeysDir string `name:"keys-dir" env:"YAAD_INDEX_KEYS_DIR" help:"directory holding private.pem + public.pem. Defaults to /etc/yaad-index/keys/, falls back through YAAD_INDEX_KEYS_DIR env and auth.keys_dir config."`
	ConfigPath string `name:"config" env:"YAAD_INDEX_CONFIG" default:"~/.config/yaad-index/config.yaml" help:"path to the yaad-index config file (read for auth.keys_dir if --keys-dir / env not set)."`
	Force bool `name:"force" help:"overwrite existing private.pem / public.pem instead of refusing."`
}

func (c *KeygenCmd) Run() error {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: clock.LogTimeAttr}))
	keysDir, err := resolveKeysDir(logger, c.KeysDir, c.ConfigPath)
	if err != nil {
		return err
	}
	if err := auth.GenerateKeypair(keysDir, c.Force); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stderr, "wrote keypair to %s\n", keysDir)
	return nil
}

// IssueTokenCmd implements `yaad-index issue-token`. Signs a
// pair-claim JWT (operator + agent + iat + exp + kid) using the
// RS256 private key under the resolved keys directory. Prints
// the token on stdout — operators redirect into a secrets
// store; nothing else is written there.
type IssueTokenCmd struct {
	Operator string `name:"operator" required:"" help:"the human (resource owner). Stamped on the token's operator claim."`
	Agent string `name:"agent" help:"the agent identity (the actor calling the API). Stamped on the token's sub claim. Required unless --operator-only is set."`
	OperatorOnly bool `name:"operator-only" help:"issue an operator-only token (Subject == Operator). Required for CLI dispatch (yaad-index / ADR-0022 §6 — command-shape inputs reject pair-claim tokens). Sets --agent equal to --operator automatically; mutually exclusive with --agent."`
	OnBehalfOfOperator bool `name:"on-behalf-of-operator" help:"mark this pair-claim token as carrying delegated operator authority (the operator confirmed via the agent skill UI). Classifies as operator-trigger on the unified /v1/fill endpoint (#361 / ADR-0029 §3) without making Subject == Operator. Requires --agent; mutually exclusive with --operator-only."`
	TTL string `name:"ttl" env:"YAAD_INDEX_DEFAULT_TTL" help:"token lifetime (Go time.ParseDuration: 24h, 30m, 2160h). Defaults via auth.default_ttl config, then 90d."`
	KeysDir string `name:"keys-dir" env:"YAAD_INDEX_KEYS_DIR" help:"directory holding private.pem (default /etc/yaad-index/keys/)."`
	ConfigPath string `name:"config" env:"YAAD_INDEX_CONFIG" default:"~/.config/yaad-index/config.yaml" help:"path to the yaad-index config file (read for auth.* defaults if CLI / env not set)."`
}

func (c *IssueTokenCmd) Run() error {
	if c.Operator == "" {
		return errors.New("issue-token: --operator is required")
	}
	if c.OperatorOnly {
		if c.Agent != "" && c.Agent != c.Operator {
			return errors.New("issue-token: --operator-only is mutually exclusive with --agent (operator-only tokens have Subject == Operator by definition)")
		}
		if c.OnBehalfOfOperator {
			return errors.New("issue-token: --on-behalf-of-operator is mutually exclusive with --operator-only (an operator-only token already carries operator authority; delegation is for agent-tier pair-claims)")
		}
		c.Agent = c.Operator
	}
	if c.Agent == "" {
		return errors.New("issue-token: --agent is required (or pass --operator-only)")
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: clock.LogTimeAttr}))
	keysDir, err := resolveKeysDir(logger, c.KeysDir, c.ConfigPath)
	if err != nil {
		return err
	}
	ttl, err := resolveTTL(logger, c.TTL, c.ConfigPath)
	if err != nil {
		return err
	}
	signer, err := auth.LoadSigner(keysDir)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	claim := auth.Claim{
		Subject: c.Agent,
		Operator: c.Operator,
		IssuedAt: now,
		ExpiresAt: now.Add(ttl),
		OperatorDelegated: c.OnBehalfOfOperator,
	}
	token, err := signer.Sign(claim)
	if err != nil {
		return err
	}
	// Token on stdout (single field, no decoration) so operators
	// can pipe it into a secrets store with `... > secret.jwt`.
	// The "wrote token for ..." annotation goes to stderr and is
	// safe to display alongside.
	_, _ = fmt.Fprintln(os.Stdout, token)
	_, _ = fmt.Fprintf(os.Stderr,
		"issued token for operator=%s agent=%s on_behalf_of_operator=%t ttl=%s exp=%s\n",
		c.Operator, c.Agent, c.OnBehalfOfOperator, ttl, claim.ExpiresAt.Format(time.RFC3339))
	return nil
}

// resolveKeysDir walks the precedence chain: CLI flag (or env
// via Kong's `env:` tag — which Kong populates into the same
// field) wins → config file's `auth.keys_dir` next → finally
// the documented `/etc/yaad-index/keys/` default.
//
// Empty config-file path is fine; a broken / non-existent
// config doesn't error out here (the keygen subcommand may run
// before any config exists). The default kicks in.
func resolveKeysDir(logger *slog.Logger, cliFlag, configPath string) (string, error) {
	if cliFlag != "" {
		return cliFlag, nil
	}
	if cfg := loadAuthConfig(logger, configPath); cfg != nil && cfg.KeysDir != "" {
		return cfg.KeysDir, nil
	}
	return authDefaultKeysDir, nil
}

// resolveTTL walks the precedence chain: CLI / env first; config
// `auth.default_ttl` next; the 90-day documented default last.
// Empty / unparseable values fall through to the default rather
// than errorring up front — Kong validates non-empty CLI input
// strictly via tags; the issue-token CLI tolerates the empty
// case explicitly because the env layer can be empty too.
func resolveTTL(logger *slog.Logger, cliFlag, configPath string) (time.Duration, error) {
	if cliFlag != "" {
		ttl, err := time.ParseDuration(cliFlag)
		if err != nil {
			return 0, fmt.Errorf("--ttl: parse %q: %w", cliFlag, err)
		}
		if ttl <= 0 {
			return 0, fmt.Errorf("--ttl: must be positive, got %s", cliFlag)
		}
		return ttl, nil
	}
	if cfg := loadAuthConfig(logger, configPath); cfg != nil && cfg.DefaultTTL != "" {
		ttl, err := time.ParseDuration(cfg.DefaultTTL)
		if err != nil {
			return 0, fmt.Errorf("auth.default_ttl: parse %q: %w", cfg.DefaultTTL, err)
		}
		if ttl <= 0 {
			return 0, fmt.Errorf("auth.default_ttl: must be positive, got %s", cfg.DefaultTTL)
		}
		return ttl, nil
	}
	return authDefaultTTL, nil
}

// loadAuthConfig parses just the auth section out of the
// operational config. Returns nil on missing / invalid config —
// the auth CLIs treat config as the lowest-priority layer; an
// absent config is normal during initial keygen.
func loadAuthConfig(logger *slog.Logger, configPath string) *config.AuthEntry {
	cfg, err := loadConfigOptional(logger, configPath)
	if err != nil || cfg == nil {
		return nil
	}
	return &cfg.Auth
}
