package server

import "testing"

func TestSafeNext(t *testing.T) {
	for next, want := range map[string]string{
		"/bikes":                 "/bikes",
		"/bikes/x?tab=parts&a=1": "/bikes/x?tab=parts&a=1",
		"/a%20b":                 "/a%20b",
		"":                       "/",
		"bikes":                  "/",
		"//evil.example":         "/",
		"/\\evil.example":        "/",
		"///evil.example":        "/",
		"https://evil.example":   "/",
		"javascript:alert(1)":    "/",
		"/\t/evil.example":       "/",
		"/\n/evil.example":       "/",
	} {
		if got := safeNext(next); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", next, got, want)
		}
	}
}
