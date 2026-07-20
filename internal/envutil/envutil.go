// Package envutil holds the small environment-variable helpers shared by the
// webhook and cleanup entrypoints.
package envutil

import (
	"log"
	"os"
)

// MustEnv returns the value of key or terminates the process if it is unset.
// Used for configuration the binary cannot start without.
func MustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("Required environment variable %s is not set", key)
	}
	return v
}

// OrDefault returns the value of key, or fallback when it is unset or empty.
func OrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
