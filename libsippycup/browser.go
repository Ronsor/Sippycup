package libsippycup

import (
	"bytes"
	"embed"
	"html/template"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

//go:embed call.html call.js audio-worklet.js
var browserFiles embed.FS

var browserTemplate = template.Must(template.ParseFS(browserFiles, "call.html"))

// prefersHTML requires an explicit HTML preference. Generic API requests with
// */* retain their protocol behavior; SDP submissions and upgrades take priority.
func prefersHTML(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}

	if r.Header.Get("Upgrade") != "" || r.Header.Get("Sec-WebSocket-Key") != "" ||
		r.Header.Get("Sec-WebSocket-Version") != "" || hasUpgradeToken(r) {
		return false
	}

	contentType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if contentType == "application/sdp" {
		return false
	}

	htmlQuality, otherQuality := -1.0, 0.0
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		kind, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}

		q := 1.0
		if value, ok := params["q"]; ok {
			q, err = strconv.ParseFloat(value, 64)
			if err != nil || !(q >= 0 && q <= 1) {
				continue
			}
		}

		if kind == "text/html" {
			htmlQuality = max(htmlQuality, q)
		} else {
			otherQuality = max(otherQuality, q)
		}
	}

	return htmlQuality > 0 && htmlQuality >= otherQuality
}

// hasUpgradeToken preserves handshake error responses even for incomplete upgrades.
func hasUpgradeToken(r *http.Request) bool {
	for _, token := range strings.Split(r.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}

	return false
}

// serveBrowser handles only browser navigation and the page's own assets.
// The engine has already checked origin and HTTP authentication.
func serveBrowser(n *Node, w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != n.Path {
		return false
	}

	asset := r.URL.Query().Get("sippycup_asset")
	if asset != "" && (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Upgrade") == "" && r.Header.Get("Sec-WebSocket-Key") == "" &&
		r.Header.Get("Sec-WebSocket-Version") == "" && !hasUpgradeToken(r) {
		if asset != "call.js" && asset != "audio-worklet.js" {
			http.NotFound(w, r)

			return true
		}

		body, err := browserFiles.ReadFile(asset)
		if err != nil {
			http.Error(w, "asset unavailable", http.StatusInternalServerError)

			return true
		}

		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}

		return true
	}

	w.Header().Add("Vary", "Accept")
	if !prefersHTML(r) {
		return false
	}

	var body bytes.Buffer
	data := struct {
		Protocol, Name              string
		Extended, Native, BasicAuth bool
	}{
		Protocol:  n.Type,
		Name:      n.Name,
		Extended:  n.Extended,
		Native:    n.EarlyMedia == "native",
		BasicAuth: n.Auth != nil && slices.Contains(n.Auth.Sources, "basic_auth"),
	}
	if err := browserTemplate.Execute(&body, data); err != nil {
		http.Error(w, "page unavailable", http.StatusInternalServerError)

		return true
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'unsafe-inline'; "+
			"connect-src 'self'; media-src 'self' blob:; frame-ancestors 'none'; base-uri 'none'")
	if r.Method != http.MethodHead {
		_, _ = w.Write(body.Bytes())
	}

	return true
}
