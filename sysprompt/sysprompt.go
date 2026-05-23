// Package sysprompt composes relay-owned durable system prompt text.
package sysprompt

import "strings"

// Compose joins the base relay prompt, optional operator extra text, and
// optional skills catalog. Empty pieces are skipped and pieces are separated
// by a blank line.
func Compose(base, extra, catalog string) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{base, extra, catalog} {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}

// Resolve returns an empty prompt when disabled, otherwise Compose(base, extra, catalog).
func Resolve(base, extra string, disabled bool, catalog string) string {
	if disabled {
		return ""
	}
	return Compose(base, extra, catalog)
}
