package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Retention is a time-based purge window: calendar months when Months > 0,
// otherwise days; zero keeps data forever.
type Retention struct {
	Days   int
	Months int
}

// Cutoff returns the creation time before which data is purged.
func (r Retention) Cutoff(now time.Time) (time.Time, bool) {
	switch {
	case r.Months > 0:
		return now.AddDate(0, -r.Months, 0), true
	case r.Days > 0:
		return now.AddDate(0, 0, -r.Days), true
	}
	return time.Time{}, false
}

func (r Retention) String() string {
	switch {
	case r.Months > 0:
		return fmt.Sprintf("%d months", r.Months)
	case r.Days > 0:
		return fmt.Sprintf("%d days", r.Days)
	}
	return "forever"
}

// Retention policy (see TRANSCRIPT_RETENTION_MONTHS/_DAYS and
// MINUTES_RETENTION_MONTHS/_DAYS; Transgenia runs 9 months for both):
//   - deleting a room deletes its recordings, transcripts, minutes and
//     participant sessions immediately (DB.DeleteRoom);
//   - recording links are dropped when they expire (24 h, JaaS default);
//   - pre-authenticated transcript links are cleared once used or expired;
//   - transcripts are deleted after the transcript retention (default 90 days);
//   - minutes are deleted after the minutes retention (default 365 days);
//   - webhook idempotency keys are kept 30 days;
//   - expired admin and participant sessions are purged.
func (db *DB) PurgeExpired(transcripts, minutes Retention) (map[string]int64, error) {
	now := db.now()
	out := map[string]int64{}
	run := func(name, q string, args ...any) error {
		res, err := db.conn.Exec(q, args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		out[name] = n
		return nil
	}
	steps := []struct {
		name string
		q    string
		args []any
	}{
		{"recordings", "DELETE FROM recordings WHERE expires_at <= ?", []any{now.Unix()}},
		{"source_links", "UPDATE minutes SET source_url = NULL WHERE source_url IS NOT NULL AND source_expires_at <= ?", []any{now.Unix()}},
		{"webhook_events", "DELETE FROM webhook_events WHERE received_at < ?", []any{now.AddDate(0, 0, -30).Unix()}},
		{"admin_sessions", "DELETE FROM admin_sessions WHERE expires_at <= ? OR last_seen_at < ?", []any{now.Unix(), now.Add(-adminIdleTimeout).Unix()}},
		{"participant_sessions", "DELETE FROM participant_sessions WHERE expires_at <= ?", []any{now.Unix()}},
	}
	if cutoff, ok := transcripts.Cutoff(now); ok {
		steps = append(steps, struct {
			name string
			q    string
			args []any
		}{"transcriptions", "DELETE FROM transcriptions WHERE created_at < ?", []any{cutoff.Unix()}},
			struct {
				name string
				q    string
				args []any
			}{"session_participants", "DELETE FROM session_participants WHERE joined_at < ?", []any{cutoff.Unix()}},
			struct {
				name string
				q    string
				args []any
			}{"capture_runs", "DELETE FROM capture_runs WHERE started_at < ? AND status IN ('done','failed')", []any{cutoff.Unix()}})
	}
	if cutoff, ok := minutes.Cutoff(now); ok {
		steps = append(steps, struct {
			name string
			q    string
			args []any
		}{"minutes", "DELETE FROM minutes WHERE created_at < ? AND status IN ('ready','failed')", []any{cutoff.Unix()}})
	}
	for _, s := range steps {
		if err := run(s.name, s.q, s.args...); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (app *App) runRetention(ctx context.Context) {
	purge := func() {
		app.DB.DeactivateExpiredRooms(app.maxMeeting())
		res, err := app.DB.PurgeExpired(app.Config.TranscriptRetention, app.Config.MinutesRetention)
		if err != nil {
			slog.Error("retention: purge failed", "error", err.Error())
			return
		}
		if n := app.purgeOrphanAudio(); n > 0 {
			res["audio_files"] = int64(n)
		}
		for k, n := range res {
			if n > 0 {
				slog.Info("retention: purged", "what", k, "rows", n)
			}
		}
	}
	purge()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}
