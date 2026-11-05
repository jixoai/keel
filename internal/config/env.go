// Package config holds the small amount of environment/config plumbing the
// runtime needs. It intentionally has no CLI surface: callers inject values.
package config

import "strings"

// GenericBaseURLEnvVar returns the conventional <PROVIDER>_BASE_URL variable
// name for a provider (dashes become underscores, uppercased). It returns ""
// for an empty provider name.
func GenericBaseURLEnvVar(providerName string) string {
	n := strings.TrimSpace(providerName)
	if n == "" {
		return ""
	}
	n = strings.ReplaceAll(n, "-", "_")
	return strings.ToUpper(n) + "_BASE_URL"
}
