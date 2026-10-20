package version

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOutdated(t *testing.T) {
	cases := []struct {
		running, latest string
		want            bool
	}{
		{"v0.8.0", "v0.9.0", true},
		{"0.8.0", "v0.8.0", false},
		{"v0.9.0", "v0.8.0", false},
		{"dev", "v0.9.0", false},
		{"v0.1.0-12-g21fdec5-dirty", "v0.9.0", false},
		{"v0.8.0", "nonsense", false},
	}
	for _, c := range cases {
		if got := Outdated(c.running, c.latest); got != c.want {
			t.Errorf("Outdated(%q, %q) = %v, want %v", c.running, c.latest, got, c.want)
		}
	}
}

func TestLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v0.9.0"}`))
	}))
	defer srv.Close()
	if got, err := Latest(context.Background(), srv.Client(), srv.URL); err != nil || got != "v0.9.0" {
		t.Fatalf("Latest = %q, %v", got, err)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	defer gone.Close()
	if _, err := Latest(context.Background(), gone.Client(), gone.URL); err == nil {
		t.Fatal("a 404 should be an error")
	}
}
