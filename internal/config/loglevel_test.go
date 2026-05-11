package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseLogLevel_Cases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in string
		want slog.Level
	}{
		{"empty defaults to info", "", slog.LevelInfo},
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"warn", "warn", slog.LevelWarn},
		{"warning alias", "warning", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"err alias", "err", slog.LevelError},
		{"case-insensitive Debug", "Debug", slog.LevelDebug},
		{"case-insensitive ERROR", "ERROR", slog.LevelError},
		{"whitespace-trimmed", " warn ", slog.LevelWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseLogLevel(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestParseLogLevel_RejectsUnknown(t *testing.T) {
	t.Parallel()

	cases := []string{"trace", "verbose", "fatal", "panic", "12", "off", "DEBUGY"}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := ParseLogLevel(in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "log_level")
			assert.Contains(t, err.Error(), in,
				"err should name the bad value so operators can grep their config for it")
		})
	}
}

// TestParseLogLevel_RuntimeFiltering pins the wiring contract: a slog
// handler constructed with the parsed Level filters records exactly
// at the threshold the operator asked for. This is the property the
// dispatch's runtime acceptance criteria rely on (debug visible at
// debug; info+ hidden at warn+; etc.).
func TestParseLogLevel_RuntimeFiltering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		level string
		wantDebugSeen bool
		wantInfoSeen bool
		wantWarnSeen bool
		wantErrorSeen bool
	}{
		{"debug", true, true, true, true},
		{"info", false, true, true, true},
		{"warn", false, false, true, true},
		{"error", false, false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.level, func(t *testing.T) {
			t.Parallel()
			level, err := ParseLogLevel(tc.level)
			require.NoError(t, err)

			var buf bytes.Buffer
			h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})
			lg := slog.New(h)
			lg.Debug("dbg-msg")
			lg.Info("info-msg")
			lg.Warn("warn-msg")
			lg.Error("err-msg")

			out := buf.String()
			assert.Equal(t, tc.wantDebugSeen, strings.Contains(out, "dbg-msg"),
				"debug visibility at level=%s", tc.level)
			assert.Equal(t, tc.wantInfoSeen, strings.Contains(out, "info-msg"),
				"info visibility at level=%s", tc.level)
			assert.Equal(t, tc.wantWarnSeen, strings.Contains(out, "warn-msg"),
				"warn visibility at level=%s", tc.level)
			assert.Equal(t, tc.wantErrorSeen, strings.Contains(out, "err-msg"),
				"error visibility at level=%s", tc.level)
		})
	}
}
