package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// The demo agent-identity registration (KB1-LOOP-151) sits behind the admin
// token, and is not mounted without one.
func TestDemoAgentIdentityAdminRequiresTheAdminToken(t *testing.T) {
	reached := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})
	router := chi.NewRouter()
	mountDemoAgentIdentityAdmin(router, "admin-secret", handler)
	post := func(authorization string) int {
		request := httptest.NewRequest(http.MethodPost, "/admin/v1/demo/agent-identities", strings.NewReader(`{}`))
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := post(""); code != http.StatusUnauthorized {
		t.Fatalf("no admin token: %d, want 401", code)
	}
	if code := post("Bearer wrong-secret"); code != http.StatusUnauthorized {
		t.Fatalf("wrong admin token: %d, want 401", code)
	}
	if reached != 0 {
		t.Fatal("an unauthenticated request reached the handler")
	}
	if code := post("Bearer admin-secret"); code != http.StatusOK || reached != 1 {
		t.Fatalf("admin token: %d, reached %d", code, reached)
	}

	unmounted := chi.NewRouter()
	mountDemoAgentIdentityAdmin(unmounted, " ", handler)
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/demo/agent-identities", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer  ")
	recorder := httptest.NewRecorder()
	unmounted.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound || reached != 1 {
		t.Fatalf("without an admin token the route answered %d", recorder.Code)
	}
}
