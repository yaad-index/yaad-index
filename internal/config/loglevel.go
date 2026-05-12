package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// DefaultLogLevel is the level applied when `log_level:` is omitted
// from the config (or the config file itself is missing). Matches
// the historical hardcoded value in cmd/yaad-index, so adding the
// new key with no value is observationally equivalent to the
// legacy behavior.
const DefaultLogLevel = slog.LevelInfo

// ParseLogLevel maps the operator-supplied `log_level:` string to a
// slog.Level. Empty input returns DefaultLogLevel (info). Accepted
// values: "debug", "info", "warn", "error". Case-insensitive +
// whitespace-trimmed. Anything else returns a non-nil error so
// Config.Validate fails fast at server start (per ADR-0006:
// operators must notice broken configs).
func ParseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return DefaultLogLevel, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error", "err":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log_level %q: must be one of debug, info, warn, error", raw)
	}
}
