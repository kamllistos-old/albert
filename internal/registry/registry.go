// Package registry looks up the latest published version of a package on the
// public registries (PyPI, npm, crates.io, the Go module proxy).
package registry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const (
	userAgent    = "albert (github.com/kamllistos/albert)"
	maxBodyBytes = 8 << 20
)

// Pkg is what a registry knows about a package.
type Pkg struct {
	Name, Version, Description, Homepage string
}

// Client talks to the registries. The base URL fields exist so tests can point
// them at httptest servers.
type Client struct {
	HTTP *http.Client
	// Base URLs, without a trailing slash.
	PyPIURL, NPMURL, CratesURL, GoProxyURL string
}

// New returns a Client for the real registries with a 10s timeout.
func New() *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		PyPIURL:    "https://pypi.org",
		NPMURL:     "https://registry.npmjs.org",
		CratesURL:  "https://crates.io",
		GoProxyURL: "https://proxy.golang.org",
	}
}

// get fetches rawURL and decodes the JSON body into v. Any non-200 status is
// an error.
func (c *Client) get(ctx context.Context, rawURL string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("registry: %s returned %s", req.URL.Redacted(), resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(v); err != nil {
		return fmt.Errorf("registry: decoding %s: %w", req.URL.Redacted(), err)
	}
	return nil
}

// escapePath escapes each "/"-separated segment of a package name.
func escapePath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// PyPI returns the latest release of a Python package.
func (c *Client) PyPI(ctx context.Context, name string) (Pkg, error) {
	var r struct {
		Info struct {
			Version     string            `json:"version"`
			Summary     string            `json:"summary"`
			HomePage    string            `json:"home_page"`
			ProjectURLs map[string]string `json:"project_urls"`
		} `json:"info"`
	}
	if err := c.get(ctx, c.PyPIURL+"/pypi/"+url.PathEscape(name)+"/json", &r); err != nil {
		return Pkg{}, err
	}
	home := r.Info.HomePage
	if home == "" {
		home = r.Info.ProjectURLs["Homepage"]
	}
	return Pkg{Name: name, Version: r.Info.Version, Description: r.Info.Summary, Homepage: home}, nil
}

type npmPackage struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Homepage    string `json:"homepage"`
	Links       struct {
		Homepage string `json:"homepage"`
	} `json:"links"`
}

func (p npmPackage) pkg() Pkg {
	home := p.Homepage
	if home == "" {
		home = p.Links.Homepage
	}
	return Pkg{Name: p.Name, Version: p.Version, Description: p.Description, Homepage: home}
}

// NPM returns the latest release of an npm package.
func (c *Client) NPM(ctx context.Context, name string) (Pkg, error) {
	var r npmPackage
	if err := c.get(ctx, c.NPMURL+"/"+escapePath(name)+"/latest", &r); err != nil {
		return Pkg{}, err
	}
	p := r.pkg()
	if p.Name == "" {
		p.Name = name
	}
	return p, nil
}

// NPMSearch returns up to 10 npm packages matching q.
func (c *Client) NPMSearch(ctx context.Context, q string) ([]Pkg, error) {
	var r struct {
		Objects []struct {
			Package npmPackage `json:"package"`
		} `json:"objects"`
	}
	u := c.NPMURL + "/-/v1/search?" + url.Values{"text": {q}, "size": {"10"}}.Encode()
	if err := c.get(ctx, u, &r); err != nil {
		return nil, err
	}
	out := make([]Pkg, 0, len(r.Objects))
	for _, o := range r.Objects {
		out = append(out, o.Package.pkg())
	}
	return out, nil
}

// Crate returns the latest stable release of a crates.io crate.
func (c *Client) Crate(ctx context.Context, name string) (Pkg, error) {
	var r struct {
		Crate struct {
			Name             string `json:"name"`
			MaxStableVersion string `json:"max_stable_version"`
			MaxVersion       string `json:"max_version"`
			Description      string `json:"description"`
			Homepage         string `json:"homepage"`
		} `json:"crate"`
	}
	if err := c.get(ctx, c.CratesURL+"/api/v1/crates/"+url.PathEscape(name), &r); err != nil {
		return Pkg{}, err
	}
	v := r.Crate.MaxStableVersion
	if v == "" {
		v = r.Crate.MaxVersion // pre-release-only crate
	}
	n := r.Crate.Name
	if n == "" {
		n = name
	}
	return Pkg{Name: n, Version: v, Description: r.Crate.Description, Homepage: r.Crate.Homepage}, nil
}

// GoLatest returns the latest version of a Go module from the module proxy.
func (c *Client) GoLatest(ctx context.Context, module string) (string, error) {
	var r struct {
		Version string `json:"Version"`
	}
	if err := c.get(ctx, c.GoProxyURL+"/"+escapePath(escapeModule(module))+"/@latest", &r); err != nil {
		return "", err
	}
	return r.Version, nil
}

// escapeModule applies the module proxy's case encoding: each uppercase letter
// becomes "!" followed by its lowercase form.
func escapeModule(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsUpper(r) {
			b.WriteByte('!')
			b.WriteRune(unicode.ToLower(r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
