// Package logging configures the process-wide log/slog logger.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Environment overrides; they take precedence over config values.
const (
	EnvLevel  = "FLEET_LOG_LEVEL"
	EnvFormat = "FLEET_LOG_FORMAT"
)

// New builds a logger writing to w. level is debug|info|warn|error (default
// info), format is text|json (default text). FLEET_LOG_LEVEL and
// FLEET_LOG_FORMAT override the arguments when set.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	if v := os.Getenv(EnvLevel); v != "" {
		level = v
	}
	if v := os.Getenv(EnvFormat); v != "" {
		format = v
	}

	var lvl slog.Level
	switch strings.ToLower(level) {
	case "", "info":
		lvl = slog.LevelInfo
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("invalid log level %q (want debug|info|warn|error)", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "", "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	default:
		return nil, fmt.Errorf("invalid log format %q (want text|json)", format)
	}
}
