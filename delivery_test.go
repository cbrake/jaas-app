package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"image/gif"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeMailer struct {
	mu   sync.Mutex
	sent []*Mail
	err  error
}

// copies returns the participant deliveries (not the host notices).
func (f *fakeMailer) copies() []*Mail {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Mail
	for _, m := range f.sent {
		if strings.HasPrefix(m.Subject, "Transcripción y minuta") {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeMailer) notices() []*Mail {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*Mail
	for _, m := range f.sent {
		if strings.HasPrefix(m.Subject, "Minuta lista para revisar") {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeMailer) Send(_ context.Context, m *Mail) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

func joinForm(tok, name, email string) url.Values {
	return url.Values{"form_token": {tok}, "display_name": {name}, "email": {email}, "consent": {"on"}}
}

func TestJoinRequiresEmailAndConsent(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("sala-prueba", "x", "")
	e.app.DB.SetRoomActive("sala-prueba", true)
	g := e.browser()
	tok := hidden(t, g.do(http.MethodGet, "/m/sala-prueba", nil).Body.String(), "form_token")

	cases := map[string]url.Values{
		"no e-mail":        {"form_token": {tok}, "display_name": {"Ana"}, "consent": {"on"}},
		"bad e-mail":       {"form_token": {tok}, "display_name": {"Ana"}, "email": {"ana@"}, "consent": {"on"}},
		"header injection": {"form_token": {tok}, "display_name": {"Ana"}, "email": {"ana@x.com\r\nBcc: v@evil.com"}, "consent": {"on"}},
		"no consent":       {"form_token": {tok}, "display_name": {"Ana"}, "email": {"ana@cliente.example"}},
	}
	for name, form := range cases {
		if rr := g.do(http.MethodPost, "/m/sala-prueba/join", form); rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	rr := g.do(http.MethodPost, "/m/sala-prueba/join", joinForm(tok, "Ana", " Ana@Cliente.Example "))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("valid join: %d", rr.Code)
	}
	c := jwtClaims(t, e, g.do(http.MethodGet, "/m/sala-prueba", nil).Body.String())
	if got := c["context"].(map[string]any)["user"].(map[string]any)["email"]; got != "ana@cliente.example" {
		t.Fatalf("JWT e-mail: %v", got)
	}
	page := e.browser().do(http.MethodGet, "/m/sala-prueba", nil).Body.String()
	if !strings.Contains(page, "legal-privacidad.html") || !regexp.MustCompile(`<input type="email"[^>]*name="email"[^>]*required`).MatchString(page) {
		t.Fatal("join page lacks the privacy notice or the required e-mail field")
	}
}

// meetingWithParticipants: Alice (host, Access) runs a meeting where Ana and
// Beto join through the link; JaaS confirms Ana and Alice in the session.
func meetingWithParticipants(t *testing.T) (*accessEnv, *browser, string, *Minutes, *fakeMailer) {
	a := newAccessEnv(t)
	fm := &fakeMailer{}
	a.app.Mailer = fm
	alice, csrf := a.user("alice@transgenia.org")
	a.createRoom(alice, csrf, "alice-cliente-2026")
	if rr := alice.do(http.MethodPost, "/admin/rooms/alice-cliente-2026/start", url.Values{"csrf_token": {csrf}, "display_name": {"Alice"}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("start: %d", rr.Code)
	}
	for _, p := range [][2]string{{"Ana", "ana@cliente.example"}, {"Beto", "beto@cliente.example"}} {
		g := a.browser()
		tok := hidden(t, g.do(http.MethodGet, "/m/alice-cliente-2026", nil).Body.String(), "form_token")
		if rr := g.do(http.MethodPost, "/m/alice-cliente-2026/join", joinForm(tok, p[0], p[1])); rr.Code != http.StatusSeeOther {
			t.Fatalf("join %s: %d", p[0], rr.Code)
		}
	}
	// Someone who used the link in another, earlier meeting.
	a.app.DB.conn.Exec("INSERT INTO session_participants (room_slug, session_id, email, display_name, source, joined_at) VALUES ('alice-cliente-2026', '', 'viejo@cliente.example', 'Viejo', 'join', ?)", a.now.Add(-48*time.Hour).Unix())

	for _, p := range [][2]string{{"Ana", "ana@cliente.example"}, {"Alice", "alice@transgenia.org"}} {
		body := `{"idempotencyKey":"pj-` + p[0] + `","appId":"` + testAppID + `","eventType":"PARTICIPANT_JOINED","sessionId":"sess-1","timestamp":` +
			strconv.FormatInt(a.now.UnixMilli(), 10) + `,"fqn":"` + testAppID + `/alice-cliente-2026","data":{"name":"` + p[0] + `","email":"` + p[1] + `","moderator":false}}`
		if rr := a.signedWebhook(body); rr.Code != http.StatusOK {
			t.Fatalf("participant webhook: %d", rr.Code)
		}
	}
	a.advance(time.Hour)
	tr := strings.Replace(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "sess-1", "alice-cliente-2026", testLink), "1760464800000", strconv.FormatInt(a.now.UnixMilli(), 10), 1)
	a.signedWebhook(tr)
	drain(a.testEnv)
	ms, _ := a.app.DB.ListMinutes("alice-cliente-2026")
	if len(ms) != 1 || ms[0].Status != "ready" {
		t.Fatalf("minutes: %+v", ms)
	}
	return a, alice, csrf, &ms[0], fm
}

func TestProposedRecipients(t *testing.T) {
	a, _, _, m, _ := meetingWithParticipants(t)
	rs, err := a.app.DB.ProposedRecipients(m)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rs {
		got[r.Email] = r.InSession
	}
	want := map[string]bool{"alice@transgenia.org": true, "ana@cliente.example": true, "beto@cliente.example": false}
	if len(got) != len(want) {
		t.Fatalf("recipients: %v", got)
	}
	for e, inSession := range want {
		if v, ok := got[e]; !ok || v != inSession {
			t.Errorf("%s: present=%v inSession=%v", e, ok, v)
		}
	}
	if _, ok := got["viejo@cliente.example"]; ok {
		t.Error("participant of another meeting proposed")
	}
}

func TestSendRequiresApprovalOwnershipAndList(t *testing.T) {
	a, alice, csrf, m, fm := meetingWithParticipants(t)
	id := strconv.FormatInt(m.ID, 10)

	page := alice.do(http.MethodGet, "/admin/minutes/"+id, nil).Body.String()
	if !strings.Contains(page, "Enviar transcripción y minuta") || !strings.Contains(page, "beto@cliente.example") {
		t.Fatal("send form not shown")
	}
	// Nothing goes out without the host's approval.
	drain(a.testEnv)
	if len(fm.copies()) != 0 {
		t.Fatal("e-mail sent without approval")
	}
	// CSRF, ownership and the recipient list are enforced.
	if rr := alice.do(http.MethodPost, "/admin/minutes/"+id+"/send", url.Values{"recipient": {"ana@cliente.example"}}); rr.Code != http.StatusForbidden {
		t.Errorf("no csrf: %d", rr.Code)
	}
	bob, bCSRF := a.user("bob@transgenia.org")
	if rr := bob.do(http.MethodPost, "/admin/minutes/"+id+"/send", url.Values{"csrf_token": {bCSRF}, "recipient": {"ana@cliente.example"}}); rr.Code != http.StatusNotFound {
		t.Errorf("other user: %d", rr.Code)
	}
	if rr := alice.do(http.MethodPost, "/admin/minutes/"+id+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"ana@cliente.example", "intruso@evil.example"}}); rr.Code != http.StatusBadRequest {
		t.Errorf("address outside the list: %d", rr.Code)
	}
	drain(a.testEnv)
	if len(fm.copies()) != 0 {
		t.Fatal("rejected requests sent mail")
	}

	// Approve Ana and Beto (Alice unticks herself).
	rr := alice.do(http.MethodPost, "/admin/minutes/"+id+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"ana@cliente.example", "beto@cliente.example"}})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("send: %d", rr.Code)
	}
	drain(a.testEnv)
	if len(fm.copies()) != 2 {
		t.Fatalf("sent %d copies", len(fm.copies()))
	}
	for _, mail := range fm.copies() {
		if mail.ReplyTo != "alice@transgenia.org" || !strings.Contains(mail.Subject, "alice-cliente-2026") {
			t.Errorf("headers: to=%s reply=%s subject=%s", mail.To, mail.ReplyTo, mail.Subject)
		}
		if !strings.Contains(mail.Text, "Se revisó el avance") || !strings.Contains(mail.Text, "un administrador de Transgenia la revisó") || !strings.Contains(mail.Text, "legal-privacidad") {
			t.Errorf("body for %s missing summary, reviewer or privacy notice", mail.To)
		}
		if strings.Contains(mail.Text, "alice@transgenia.org") || strings.Contains(mail.HTML, "alice@transgenia.org") {
			t.Errorf("copy names the approving host")
		}
		if len(mail.Attachments) != 2 || !strings.Contains(string(mail.Attachments[0].Data), "Pablo envía la propuesta") ||
			!strings.Contains(string(mail.Attachments[1].Data), "| Enviar propuesta |") {
			t.Errorf("attachments for %s", mail.To)
		}
	}
	// Approving again never duplicates.
	alice.do(http.MethodPost, "/admin/minutes/"+id+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"ana@cliente.example"}})
	drain(a.testEnv)
	if len(fm.copies()) != 2 {
		t.Fatalf("duplicate delivery: %d copies", len(fm.copies()))
	}
	page = alice.do(http.MethodGet, "/admin/minutes/"+id, nil).Body.String()
	if strings.Count(page, `badge ok">enviado`) != 2 {
		t.Error("delivery status not shown")
	}
	// No recipient address in the logs.
	for _, addr := range []string{"ana@cliente.example", "beto@cliente.example"} {
		if strings.Contains(a.logs.String(), addr) {
			t.Errorf("log contains %s", addr)
		}
	}
}

func TestDeliveryRetriesThenFails(t *testing.T) {
	a, alice, csrf, m, fm := meetingWithParticipants(t)
	fm.err = errors.New("smtp: connection failed")
	alice.do(http.MethodPost, "/admin/minutes/"+strconv.FormatInt(m.ID, 10)+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"ana@cliente.example"}})
	for i := 1; i <= maxDeliveryAttempts; i++ {
		if !a.app.processDelivery(context.Background()) {
			t.Fatalf("attempt %d not claimed", i)
		}
		a.advance(4 * time.Hour)
	}
	var status, msg string
	a.app.DB.conn.QueryRow("SELECT status, error FROM minutes_deliveries").Scan(&status, &msg)
	if status != "failed" || msg != "smtp: connection failed" {
		t.Fatalf("status %s error %q", status, msg)
	}
	// Permanent SMTP rejection fails at once.
	fm.err = permanent("smtp: 550")
	alice.do(http.MethodPost, "/admin/minutes/"+strconv.FormatInt(m.ID, 10)+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"beto@cliente.example"}})
	a.app.processDelivery(context.Background())
	a.app.DB.conn.QueryRow("SELECT status FROM minutes_deliveries WHERE email = 'beto@cliente.example'").Scan(&status)
	if status != "failed" {
		t.Fatalf("550 not permanent: %s", status)
	}
}

func TestDeleteRoomRemovesParticipantsAndDeliveries(t *testing.T) {
	a, alice, csrf, m, _ := meetingWithParticipants(t)
	alice.do(http.MethodPost, "/admin/minutes/"+strconv.FormatInt(m.ID, 10)+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"ana@cliente.example"}})
	if rr := alice.do(http.MethodPost, "/admin/rooms/alice-cliente-2026/delete", url.Values{"csrf_token": {csrf}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rr.Code)
	}
	for _, q := range []string{"SELECT COUNT(*) FROM session_participants", "SELECT COUNT(*) FROM minutes_deliveries"} {
		var n int
		a.app.DB.conn.QueryRow(q).Scan(&n)
		if n != 0 {
			t.Errorf("%s: %d left", q, n)
		}
	}
}

func TestBuildMIME(t *testing.T) {
	m := &Mail{To: "ana@cliente.example", ReplyTo: "alice@transgenia.org", Subject: "Transcripción y minuta: sala (2026-10-15)",
		Text: "Hola Ana:\nlínea con acentos ñ\n", Attachments: []Attachment{{Name: "t.txt", ContentType: "text/plain; charset=utf-8", Data: []byte("hola transcripción")}}}
	raw, err := buildMIME("contacto@transgenia.org", "Transgenia Reuniones", m, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	dec := new(mime.WordDecoder)
	subject, _ := dec.DecodeHeader(msg.Header.Get("Subject"))
	if subject != m.Subject || msg.Header.Get("Reply-To") != "<alice@transgenia.org>" || msg.Header.Get("Bcc") != "" {
		t.Fatalf("headers: %v", msg.Header)
	}
	_, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(p)
		if p.Header.Get("Content-Transfer-Encoding") == "base64" {
			b, _ = base64.StdEncoding.DecodeString(string(b))
		}
		parts = append(parts, string(b))
	}
	if len(parts) != 2 || !strings.Contains(parts[0], "línea con acentos ñ") || parts[1] != "hola transcripción" {
		t.Fatalf("parts: %q", parts)
	}
	s := &smtpMailer{From: "contacto@transgenia.org"}
	for _, bad := range []string{"ana@x.com\r\nBcc: v@evil.com", "Ana <ana@x.com>", ""} {
		if err := s.Send(context.Background(), &Mail{To: bad}); err == nil || !isPermanent(err) {
			t.Errorf("address %q accepted", bad)
		}
	}
}

func TestHostNoticeWhenMinutesReady(t *testing.T) {
	a, _, _, m, fm := meetingWithParticipants(t)
	notices := fm.notices()
	if len(notices) != 1 {
		t.Fatalf("host notices: %d", len(notices))
	}
	n := notices[0]
	link := testOrigin + "/admin/minutes/" + strconv.FormatInt(m.ID, 10)
	if n.To != "alice@transgenia.org" || !strings.Contains(n.Text, link) || !strings.Contains(n.Subject, "alice-cliente-2026") {
		t.Fatalf("notice: to=%s subject=%s", n.To, n.Subject)
	}
	if !strings.Contains(n.Text, "Hay 3 participantes propuestos") {
		t.Errorf("participant count missing: %q", n.Text)
	}
	// No meeting content before approval: no summary, transcript or attachments.
	if strings.Contains(n.Text, "Se revisó el avance") || strings.Contains(n.Text, "Pablo") || len(n.Attachments) != 0 {
		t.Error("notice leaks meeting content")
	}
	for _, mail := range fm.sent {
		if mail.To != "alice@transgenia.org" {
			t.Errorf("mail to %s before approval", mail.To)
		}
	}
	// A re-run of the minutes (admin retry) never notifies twice.
	calls := a.llm.calls
	if _, err := a.app.DB.conn.Exec("UPDATE minutes SET status = 'failed' WHERE id = ?", m.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := a.app.DB.RequeueMinutes(m.ID); !ok || err != nil {
		t.Fatalf("requeue: %v %v", ok, err)
	}
	drain(a.testEnv)
	if a.llm.calls != calls+1 {
		t.Fatalf("minutes not regenerated: %d calls", a.llm.calls-calls)
	}
	a.advance(time.Hour) // let any (wrongly) re-queued notice become due
	drain(a.testEnv)
	if len(fm.notices()) != 1 {
		t.Fatalf("notice repeated: %d", len(fm.notices()))
	}
}

func TestHostNoticeForRoomWithoutOwnerGoesToAdmins(t *testing.T) {
	e := newTestEnv(t)
	fm := &fakeMailer{}
	e.app.Mailer = fm
	e.app.Config.AdminEmails = []string{"admin@transgenia.org"}
	e.app.DB.CreateRoom("sala-legado", "x", "")
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "sala-legado", testLink))
	drain(e)
	if ns := fm.notices(); len(ns) != 1 || ns[0].To != "admin@transgenia.org" || !strings.Contains(ns[0].Text, "No hay participantes") {
		t.Fatalf("notices: %+v", ns)
	}
}

func TestHostNoticeRetriesThenFails(t *testing.T) {
	e := newTestEnv(t)
	fm := &fakeMailer{err: errors.New("smtp: connection failed")}
	e.app.Mailer = fm
	e.app.Config.AdminEmails = []string{"admin@transgenia.org"}
	e.app.DB.CreateRoom("sala-legado", "x", "")
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "k1", "s1", "sala-legado", testLink))
	e.app.processOne(context.Background())
	for i := 1; i <= maxDeliveryAttempts; i++ {
		if !e.app.processHostNotification(context.Background()) {
			t.Fatalf("attempt %d not claimed", i)
		}
		e.advance(4 * time.Hour)
	}
	var status string
	e.app.DB.conn.QueryRow("SELECT status FROM host_notifications").Scan(&status)
	if status != "failed" || e.app.processHostNotification(context.Background()) {
		t.Fatalf("status %s after max attempts", status)
	}
	// The minutes themselves are unaffected by the notice failing.
	if ms, _ := e.app.DB.ListMinutes("sala-legado"); ms[0].Status != "ready" {
		t.Fatalf("minutes status %s", ms[0].Status)
	}
}

func TestMinutesEmailHTML(t *testing.T) {
	a, alice, csrf, m, fm := meetingWithParticipants(t)
	alice.do(http.MethodPost, "/admin/minutes/"+strconv.FormatInt(m.ID, 10)+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"ana@cliente.example"}})
	drain(a.testEnv)
	copies := fm.copies()
	if len(copies) != 1 {
		t.Fatalf("copies: %d", len(copies))
	}
	h := copies[0].HTML
	for _, want := range []string{
		"TRANSGENIA", "Hola Ana:", "Puntos clave", "Migración a Odoo 18", "Tareas acordadas", "Enviar propuesta",
		"Análisis de sentimiento", "Por participante", "Momentos clave", "De acuerdo, la mando el viernes",
		"linear-gradient(90deg,#EE7623 0%,#F0A055 33%,#D8292F 72%,#D1333B 100%)", "legal-privacidad.html",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML lacks %q", want)
		}
	}
	// The model's invented participant is dropped (not among the speakers).
	if strings.Contains(h, "Inventado") {
		t.Error("invented participant shown")
	}
	// Only palette colours (plus white body) and no remote images.
	allowed := map[string]bool{"#101820": true, "#EE7623": true, "#D1333B": true, "#F0A055": true, "#D8292F": true,
		"#FEFEEA": true, "#003C4D": true, "#3D3935": true, "#FFFFFF": true}
	for _, c := range regexp.MustCompile(`#[0-9A-Fa-f]{6}\b`).FindAllString(h, -1) {
		if !allowed[strings.ToUpper(c)] {
			t.Errorf("colour outside the palette: %s", c)
		}
	}
	if regexp.MustCompile(`<img[^>]+src="(https?:)?//`).MatchString(h) || strings.Contains(h, "<style") || strings.Contains(h, "—") {
		t.Error("remote image, <style> block or long dash in the e-mail")
	}
	if n := strings.Count(h, "<img"); n != 1 || !strings.Contains(h, `src="cid:`) {
		t.Errorf("expected exactly one embedded (cid) image, got %d", n)
	}
	// Orange is never used as a text colour on a light background.
	if regexp.MustCompile(`[^-]color:#EE7623`).MatchString(strings.ReplaceAll(h, "background-color:#EE7623", "")) &&
		!strings.Contains(h, "color:#EE7623;text-transform:uppercase;padding-top:6px") {
		t.Error("orange text outside the navy header")
	}
	// Plain-text alternative is still there; MIME carries both.
	raw, err := buildMIME("contacto@transgenia.org", "Transgenia Reuniones", copies[0], time.Now())
	if err != nil || !strings.Contains(string(raw), "multipart/alternative") || !strings.Contains(string(raw), "text/html") {
		t.Fatalf("mime: %v", err)
	}
	notices := fm.notices()
	if len(notices) != 1 || !strings.Contains(notices[0].HTML, "Revisar y aprobar") || !strings.Contains(notices[0].HTML, "/admin/minutes/") {
		t.Fatal("host notice HTML")
	}
}

func TestComputeMetrics(t *testing.T) {
	base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	us := []utterance{
		{Name: "Ana", Text: "uno dos tres cuatro cinco seis", Start: base, Duration: 3 * time.Second},
		{Name: "Beto", Text: "siete ocho", Start: base.Add(2 * time.Second), Duration: 1 * time.Second}, // interrupts Ana
		{Name: "Ana", Text: "nueve diez once doce", Start: base.Add(5 * time.Second), Duration: 2 * time.Second},
	}
	m := computeMetrics(us)
	if m == nil || !m.Measured || len(m.Speakers) != 2 {
		t.Fatalf("metrics: %+v", m)
	}
	ana, beto := m.Speakers[0], m.Speakers[1]
	if ana.Speaker != "Ana" || ana.Turns != 2 || ana.Words != 10 || ana.TalkSeconds != 5 || ana.SharePct != 83.3 || ana.WordsPerMinute != 120 {
		t.Errorf("ana: %+v", ana)
	}
	if beto.Interruptions != 1 || beto.Turns != 1 || ana.Interruptions != 0 {
		t.Errorf("interruptions: ana %d beto %d", ana.Interruptions, beto.Interruptions)
	}
	if !strings.Contains(metricsForPrompt(m), "Ana: 83% del tiempo de habla, 2 turnos, 120 palabras/min") {
		t.Errorf("prompt: %s", metricsForPrompt(m))
	}
	// Without durations (JaaS transcripts) the figures are estimated and flagged.
	us[0].Duration, us[1].Duration, us[2].Duration = 0, 0, 0
	if computeMetrics(us).Measured {
		t.Error("estimated metrics flagged as measured")
	}
}

func TestEmailsEmbedAnimatedGEN(t *testing.T) {
	a, alice, csrf, m, fm := meetingWithParticipants(t)
	alice.do(http.MethodPost, "/admin/minutes/"+strconv.FormatInt(m.ID, 10)+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"ana@cliente.example"}})
	drain(a.testEnv)
	for _, mail := range append(fm.copies(), fm.notices()...) {
		if !strings.Contains(mail.HTML, `src="cid:gen-analiza@transgenia.org"`) || !strings.Contains(mail.HTML, `alt="GEN, la mascota de Transgenia`) {
			t.Fatalf("%s: GEN not referenced", mail.Subject)
		}
		if len(mail.Inline) != 1 || mail.Inline[0].ContentID != "gen-analiza@transgenia.org" {
			t.Fatalf("%s: GEN not attached inline", mail.Subject)
		}
		g, err := gif.DecodeAll(bytes.NewReader(mail.Inline[0].Data))
		if err != nil || len(g.Image) < 20 || g.Config.Width != 336 || g.Config.Height != 168 || g.LoopCount != 0 {
			t.Fatalf("GEN gif: err=%v frames=%d", err, len(g.Image))
		}
		raw, err := buildMIME("contacto@transgenia.org", "Transgenia Reuniones", mail, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"multipart/related", "Content-ID: <gen-analiza@transgenia.org>", "Content-Disposition: inline", "image/gif"} {
			if !strings.Contains(string(raw), want) {
				t.Errorf("%s: MIME lacks %q", mail.Subject, want)
			}
		}
		if strings.Contains(mail.HTML, "http://") || regexp.MustCompile(`src="https?:`).MatchString(mail.HTML) {
			t.Errorf("%s: remote image", mail.Subject)
		}
	}
}

func TestCopyAttributionAndSignature(t *testing.T) {
	a, alice, csrf, m, fm := meetingWithParticipants(t)
	a.app.Config.MailReviewer = "un administrador de Transgenia"
	a.app.Config.MailSignatureName = "Aitana Yarandiz"
	a.app.Config.MailSignatureRole = "Agente Quack IA de Transgenia"
	a.app.Config.MailReplyTo = "admin@transgenia.org"
	alice.do(http.MethodPost, "/admin/minutes/"+strconv.FormatInt(m.ID, 10)+"/send", url.Values{"csrf_token": {csrf}, "recipient": {"ana@cliente.example"}})
	drain(a.testEnv)
	c := fm.copies()[0]
	if c.ReplyTo != "admin@transgenia.org" {
		t.Errorf("reply-to %s", c.ReplyTo)
	}
	for _, body := range []string{c.Text, c.HTML} {
		for _, want := range []string{"un administrador de Transgenia la revisó", "Aitana Yarandiz", "Agente Quack IA de Transgenia"} {
			if !strings.Contains(body, want) {
				t.Errorf("missing %q", want)
			}
		}
		if strings.Contains(body, "alice@transgenia.org") {
			t.Error("approving host named in the copy")
		}
	}
	// The approver is still recorded for audit.
	var approver string
	a.app.DB.conn.QueryRow("SELECT approved_by FROM minutes_deliveries").Scan(&approver)
	if approver != "alice@transgenia.org" {
		t.Errorf("approved_by %q", approver)
	}
}
