// Package web embeds the public Maintainer Cockpit browser application.
package web

import (
	"embed"
	"io/fs"
)

//go:embed static/index.html static/app.js static/filters.js static/groups.js static/hidden.js static/important.js static/router.js static/row-action.js static/sorting.js static/table-columns.js static/tooltip.js static/view-model.js static/styles.css
var staticFiles embed.FS

// Files returns the static application rooted at its public directory.
func Files() fs.FS {
	files, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	return files
}
