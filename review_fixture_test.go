package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// Optional offline browser fixture export. No server, microphone, credentials or
// external Jitsi/OpenAI requests are used. Normal CI leaves this test skipped.
func TestRenderReviewPages(t *testing.T) {
	dir := os.Getenv("MEET_REVIEW_DIR")
	if dir == "" {
		t.Skip("set MEET_REVIEW_DIR for offline browser fixtures")
	}
	e := newTestEnv(t)
	write := func(name string, b []byte) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := staticFS.ReadDir("static")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		b, err := staticFS.ReadFile("static/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		write("static/"+entry.Name(), b)
	}
	pages := map[string]map[string]any{
		"meeting.html":  {"Slug": "proyecto-alfa", "AppID": "fixture", "JWT": "fixture-not-a-token", "DisplayName": "Participante de prueba", "IsModerator": true, "CSRF": "fixture", "Capture": true},
		"captures.html": {"Filter": sessionFilter{}, "CSRF": "fixture", "Sessions": []sessionRow{{CaptureID: 1, Room: "proyecto-alfa", Session: "captura-1", Client: "Cliente de ejemplo", Project: "Proyecto Alfa", Organizer: "organizador@example.test", Status: "pending", Started: e.now}}},
	}
	for name, data := range pages {
		var b bytes.Buffer
		if err := e.app.templates.ExecuteTemplate(&b, name, data); err != nil {
			t.Fatal(err)
		}
		page := regexp.MustCompile(`https://8x8\.vc/[^\"]+/external_api\.js`).ReplaceAllString(b.String(), "/static/jitsi-fixture.js")
		write(name, []byte(page))
	}
	write("static/jitsi-fixture.js", []byte(`window.JitsiMeetExternalAPI=function(){throw new Error("Fixture de diseño: Jitsi no conectado")};`))
	for _, f := range []struct{ src, dst string }{{"index.html", "visualizations/index.html"}, {"app.js", "visualizations/assets/app.js"}, {"app.css", "visualizations/assets/app.css"}} {
		b, err := visualizationFS.ReadFile("visualization/" + f.src)
		if err != nil {
			t.Fatal(err)
		}
		write(f.dst, b)
	}
}
