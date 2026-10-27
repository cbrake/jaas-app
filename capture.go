package main

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Browser capture: when a room has transcription enabled, each participant's
// page records their own microphone (silence while muted in the meeting) and
// uploads it in short chunks. Chunks of one page session form one "stream"
// (a single WebM/Opus file on disk). Streams of the same room that overlap in
// time form a "run" (one meeting). After the host approves a quiet run, its
// streams are transcribed with OpenAI. Window timestamps and declared channel
// names are kept distinct from measured speech/confirmed identity. Audio is
// retained under the configured audio retention policy.

const (
	chunkBodyLimit  = 1 << 20    // 15 s of 96 kbps Opus is approximately 180 KB
	streamMaxBytes  = 24_000_000 // margin below the provider's 25,000,000-byte limit
	streamIdleClose = 2 * time.Minute
	runIdleClose    = 5 * time.Minute
	runMaxGap       = 30 * time.Minute
)

var streamIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

func (app *App) audioDir() string {
	return filepath.Join(filepath.Dir(app.Config.DBPath), "audio")
}

func (app *App) streamPath(id string) string {
	return filepath.Join(app.audioDir(), id+".webm")
}

// --- upload endpoint ---

// handleAudioChunk: POST /m/{slug}/audio?stream=ID&seq=N&start=MS with the raw
// chunk as body. Requires the participant session of this room, its CSRF
// token (X-CSRF-Token) and a room with transcription enabled.
func (app *App) handleAudioChunk(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	p, err := app.DB.GetParticipantSession(app.cookieValue(r, "tgm_part"))
	if err != nil || p.RoomSlug != slug || !tokensEqual(r.Header.Get("X-CSRF-Token"), p.CSRFToken) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	room, err := app.DB.GetRoom(slug)
	if err != nil || !room.Transcription {
		http.Error(w, "Transcription disabled", http.StatusConflict)
		return
	}
	q := r.URL.Query()
	hints := CaptureHints{Language: q.Get("language"), MetadataJSON: q.Get("metadata")}
	switch q.Get("shared") {
	case "", "0":
	case "1":
		hints.SharedMicrophone = true
	default:
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if rawEnd := q.Get("end"); rawEnd != "" {
		endMs, err := strconv.ParseInt(rawEnd, 10, 64)
		if err != nil || endMs < 0 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		hints.EndedAt = time.UnixMilli(endMs)
	}
	if _, err := normalizeCaptureHints(hints); err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	streamID := q.Get("stream")
	seq, err1 := strconv.Atoi(q.Get("seq"))
	startMs, err2 := strconv.ParseInt(q.Get("start"), 10, 64)
	if !streamIDPattern.MatchString(streamID) || err1 != nil || err2 != nil || seq < 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "audio/webm") {
		http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
		return
	}
	if len(data) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch err := app.AppendChunkWithMetadata(p, streamID, seq, time.UnixMilli(startMs), data, hints); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errChunkGap), errors.Is(err, errChunkConflict):
		http.Error(w, "Conflict", http.StatusConflict)
	case errors.Is(err, errChunkForbidden):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, errChunkClosed), errors.Is(err, errChunkTooLarge):
		http.Error(w, "Gone", http.StatusGone)
	case errors.Is(err, errAudioQuota):
		http.Error(w, "Audio storage quota reached", http.StatusInsufficientStorage)
	default:
		app.serverError(w, "audio chunk", err)
	}
}

// handleFinalizeAudio verifies the sender's terminal manifest. Idle timeout
// only stops accepting new chunks; it never proves capture completeness.
func (app *App) handleFinalizeAudio(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	p, err := app.DB.GetParticipantSession(app.cookieValue(r, "tgm_part"))
	if !validSlug(slug) || err != nil || p.RoomSlug != slug || !tokensEqual(r.Header.Get("X-CSRF-Token"), p.CSRFToken) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	id := q.Get("stream")
	seq, e1 := strconv.Atoi(q.Get("seq"))
	size, e2 := strconv.ParseInt(q.Get("bytes"), 10, 64)
	end, e3 := strconv.ParseInt(q.Get("end"), 10, 64)
	if !streamIDPattern.MatchString(id) || e1 != nil || e2 != nil || e3 != nil || seq < 1 || size < 1 || end < 0 {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	switch err := app.FinalizeStream(p, id, seq, size, time.UnixMilli(end)); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, errChunkForbidden), errors.Is(err, ErrNotFound):
		http.Error(w, "Forbidden", http.StatusForbidden)
	case errors.Is(err, ErrCaptureIncomplete), errors.Is(err, ErrCaptureCancelled), errors.Is(err, errChunkClosed):
		http.Error(w, "Capture requires review", http.StatusConflict)
	default:
		app.serverError(w, "finalize audio", err)
	}
}

// --- merging ---

// Whisper is known to "hear" stock phrases in silence (muted periods are sent
// as silence); drop segments that are likely not speech.
var hallucinations = []string{
	"gracias por ver", "suscríbete", "suscribete", "subtítulos realizados por", "subtitulos realizados por",
	"amara.org", "gracias por su atención", "¡gracias!", "gracias.",
}

func keepSegment(s transcriptSegment) bool {
	text := strings.TrimSpace(s.Text)
	// File-transcription windows have no Whisper acoustic confidence fields.
	// Preserve legitimate short acknowledgements; quality gates may hold a
	// draft for review but must not silently rewrite the source transcript.
	if s.TimePrecision == "window" {
		return text != ""
	}
	if text == "" || s.NoSpeechProb > 0.6 || (s.AvgLogprob != 0 && s.AvgLogprob < -1.2) {
		return false
	}
	l := strings.ToLower(text)
	for _, h := range hallucinations {
		if l == h || (len(l) < 40 && strings.Contains(l, h)) {
			return false
		}
	}
	return true
}

type timedLine struct {
	At            time.Time
	Dur           time.Duration
	Name          string
	Text          string
	Score         float64
	TimePrecision string
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) > 2 {
			out[w] = true
		}
	}
	return out
}

func similar(a, b string) bool {
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return false
	}
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	small := len(wa)
	if len(wb) < small {
		small = len(wb)
	}
	return float64(inter)/float64(small) >= 0.7
}

// mergeTranscripts orders all speakers' lines by wall-clock time and drops
// echoes: the same words picked up by another participant's microphone at
// nearly the same moment are kept only once, under the clearest recording.
func mergeTranscripts(lines []timedLine) []timedLine {
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].At.Before(lines[j].At) })
	drop := make([]bool, len(lines))
	for i := range lines {
		for j := i + 1; j < len(lines) && lines[j].At.Sub(lines[i].At) <= 4*time.Second; j++ {
			if lines[i].Name == lines[j].Name || drop[i] || drop[j] || !similar(lines[i].Text, lines[j].Text) {
				continue
			}
			if lines[i].Score >= lines[j].Score {
				drop[j] = true
			} else {
				drop[i] = true
			}
		}
	}
	out := lines[:0:0]
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// deleteRoomAudio removes the audio files of a room before its rows go.
func (app *App) deleteRoomAudio(slug string) {
	rows, err := app.DB.conn.Query("SELECT id FROM audio_streams WHERE room_slug = ?", slug)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		os.Remove(app.streamPath(id))
		os.Remove(app.resultPath(id))
		os.Remove(app.requestPath(id))
	}
}

// captureSpeakers: participants who uploaded audio for a capture session.
func (db *DB) captureSpeakers(session string) ([][2]string, error) {
	runID, ok := strings.CutPrefix(session, "captura-")
	if !ok {
		return nil, nil
	}
	rows, err := db.conn.Query("SELECT DISTINCT email, display_name FROM audio_streams WHERE run_id = ? AND email != ''", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var e, n string
		if err := rows.Scan(&e, &n); err != nil {
			return nil, err
		}
		out = append(out, [2]string{e, n})
	}
	return out, rows.Err()
}
