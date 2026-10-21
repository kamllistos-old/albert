package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New()
	c.PyPIURL, c.NPMURL, c.CratesURL, c.GoProxyURL = srv.URL, srv.URL, srv.URL, srv.URL
	return c
}

func TestNewDefaults(t *testing.T) {
	c := New()
	if c.PyPIURL != "https://pypi.org" || c.NPMURL != "https://registry.npmjs.org" ||
		c.CratesURL != "https://crates.io" || c.GoProxyURL != "https://proxy.golang.org" {
		t.Fatalf("unexpected base URLs: %+v", c)
	}
	if c.HTTP == nil || c.HTTP.Timeout.Seconds() != 10 {
		t.Fatalf("expected 10s timeout, got %+v", c.HTTP)
	}
}

func TestPyPI(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pypi/ruff/json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"info":{"name":"ruff","version":"0.6.9","summary":"A fast linter","home_page":"","project_urls":{"Homepage":"https://docs.astral.sh/ruff"}}}`))
	})
	p, err := c.PyPI(context.Background(), "ruff")
	if err != nil {
		t.Fatal(err)
	}
	want := Pkg{Name: "ruff", Version: "0.6.9", Description: "A fast linter", Homepage: "https://docs.astral.sh/ruff"}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
}

func TestPyPIPrefersHomePage(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"info":{"version":"1.0","summary":"s","home_page":"https://a.example","project_urls":{"Homepage":"https://b.example"}}}`))
	})
	p, err := c.PyPI(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if p.Homepage != "https://a.example" || p.Name != "x" {
		t.Fatalf("got %+v", p)
	}
}

func TestNPM(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/typescript/latest" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"name":"typescript","version":"5.6.3","description":"TypeScript is a language","homepage":"https://www.typescriptlang.org/"}`))
	})
	p, err := c.NPM(context.Background(), "typescript")
	if err != nil {
		t.Fatal(err)
	}
	want := Pkg{Name: "typescript", Version: "5.6.3", Description: "TypeScript is a language", Homepage: "https://www.typescriptlang.org/"}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
}

func TestNPMScopedName(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/@vercel/ncc/latest" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"name":"@vercel/ncc","version":"0.38.1"}`))
	})
	p, err := c.NPM(context.Background(), "@vercel/ncc")
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "0.38.1" {
		t.Fatalf("got %+v", p)
	}
}

func TestNPMSearch(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/-/v1/search" {
			t.Errorf("path = %q", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("text") != "lint tool" || q.Get("size") != "10" {
			t.Errorf("query = %v", q)
		}
		w.Write([]byte(`{"objects":[
			{"package":{"name":"eslint","version":"9.1.0","description":"linter","links":{"homepage":"https://eslint.org"}}},
			{"package":{"name":"jshint","version":"2.13.6","description":"hinter"}}]}`))
	})
	got, err := c.NPMSearch(context.Background(), "lint tool")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results: %+v", len(got), got)
	}
	if got[0] != (Pkg{Name: "eslint", Version: "9.1.0", Description: "linter", Homepage: "https://eslint.org"}) {
		t.Fatalf("got[0] = %+v", got[0])
	}
	if got[1].Name != "jshint" || got[1].Homepage != "" {
		t.Fatalf("got[1] = %+v", got[1])
	}
}

func TestCrateSendsUserAgent(t *testing.T) {
	var ua string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		ua = r.Header.Get("User-Agent")
		if r.URL.Path != "/api/v1/crates/ripgrep" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Write([]byte(`{"crate":{"name":"ripgrep","max_stable_version":"14.1.1","max_version":"15.0.0-rc.1","description":"fast grep","homepage":"https://github.com/BurntSushi/ripgrep"}}`))
	})
	p, err := c.Crate(context.Background(), "ripgrep")
	if err != nil {
		t.Fatal(err)
	}
	if ua != "albert (github.com/kamllistos/albert)" {
		t.Fatalf("User-Agent = %q", ua)
	}
	want := Pkg{Name: "ripgrep", Version: "14.1.1", Description: "fast grep", Homepage: "https://github.com/BurntSushi/ripgrep"}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
}

func TestGoLatestEscapesUppercase(t *testing.T) {
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"Version":"v1.4.0","Time":"2024-01-01T00:00:00Z"}`))
	})
	v, err := c.GoLatest(context.Background(), "github.com/BurntSushi/toml")
	if err != nil {
		t.Fatal(err)
	}
	if path != "/github.com/!burnt!sushi/toml/@latest" {
		t.Fatalf("path = %q", path)
	}
	if v != "v1.4.0" {
		t.Fatalf("version = %q", v)
	}
}

func TestRegistry404(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	ctx := context.Background()
	checks := map[string]func() error{
		"pypi":   func() error { _, err := c.PyPI(ctx, "nope"); return err },
		"npm":    func() error { _, err := c.NPM(ctx, "nope"); return err },
		"search": func() error { _, err := c.NPMSearch(ctx, "nope"); return err },
		"crate":  func() error { _, err := c.Crate(ctx, "nope"); return err },
		"go":     func() error { _, err := c.GoLatest(ctx, "example.com/nope"); return err },
	}
	for name, f := range checks {
		err := f()
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Errorf("%s: want error mentioning 404, got %v", name, err)
		}
	}
}

func TestRegistryBadJSON(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	})
	if _, err := c.PyPI(context.Background(), "x"); err == nil {
		t.Fatal("want decode error")
	}
}
