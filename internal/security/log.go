package security

import (
	"log/slog"
	"os"
	"strings"
	"sync"
)

// log returns the slog logger for the security package. Centralised so the
// "[security]" prefix and level filtering match across files.
//
// We keep a package-private singleton rather than wiring slog from main
// because the security package is also used by tests that don't go through
// the CLI. Default level is Info; SetLevel changes it process-wide.
var (
	logMu       sync.Mutex
	loggerLevel = new(slog.LevelVar)
	logger      = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: loggerLevel}))
)

// SetLevel adjusts the security package logger level. Accepts the same
// strings as the YAML config: debug | info | warn | error.
func SetLevel(name string) {
	logMu.Lock()
	defer logMu.Unlock()
	switch strings.ToLower(name) {
	case "debug":
		loggerLevel.Set(slog.LevelDebug)
	case "info":
		loggerLevel.Set(slog.LevelInfo)
	case "warn", "warning":
		loggerLevel.Set(slog.LevelWarn)
	case "error":
		loggerLevel.Set(slog.LevelError)
	}
}
