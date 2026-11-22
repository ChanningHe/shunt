package admin

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// The only URLs the web UI may contain, each exactly once: the base_url
// placeholder (a JavaScript string) and the SVG namespace, which is an
// identifier and never fetched.
var allowedURLs = map[string]string{
	"app.js":   "'https://api.example.com/anthropic'",
	"icon.svg": `xmlns="http://www.w3.org/2000/svg"`,
}

func TestWebAssetsServed(t *testing.T) {
	g := newRig(t, baseCfg, "")
	for path, ctype := range map[string]string{
		Prefix + "/":          "text/html; charset=utf-8",
		Prefix + "/app.js":    "text/javascript; charset=utf-8",
		Prefix + "/graph.js":  "text/javascript; charset=utf-8",
		Prefix + "/style.css": "text/css; charset=utf-8",
		Prefix + "/icon.svg":  "image/svg+xml",
	} {
		r := req(http.MethodGet, path, "")
		r.Header.Del("Authorization")
		rec := g.serve(r)
		expect(t, rec, http.StatusOK)
		h := rec.Header()
		if got := h.Get("Content-Type"); got != ctype {
			t.Errorf("%s: Content-Type %q, want %q", path, got, ctype)
		}
		if h.Get("Content-Security-Policy") != "default-src 'self'; frame-ancestors 'none'" ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: security headers %v", path, h)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", path)
		}
	}
}

// The page runs under CSP default-src 'self' and renders untrusted strings:
// no inline code or styles, no HTML sinks, no storage, no remote resources.
func TestWebAssetsAreSelfContained(t *testing.T) {
	banned := []string{
		"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function",
		"localStorage", "sessionStorage", "document.cookie", "http://", "https://",
		"style=", "<style", "'style'", "javascript:", "@import", "url(",
	}
	inlineHandler := regexp.MustCompile(`(?i)\son[a-z]+\s*=`)
	script := regexp.MustCompile(`(?is)<script([^>]*)>(.*?)</script>`)
	seen := 0
	err := fs.WalkDir(webRoot, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		seen++
		b, err := fs.ReadFile(webRoot, name)
		if err != nil {
			return err
		}
		text := string(b)
		if allowed, ok := allowedURLs[name]; ok {
			if strings.Count(text, allowed) != 1 {
				t.Errorf("%s: want %s exactly once", name, allowed)
			}
			text = strings.Replace(text, allowed, "", 1)
		}
		for _, s := range banned {
			if strings.Contains(text, s) {
				t.Errorf("%s contains %q", name, s)
			}
		}
		if m := inlineHandler.FindString(text); m != "" {
			t.Errorf("%s has an inline event handler: %q", name, m)
		}
		for _, m := range script.FindAllStringSubmatch(text, -1) {
			if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
				t.Errorf("%s has an inline script: %q", name, m[0])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 5 {
		t.Errorf("embedded %d files, want index.html, app.js, graph.js, style.css and icon.svg", seen)
	}
}

// app.js calls into graph.js, and the edges are drawn into a static <svg>
// whose namespaceURI app code borrows instead of spelling the SVG URL.
func TestWebGraphMarkup(t *testing.T) {
	b, err := fs.ReadFile(webRoot, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	graph := strings.Index(page, `<script src="graph.js" defer></script>`)
	app := strings.Index(page, `<script src="app.js" defer></script>`)
	if graph < 0 || app < 0 || graph > app {
		t.Errorf("index.html must load graph.js before app.js, both deferred")
	}
	if !regexp.MustCompile(`<svg [^>]*aria-hidden="true"[^>]*></svg>`).MatchString(page) {
		t.Errorf("index.html lacks the static aria-hidden <svg> for the edges")
	}
}
