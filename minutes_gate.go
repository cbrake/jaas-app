package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

var errAnalysisCancelled = permanent("análisis cancelado; el consumo anterior no se recupera")

type minutesUsageKey struct{}
type beforeMinutesKey struct{}

func observeMinutesUsage(ctx context.Context, id string, data []byte) {
	if f, ok := ctx.Value(minutesUsageKey{}).(func(string, []byte)); ok {
		f(id, data)
	}
}
func safeUsage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var u struct {
		InputTokens  *int64 `json:"input_tokens,omitempty"`
		OutputTokens *int64 `json:"output_tokens,omitempty"`
		TotalTokens  *int64 `json:"total_tokens,omitempty"`
		InputDetails *struct {
			CachedTokens *int64 `json:"cached_tokens,omitempty"`
		} `json:"input_tokens_details,omitempty"`
		OutputDetails *struct {
			ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
		} `json:"output_tokens_details,omitempty"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return nil
	}
	b, _ := json.Marshal(u)
	return b
}

func (db *DB) minutesCancelled(id int64) bool {
	var cancelled sql.NullInt64
	if err := db.conn.QueryRow("SELECT cancelled_at FROM minutes WHERE id = ?", id).Scan(&cancelled); err != nil {
		return true // fail closed when state cannot be read
	}
	return cancelled.Valid
}

func (db *DB) CancelMinutes(id int64) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE minutes SET cancelled_at = COALESCE(cancelled_at, ?), updated_at = ?, locked_until = NULL WHERE id = ?", db.unix(), db.unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	for _, table := range []string{"minutes_deliveries", "host_notifications"} {
		if _, err := tx.Exec("UPDATE "+table+" SET status='failed', error='análisis cancelado' WHERE minutes_id=? AND status='queued'", id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (app *App) minutesContext(parent context.Context, id int64) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	app.minutesMu.Lock()
	if app.minuteCancels == nil {
		app.minuteCancels = make(map[int64]context.CancelFunc)
	}
	app.minuteCancels[id] = cancel
	app.minutesMu.Unlock()
	if app.DB.minutesCancelled(id) {
		cancel()
	}
	return ctx, func() {
		app.minutesMu.Lock()
		delete(app.minuteCancels, id)
		app.minutesMu.Unlock()
		cancel()
	}
}

func (app *App) cancelMinutesWork(id int64) {
	app.minutesMu.Lock()
	defer app.minutesMu.Unlock()
	if cancel := app.minuteCancels[id]; cancel != nil {
		cancel()
	}
}

func normalEvidence(s string) string {
	return strings.Join(strings.Fields(strings.Trim(s, " \t\n\r\"“”")), " ")
}

// Evidence is a literal containment check, not a claim that an LLM's
// interpretation is true. Acceptance and completion still need host review.
func validateEvidence(r *MinutesResult, raw string) error {
	us := parseUtterances(raw)
	if len(us) == 0 {
		return permanent("sin evidencia textual verificable")
	}
	known := make(map[string]bool)
	for _, u := range us {
		known[u.Name] = true
	}
	contains := func(speaker, quote string) bool {
		q := normalEvidence(quote)
		if prefix, rest, ok := strings.Cut(q, ":"); ok && strings.TrimSpace(prefix) == speaker {
			q = normalEvidence(rest)
		}
		if q == "" {
			return false
		}
		for _, u := range us {
			if speaker != "" && u.Name != speaker {
				continue
			}
			if strings.Contains(normalEvidence(u.Text), q) {
				return true
			}
		}
		return false
	}
	for _, a := range r.ActionItems {
		speaker, quote, ok := strings.Cut(a.Evidence, ":")
		if !ok || !known[strings.TrimSpace(speaker)] || !contains(strings.TrimSpace(speaker), quote) {
			return permanent("una tarea contiene evidencia no verificable; requiere revisión")
		}
		if a.Owner != nil && !known[*a.Owner] {
			return permanent("responsable no confirmado en la transcripción")
		}
	}
	seen := map[string]bool{}
	for _, s := range r.Sentiment.BySpeaker {
		if seen[s.Speaker] || !known[s.Speaker] || !contains(s.Speaker, s.Evidence) {
			return permanent("evidencia de postura por participante no verificable")
		}
		seen[s.Speaker] = true
	}
	for _, m := range r.Sentiment.Moments {
		if !known[m.Speaker] || !contains(m.Speaker, m.Quote) {
			return permanent("cita de postura no verificable")
		}
	}
	for _, s := range r.CommercialSignals {
		if !known[s.Speaker] || !contains(s.Speaker, s.Evidence) {
			return permanent("señal comercial sin cita literal verificable; requiere revisión")
		}
	}
	return nil
}

// Cheap deterministic gates operate after ASR, before the summary call.
// They do not retroactively erase the audio transcription charge.
func transcriptQuality(raw string) error {
	var header struct {
		Source  string `json:"source"`
		Quality string `json:"quality"`
	}
	_ = json.Unmarshal([]byte(raw), &header)
	if header.Quality == "partial" {
		return permanent("captura parcial: requiere revisión antes de generar minuta")
	}
	us := parseUtterances(raw)
	if len(us) == 0 {
		return permanent("sin contenido para analizar")
	}
	words, acknowledgements, contextOnly := 0, 0, 0
	for _, u := range us {
		ws := strings.Fields(u.Text)
		words += len(ws)
		clean := strings.ToLower(strings.TrimFunc(u.Text, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) }))
		if clean == "sí" || clean == "si" || clean == "ajá" || clean == "ok" || clean == "bueno, bueno" {
			acknowledgements++
		}
		if strings.Contains(clean, "términos de trabajo de transgenia") {
			contextOnly++
		}
		streak := 0
		previous := ""
		for _, w := range ws {
			w = strings.ToLower(strings.TrimFunc(w, unicode.IsPunct))
			if w == previous {
				streak++
			} else {
				streak = 1
			}
			previous = w
			if streak >= 12 {
				return permanent("repetición anómala: revisar transcripción")
			}
		}
	}
	if len(us) >= 10 && acknowledgements*100 >= len(us)*80 {
		return permanent("predominan segmentos repetitivos: revisar transcripción")
	}
	if contextOnly*2 >= len(us) {
		return permanent("posible texto de contexto en la transcripción")
	}
	if header.Source == "browser-capture" && words < 12 {
		return permanent("contenido insuficiente para una minuta de trabajo")
	}
	return nil
}

func unmeasuredTranscript(raw string) bool {
	var doc struct {
		Messages []struct {
			TimePrecision   string `json:"time_precision"`
			SpeakerVerified *bool  `json:"speaker_verified"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return true
	}
	for _, m := range doc.Messages {
		if m.TimePrecision == "window" || (m.SpeakerVerified != nil && !*m.SpeakerVerified) {
			return true
		}
	}
	return false
}

func isCancellation(err error) bool { return errors.Is(err, errAnalysisCancelled) }
