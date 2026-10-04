package plugin

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
)

//go:embed web/ui.html
var uiHTML []byte

// uiResponse serves the self-contained panel page. The host dispatches
// /v0/resource/plugins/<id>/ui here via the management.handle callback.
func uiResponse() []byte {
	resp := ManagementResponse{
		StatusCode: 200,
		Headers: map[string][]string{
			"Content-Type":           {"text/html; charset=utf-8"},
			"Cache-Control":          {"private, no-store"},
			"Pragma":                 {"no-cache"},
			"Referrer-Policy":        {"no-referrer"},
			"X-Content-Type-Options": {"nosniff"},
			"Content-Security-Policy": {
				"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; " +
					"connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'",
			},
		},
		Body: uiHTML,
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope(500, "plugin_error", err.Error())
	}
	return mustOKEnvelope(raw)
}

// pluginLogo is an inline SVG data URI so the management center needs no
// external image assets.
var pluginLogo = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(pluginIconSVG))

const pluginIconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48" fill="#72787c">
  <path d="M24 4C13 4 4 13 4 24s9 20 20 20c3.3 0 6.5-.8 9.3-2.3l-1.8-3.7A16 16 0 1 1 40 24c0 2.9-.8 5.7-2.2 8.1l3.5 2A20 20 0 0 0 24 4z"/>
  <path d="M24 12l-9 7v3h2v10h5v-6h4v6h5V22h2v-3l-9-7zm0 4.3L28.5 20h-9L24 16.3zM25 25v-2h-2v2h-3v-1.8l4-3.1 4 3.1V25h-3z"/>
</svg>
`
