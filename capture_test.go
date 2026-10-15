package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTranscriber returns segments keyed by the first bytes of each stream,
// so a test controls what every participant "said".
type fakeTranscriber struct {
	mu     sync.Mutex
	calls  int
	byMark map[string][]transcriptSegment
	err    error
}

func (f *fakeTranscriber) Transcribe(_ context.Context, audio []byte, _ string) ([]transcriptSegment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	for mark, segs := range f.byMark {
		if bytes.HasPrefix(audio, []byte(mark)) {
			return segs, nil
		}
	}
	return nil, nil
}

type participant struct {
	b    *browser
	csrf string
}

// joinCaptureRoom: a room with transcription on and one joined participant.
func joinCaptureRoom(t *testing.T, e *testEnv, slug, name, email string, moderator bool) participant {
	t.Helper()
	b := e.browser()
	tok := hidden(t, b.do(http.MethodGet, "/m/"+slug, nil).Body.String(), "form_token")
	form := joinForm(tok, name, email)
	if moderator {
		form.Set("host_password", "host-secret-pw")
	}
	if rr := b.do(http.MethodPost, "/m/"+slug+"/join", form); rr.Code != http.StatusSeeOther {
		t.Fatalf("join %s: %d", name, rr.Code)
	}
	page := b.do(http.MethodGet, "/m/"+slug, nil).Body.String()
	if !strings.Contains(page, `data-capture="true"`) {
		t.Fatal("meeting page does not enable capture")
	}
	m := regexp.MustCompile(`data-csrf="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no CSRF on meeting page")
	}
	return participant{b: b, csrf: m[1]}
}

func (p participant) upload(e *testEnv, slug, stream string, seq int, data []byte, csrf string) int {
	q := url.Values{"stream": {stream}, "seq": {strconv.Itoa(seq)}, "start": {strconv.FormatInt(e.now.UnixMilli(), 10)}}
	req := httptest.NewRequest(http.MethodPost, testOrigin+"/m/"+slug+"/audio?"+q.Encode(), bytes.NewReader(data))
	req.Header.Set("Content-Type", "audio/webm;codecs=opus")
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Origin", testOrigin)
	for _, c := range p.b.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	return rr.Code
}

func captureEnv(t *testing.T) (*testEnv, *fakeTranscriber) {
	e := newTestEnv(t)
	ft := &fakeTranscriber{byMark: map[string][]transcriptSegment{}}
	e.app.Transcriber = ft
	if err := e.app.DB.SetAdminPassword(testAdminPW, false); err != nil {
		t.Fatal(err)
	}
	b, csrf := e.loginAdminNoChange()
	e.createRoom(b, csrf, "capture-room")
	e.app.DB.SetTranscription("capture-room", true)
	e.app.DB.SetRoomActive("capture-room", true)
	return e, ft
}

func (e *testEnv) loginAdminNoChange() (*browser, string) {
	e.t.Helper()
	b := e.browser()
	page := b.do(http.MethodGet, "/admin/login", nil)
	b.do(http.MethodPost, "/admin/login", url.Values{"form_token": {hidden(e.t, page.Body.String(), "form_token")}, "password": {testAdminPW}})
	dash := b.do(http.MethodGet, "/admin", nil)
	if dash.Code != http.StatusOK {
		e.t.Fatalf("dashboard: %d", dash.Code)
	}
	return b, hidden(e.t, dash.Body.String(), "csrf_token")
}

const streamA = "AAAAAAAAAAAAAAAAAAAAAAAA"
const streamB = "BBBBBBBBBBBBBBBBBBBBBBBB"

func TestAudioUploadRules(t *testing.T) {
	e, _ := captureEnv(t)
	ana := joinCaptureRoom(t, e, "capture-room", "Ana", "ana@client.example", false)
	beto := joinCaptureRoom(t, e, "capture-room", "Beto", "beto@client.example", false)

	if c := ana.upload(e, "capture-room", streamA, 0, []byte("A-part0|"), "wrong"); c != http.StatusForbidden {
		t.Errorf("bad csrf: %d", c)
	}
	if c := ana.upload(e, "capture-room", streamA, 1, []byte("A-part1|"), ana.csrf); c != http.StatusConflict {
		t.Errorf("first chunk must be seq 0: %d", c)
	}
	for i, part := range []string{"A-part0|", "A-part1|", "A-part2|"} {
		if c := ana.upload(e, "capture-room", streamA, i, []byte(part), ana.csrf); c != http.StatusNoContent {
			t.Fatalf("chunk %d: %d", i, c)
		}
	}
	if c := ana.upload(e, "capture-room", streamA, 1, []byte("A-part1|"), ana.csrf); c != http.StatusNoContent {
		t.Errorf("retried chunk: %d", c)
	}
	if c := ana.upload(e, "capture-room", streamA, 5, []byte("A-part5|"), ana.csrf); c != http.StatusConflict {
		t.Errorf("gap: %d", c)
	}
	// Another participant cannot write into Ana's stream.
	if c := beto.upload(e, "capture-room", streamA, 3, []byte("intruder"), beto.csrf); c != http.StatusForbidden {
		t.Errorf("foreign stream: %d", c)
	}
	got, _ := os.ReadFile(e.app.streamPath(streamA))
	if string(got) != "A-part0|A-part1|A-part2|" {
		t.Fatalf("stream file: %q", got)
	}
	if info, _ := os.Stat(e.app.streamPath(streamA)); info.Mode().Perm() != 0o600 {
		t.Errorf("audio file mode %v", info.Mode().Perm())
	}

	// Oversized chunk, wrong type, transcription disabled.
	if c := ana.upload(e, "capture-room", streamA, 3, bytes.Repeat([]byte("x"), chunkBodyLimit+1), ana.csrf); c != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized chunk: %d", c)
	}
	req := httptest.NewRequest(http.MethodPost, testOrigin+"/m/capture-room/audio?stream="+streamB+"&seq=0&start=1", strings.NewReader("x"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-CSRF-Token", beto.csrf)
	req.Header.Set("Origin", testOrigin)
	for _, c := range beto.b.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	rr := httptest.NewRecorder()
	e.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("content type: %d", rr.Code)
	}
	e.app.DB.SetTranscription("capture-room", false)
	if c := beto.upload(e, "capture-room", streamB, 0, []byte("B"), beto.csrf); c != http.StatusConflict {
		t.Errorf("transcription off: %d", c)
	}
	// Anonymous upload.
	anon := participant{b: e.browser()}
	if c := anon.upload(e, "capture-room", streamB, 0, []byte("B"), "x"); c != http.StatusForbidden {
		t.Errorf("anonymous: %d", c)
	}
}

func TestCaptureProducesMinutesWithSpeakers(t *testing.T) {
	e, ft := captureEnv(t)
	fm := &fakeMailer{}
	e.app.Mailer = fm
	e.app.Config.AdminEmails = []string{"admin@example.com"}
	ana := joinCaptureRoom(t, e, "capture-room", "Ana", "ana@client.example", true)
	beto := joinCaptureRoom(t, e, "capture-room", "Beto", "beto@client.example", false)

	ft.byMark["ANA"] = []transcriptSegment{
		{Start: 1, Text: "Good morning, let's review the rollout plan.", AvgLogprob: -0.2},
		{Start: 20, Text: "Pablo sends the proposal on Friday.", AvgLogprob: -0.3},
		{Start: 40, Text: "Thanks for watching the video.", AvgLogprob: -0.4}, // whisper hallucination
		{Start: 50, Text: "ruido", AvgLogprob: -0.3, NoSpeechProb: 0.9},       // silence
	}
	ft.byMark["BETO"] = []transcriptSegment{
		{Start: 5, Text: "Agreed, I will check the inventory.", AvgLogprob: -0.25},
		{Start: 19.5, Text: "Pablo sends the proposal on Friday", AvgLogprob: -0.9}, // echo of Ana
	}
	if c := ana.upload(e, "capture-room", streamA, 0, []byte("ANA-audio"), ana.csrf); c != http.StatusNoContent {
		t.Fatalf("ana: %d", c)
	}
	if c := beto.upload(e, "capture-room", streamB, 0, []byte("BETO-audio"), beto.csrf); c != http.StatusNoContent {
		t.Fatalf("beto: %d", c)
	}

	// Still recording: nothing happens.
	drain(e)
	if ft.calls != 0 {
		t.Fatal("transcribed while recording")
	}
	e.advance(streamIdleClose + runIdleClose + time.Minute)
	drain(e)

	if ft.calls != 2 {
		t.Fatalf("transcriber calls: %d", ft.calls)
	}
	for _, id := range []string{streamA, streamB} {
		if _, err := os.Stat(e.app.streamPath(id)); !os.IsNotExist(err) {
			t.Errorf("audio %s not deleted after transcription", id)
		}
	}
	ts, _ := e.app.DB.ListTranscriptions("capture-room")
	if len(ts) != 1 || !strings.HasPrefix(ts[0].SessionID, "captura-") {
		t.Fatalf("transcripts: %+v", ts)
	}
	tr, _ := e.app.DB.GetTranscription(ts[0].ID)
	text, n := renderTranscript(tr.Data, e.app.Config.DisplayTimezone)
	if n != 3 {
		t.Fatalf("merged lines %d:\n%s", n, text)
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, want := range []string{"Ana: Good morning", "Beto: Agreed", "Ana: Pablo sends"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want %q", i, lines[i], want)
		}
	}
	if strings.Contains(text, "Gracias por ver") || strings.Contains(text, "ruido") {
		t.Error("hallucination or silence kept")
	}
	ms, _ := e.app.DB.ListMinutes("capture-room")
	if len(ms) != 1 || ms[0].Status != "ready" {
		t.Fatalf("minutes: %+v", ms)
	}
	if !strings.Contains(e.llm.last.Transcript, "Beto: Agreed, I will check the inventory.") {
		t.Fatalf("model input: %q", e.llm.last.Transcript)
	}
	rs, _ := e.app.DB.ProposedRecipients(&ms[0])
	got := map[string]bool{}
	for _, r := range rs {
		got[r.Email] = r.InSession
	}
	if !got["ana@client.example"] || !got["beto@client.example"] {
		t.Fatalf("speakers not proposed as confirmed recipients: %v", got)
	}
	var leftover int
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM audio_streams WHERE transcript IS NOT NULL").Scan(&leftover)
	if leftover != 0 {
		t.Error("per-stream transcripts kept after merge")
	}
	for _, s := range []string{"Good morning", "ana@client.example", "beto@client.example"} {
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("log contains %q", s)
		}
	}
	// A late chunk for a finished stream is refused.
	if c := ana.upload(e, "capture-room", streamA, 1, []byte("late"), ana.csrf); c != http.StatusGone {
		t.Errorf("late chunk: %d", c)
	}
}

func TestCaptureRetriesAndRoomDeletion(t *testing.T) {
	e, ft := captureEnv(t)
	ana := joinCaptureRoom(t, e, "capture-room", "Ana", "ana@client.example", false)
	ana.upload(e, "capture-room", streamA, 0, []byte("ANA"), ana.csrf)
	ft.err = &httpStatusError{Op: "openai audio", Code: 503}
	e.advance(streamIdleClose + runIdleClose + time.Minute)
	drain(e)
	var status, errMsg string
	e.app.DB.conn.QueryRow("SELECT status, COALESCE(error,'') FROM audio_streams").Scan(&status, &errMsg)
	if status != "closed" || errMsg != "openai audio: HTTP 503" {
		t.Fatalf("after transient error: %s %q", status, errMsg)
	}
	if _, err := os.Stat(e.app.streamPath(streamA)); err != nil {
		t.Fatal("audio removed before a successful transcription")
	}
	// Deleting the room removes the audio file and every capture row.
	b, csrf := e.loginAdminNoChange()
	if rr := b.do(http.MethodPost, "/admin/rooms/capture-room/delete", url.Values{"csrf_token": {csrf}}); rr.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", rr.Code)
	}
	if _, err := os.Stat(e.app.streamPath(streamA)); !os.IsNotExist(err) {
		t.Error("audio kept after room deletion")
	}
	var n int
	e.app.DB.conn.QueryRow("SELECT (SELECT COUNT(*) FROM audio_streams) + (SELECT COUNT(*) FROM capture_runs)").Scan(&n)
	if n != 0 {
		t.Errorf("%d capture rows left", n)
	}
}

func TestOrphanAudioPurge(t *testing.T) {
	e, _ := captureEnv(t)
	os.MkdirAll(e.app.audioDir(), 0o700)
	old := filepath.Join(e.app.audioDir(), "viejo.webm")
	os.WriteFile(old, []byte("x"), 0o600)
	os.Chtimes(old, e.now.Add(-25*time.Hour), e.now.Add(-25*time.Hour))
	fresh := filepath.Join(e.app.audioDir(), "nuevo.webm")
	os.WriteFile(fresh, []byte("x"), 0o600)
	os.Chtimes(fresh, e.now, e.now)
	if n := e.app.purgeOrphanAudio(); n != 1 {
		t.Fatalf("purged %d", n)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("fresh audio removed")
	}
}

func TestMergeAndFilters(t *testing.T) {
	base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	lines := mergeTranscripts([]timedLine{
		{At: base.Add(10 * time.Second), Name: "Beto", Text: "Hello everyone, let's start", Score: -0.8},
		{At: base.Add(9 * time.Second), Name: "Ana", Text: "Hello everyone, let's start", Score: -0.2},
		{At: base.Add(30 * time.Second), Name: "Beto", Text: "Yo tengo una duda", Score: -0.3},
	})
	if len(lines) != 2 || lines[0].Name != "Ana" || lines[1].Name != "Beto" {
		t.Fatalf("merge: %+v", lines)
	}
	for _, s := range []transcriptSegment{{Text: "  "}, {Text: "hello", NoSpeechProb: 0.8}, {Text: "Please subscribe to the channel"}, {Text: "Thank you for watching."}} {
		if keepSegment(s) {
			t.Errorf("kept %+v", s)
		}
	}
	if !keepSegment(transcriptSegment{Text: "Thanks for walking us through the quarterly numbers, Ana.", AvgLogprob: -0.3}) {
		t.Error("real sentence with 'gracias' dropped")
	}
}
