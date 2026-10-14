package httpx

import (
	"net/http"
	"testing"
)

func TestBearerFromHeader(t *testing.T) {
	for in, want := range map[string]string{
		"Bearer abc":     "abc",
		"bearer  abc ":   "abc",
		"Basic abc":      "",
		"Bearer":         "",
		"Bearer a b":     "",
		"":               "",
		"Bearer\tabc":    "",
		"BEARER x.y.z-_": "x.y.z-_",
	} {
		h := http.Header{}
		if in != "" {
			h.Set("Authorization", in)
		}
		if got := BearerFromHeader(h); got != want {
			t.Errorf("BearerFromHeader(%q) = %q, want %q", in, got, want)
		}
	}
}
