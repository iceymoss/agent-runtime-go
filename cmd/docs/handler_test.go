package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestSPAHandler(t *testing.T) {
	assets := fstest.MapFS{
		"index.html":        {Data: []byte("index")},
		"assets/app-123.js": {Data: []byte("script")},
	}
	handler := spaHandler(assets)

	tests := []struct {
		path, body, cache string
		status            int
	}{
		{path: "/docs/architecture", body: "index", cache: "no-cache", status: http.StatusOK},
		{path: "/assets/app-123.js", body: "script", cache: "public, max-age=31536000, immutable", status: http.StatusOK},
		{path: "/assets/missing.js", body: "404 page not found\n", status: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || response.Body.String() != test.body {
				t.Fatalf("got status=%d body=%q", response.Code, response.Body.String())
			}
			if got := response.Header().Get("Cache-Control"); got != test.cache {
				t.Fatalf("Cache-Control = %q, want %q", got, test.cache)
			}
		})
	}
}

func TestEmbeddedAssetContract(t *testing.T) {
	var _ fs.FS = fstest.MapFS{}
}
