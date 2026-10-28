// Package registry looks up the latest published version of a package on the
// public registries (PyPI, npm, crates.io, the Go module proxy).
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	userAgent    = "albert (github.com/kamllistos/albert)"
	maxBodyBytes = 8 << 20
)

// ErrNotFound means the registry has no such package (HTTP 404, or 410 from
// the Go module proxy). It is an answer, not a failure: the package simply is
// not published there. Check with errors.Is.
var ErrNotFound = errors.New("registry: package not found")

// Pkg is what a registry knows about a package.
type Pkg struct {
	Name, Version, Description, Homepage string
}

// Client talks to the registries. The base URL fields exist so tests can point
// them at httptest servers.
type Client struct {
	HTTP *http.Client
	// Base URLs, without a trailing slash.
	PyPIURL, NPMURL, CratesURL, GoProxyURL, GitHubURL string
}

// New returns a Client for the real registries with a 10s timeout.
func New() *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: 10 * time.Second},
		PyPIURL:    "https://pypi.org",
		NPMURL:     "https://registry.npmjs.org",
		CratesURL:  "https://crates.io",
		GoProxyURL: "https://proxy.golang.org",
		GitHubURL:  "https://api.github.com",
	}
}

// get fetches rawURL and decodes the JSON body into v. Any non-200 status is
// an error; 404 and 410 wrap ErrNotFound.
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
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			return fmt.Errorf("%w: %s returned %s", ErrNotFound, req.URL.Redacted(), resp.Status)
		}
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

// NewerAlbert returns the latest Albert release version when it is newer than
// current, or "" when current is up to date or a dev build.
func (c *Client) NewerAlbert(ctx context.Context, current string) (string, error) {
	current = strings.TrimPrefix(current, "v")
	if current == "" || current == "dev" {
		return "", nil
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := c.get(ctx, c.GitHubURL+"/repos/kamllistos/albert/releases/latest", &rel); err != nil {
		return "", err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	if !newerVersion(latest, current) {
		return "", nil
	}
	return latest, nil
}

// newerVersion reports whether dotted version a is greater than b. A
// pre-release suffix ("1.2.0-rc1") is ignored.
func newerVersion(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		x, y := versionPart(as, i), versionPart(bs, i)
		if x != y {
			return x > y
		}
	}
	return false
}

func versionPart(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(strings.SplitN(parts[i], "-", 2)[0])
	return n
}
