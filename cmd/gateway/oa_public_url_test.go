package main

import (
	"io"
	"log/slog"
	"testing"
)

func TestOAPublicBaseURLFromEnvIsOptionalAndValidated(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for value, want := range map[string]string{
		"": "",
		"https://wmm-wsl.tail8b36e9.ts.net:8443/": "https://wmm-wsl.tail8b36e9.ts.net:8443",
		"http://127.0.0.1:8092":                   "http://127.0.0.1:8092",
		"oa.internal:8092":                        "",
		"https://oa.example/?x=1":                 "",
	} {
		t.Setenv("PUBLIC_OA_BASE_URL", value)
		if got := oaPublicBaseURLFromEnv(logger); got != want {
			t.Fatalf("PUBLIC_OA_BASE_URL=%q gave %q, want %q", value, got, want)
		}
	}
}
