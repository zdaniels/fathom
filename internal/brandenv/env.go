// Package brandenv reads Fathom settings while accepting legacy environment names.
package brandenv

import (
	"os"
	"strings"
)

// Get prefers an explicitly set FATHOM_ value, including an empty value,
// then falls back to the equivalent FANTAZM_ setting.
func Get(key string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	if strings.HasPrefix(key, "FATHOM_") {
		return os.Getenv("FANTAZM_" + strings.TrimPrefix(key, "FATHOM_"))
	}
	return ""
}
