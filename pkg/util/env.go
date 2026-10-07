package util

import "os"

// GetEnv returns the environment variable key, or fallback if it is unset or empty.
func GetEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
