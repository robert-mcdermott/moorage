// Package brand holds Moorage's icon for the pages the server serves. The
// logos for documents are in docs/assets; icon.svg is a copy of
// docs/assets/moorage-icon.svg.
package brand

import (
	_ "embed"
	"encoding/base64"
	"html/template"
)

//go:embed icon.svg
var icon string

// Icon is the icon's SVG markup, to place in a page.
func Icon() template.HTML { return template.HTML(icon) }

// FaviconURL is the icon as a data: URL, for a page's <link rel="icon">.
func FaviconURL() template.URL {
	return template.URL("data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(icon)))
}
