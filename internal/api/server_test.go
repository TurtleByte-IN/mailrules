package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz(t *testing.T) {
	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodGet, "/nope", http.StatusNotFound},
	}
	for _, tt := range tests {
		req := httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil)
		rec := httptest.NewRecorder()
		NewHandler().ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Errorf("%s %s: got %d want %d", tt.method, tt.path, rec.Code, tt.want)
		}
	}
}
