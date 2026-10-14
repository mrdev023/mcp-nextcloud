// Package httpx holds small HTTP helpers shared by the server packages.
package httpx

import (
	"net/http"
	"strings"
)

// BearerFromHeader extracts the token of an "Authorization: Bearer <token>"
// header, or "" if absent or malformed.
func BearerFromHeader(h http.Header) string {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(h.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	tok = strings.TrimSpace(tok)
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return ""
	}
	return tok
}
