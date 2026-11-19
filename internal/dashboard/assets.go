package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// vendored holds third-party files served by the binary, never from a CDN:
// xterm.js and its fit addon for the terminal view (assets/xterm/VERSIONS
// pins them; the licences sit beside them) and the design system stylesheet
// (assets/sandcorp.css, inlined into the page by DesignCSS).
//
//go:embed assets
var vendored embed.FS

// AssetsPrefix is the URL path the vendored files are served under.
const AssetsPrefix = "/assets/"

// TerminalAssets are the terminal view's files, loaded by the page only when
// the admin bootstrap reports hosting on: stylesheets, then scripts in order.
var TerminalAssets = []string{
	AssetsPrefix + "xterm/xterm.css",
	AssetsPrefix + "xterm/xterm.js",
	AssetsPrefix + "xterm/addon-fit.js",
}

var assetTypes = map[string]string{
	".js":  "text/javascript; charset=utf-8",
	".css": "text/css; charset=utf-8",
}

// Assets serves one vendored file (GET or HEAD). Directories are not listed.
func Assets(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, AssetsPrefix)
	if !ok || name == "" || path.Clean(name) != name {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	b, err := fs.ReadFile(vendored, "assets/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	typ, ok := assetTypes[path.Ext(name)]
	if !ok {
		typ = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("Cache-Control", "no-cache")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(b)
}

// DesignCSS is the design system stylesheet (assets/sandcorp.css, copied from
// sandcorporation/sandcorp-design by scripts/sync-design-css.sh). The page
// inlines it, so it reaches the browser with hosting off as well.
func DesignCSS() string {
	b, err := fs.ReadFile(vendored, "assets/sandcorp.css")
	if err != nil {
		panic("dashboard: embedded sandcorp.css missing: " + err.Error())
	}
	return string(b)
}
