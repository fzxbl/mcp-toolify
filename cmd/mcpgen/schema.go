package main

import (
	"strings"
	"unicode"
)

// snakeCase converts camelCase / PascalCase to snake_case.
// Runs of uppercase letters are kept together until a lowercase follows
// (e.g. HTTPServer -> http_server, appID -> app_id).
func snakeCase(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		if unicode.IsUpper(r) {
			prevLower := i > 0 && unicode.IsLower(rs[i-1])
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			prevUpper := i > 0 && unicode.IsUpper(rs[i-1])
			if i > 0 && (prevLower || (prevUpper && nextLower)) {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// exportName uppercases the first rune.
func exportName(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
