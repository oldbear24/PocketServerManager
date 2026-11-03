package panel

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

const uiNotBuilt = `<!doctype html><meta charset="utf-8"><title>PocketCloud</title>
<p>The web UI is not part of this build. Run <code>task web</code> and rebuild the panel.
The API is available under <code>/api/</code>.</p>`

// bindUI serves the embedded UI at / with a fallback to index.html for deep
// links. The API stays at /api/ and PocketBase's dashboard at /_/.
func bindUI(se *core.ServeEvent, ui fs.FS) {
	static := apis.Static(ui, true)

	_, err := fs.Stat(ui, "index.html")
	built := err == nil

	se.Router.GET("/{path...}", func(e *core.RequestEvent) error {
		path := e.Request.PathValue(apis.StaticWildcardParam)

		// an unknown API route must stay an API error, not become the UI
		if path == "api" || strings.HasPrefix(path, "api/") {
			return e.NotFoundError("", nil)
		}

		if !built {
			return e.HTML(http.StatusServiceUnavailable, uiNotBuilt)
		}

		// file names under assets/ contain a hash of their content
		if strings.HasPrefix(path, "assets/") {
			e.Response.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			e.Response.Header().Set("Cache-Control", "no-cache")
		}

		return static(e)
	}).Bind(apis.Gzip())
}
