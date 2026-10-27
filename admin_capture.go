package main

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed visualization/*
var visualizationFS embed.FS

type sessionFilter struct {
	Query, From, To, Client, Project, Organizer, Status string
	Page                                                int
}
type sessionRow struct {
	CaptureID, MinutesID                              int64
	Room, Session, Client, Project, Organizer, Status string
	Started                                           time.Time
	AudioBytes                                        int64
	Shared                                            bool
}

// UNION includes historical JaaS minutes without pretending they have local audio.
const sessionsSQL = `WITH sessions AS (
 SELECT r.id capture_id,COALESCE(m.id,0) minutes_id,r.room_slug room,'captura-'||r.id session,
 r.client_name client,r.project_name project,COALESCE(NULLIF(r.organizer_email,''),o.owner_email) organizer,
 CASE WHEN r.cancelled_at IS NOT NULL OR m.cancelled_at IS NOT NULL THEN 'cancelled'
 WHEN m.status IS NOT NULL THEN m.status WHEN r.status='failed' THEN 'failed'
 WHEN r.processing_approved_at IS NULL THEN 'pending' ELSE 'processing' END state,
 r.started_at started,r.shared_microphone shared,
 (SELECT COALESCE(SUM(s.bytes),0) FROM audio_streams s WHERE s.run_id=r.id AND s.purged_at IS NULL) audio_bytes,o.owner_email owner
 FROM capture_runs r JOIN rooms o ON o.slug=r.room_slug
 LEFT JOIN minutes m ON m.room_slug=r.room_slug AND m.session_id='captura-'||r.id
 UNION ALL
 SELECT 0,m.id,m.room_slug,m.session_id,'','',o.owner_email,
 CASE WHEN m.cancelled_at IS NOT NULL THEN 'cancelled' ELSE m.status END,m.meeting_at,0,0,o.owner_email
 FROM minutes m JOIN rooms o ON o.slug=m.room_slug
 WHERE NOT EXISTS(SELECT 1 FROM capture_runs r WHERE r.room_slug=m.room_slug AND m.session_id='captura-'||r.id)
) SELECT capture_id,minutes_id,room,session,client,project,organizer,state,started,shared,audio_bytes FROM sessions WHERE 1=1`

func dateBound(value string, loc *time.Location, end bool) (int64, error) {
	if value == "" {
		return 0, nil
	}
	t, err := time.ParseInLocation("2006-01-02", value, loc)
	if err != nil {
		return 0, errors.New("fecha inválida")
	}
	if end {
		t = t.AddDate(0, 0, 1)
	}
	return t.Unix(), nil
}

func (db *DB) searchSessions(f sessionFilter, owner string, all bool, loc *time.Location) ([]sessionRow, bool, error) {
	from, err := dateBound(f.From, loc, false)
	if err != nil {
		return nil, false, err
	}
	to, err := dateBound(f.To, loc, true)
	if err != nil {
		return nil, false, err
	}
	if from > 0 && to > 0 && from >= to {
		return nil, false, errors.New("rango de fechas inválido")
	}
	q := sessionsSQL
	var args []any
	if !all {
		q += " AND owner=?"
		args = append(args, owner)
	}
	if from > 0 {
		q += " AND started>=?"
		args = append(args, from)
	}
	if to > 0 {
		q += " AND started<?"
		args = append(args, to)
	}
	like := func(s string) string {
		return "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(s) + "%"
	}
	for _, field := range []struct{ name, value string }{{"client", f.Client}, {"project", f.Project}, {"organizer", f.Organizer}} {
		if field.value != "" {
			q += " AND " + field.name + " LIKE ? ESCAPE '\\'"
			args = append(args, like(field.value))
		}
	}
	if f.Query != "" {
		q += " AND (room LIKE ? ESCAPE '\\' OR session LIKE ? ESCAPE '\\')"
		args = append(args, like(f.Query), like(f.Query))
	}
	if f.Status != "" {
		q += " AND state=?"
		args = append(args, f.Status)
	}
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Page > 100000 {
		return nil, false, errors.New("página inválida")
	}
	q += " ORDER BY started DESC,capture_id DESC,minutes_id DESC LIMIT 26 OFFSET ?"
	args = append(args, (f.Page-1)*25)
	rows, err := db.conn.Query(q, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		var at int64
		if err := rows.Scan(&s.CaptureID, &s.MinutesID, &s.Room, &s.Session, &s.Client, &s.Project, &s.Organizer, &s.Status, &at, &s.Shared, &s.AudioBytes); err != nil {
			return nil, false, err
		}
		s.Started = time.Unix(at, 0)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > 25
	if more {
		out = out[:25]
	}
	return out, more, nil
}

func (app *App) handleCaptureSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	f := sessionFilter{q.Get("q"), q.Get("from"), q.Get("to"), q.Get("client"), q.Get("project"), q.Get("organizer"), q.Get("status"), page}
	for _, v := range []string{f.Query, f.Client, f.Project, f.Organizer} {
		if len(v) > 254 {
			http.Error(w, "Filtro demasiado largo", 400)
			return
		}
	}
	s := adminFrom(r)
	rows, more, err := app.DB.searchSessions(f, s.Email, s.Superadmin, app.Config.DisplayTimezone)
	if err != nil {
		http.Error(w, "No se pudo consultar el rango o los filtros", 400)
		return
	}
	link := func(p int) string {
		v := r.URL.Query()
		v.Set("page", strconv.Itoa(p))
		return "/admin/captures?" + v.Encode()
	}
	previous := ""
	next := ""
	if page > 1 {
		previous = link(page - 1)
	}
	if more {
		next = link(page + 1)
	}
	app.render(w, 200, "captures.html", map[string]any{"Sessions": rows, "Filter": f, "CSRF": s.CSRFToken, "Next": next, "Previous": previous, "Notice": q.Get("notice")})
}

func (app *App) managedCapture(w http.ResponseWriter, r *http.Request) (*captureRun, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return nil, false
	}
	run, err := app.DB.GetCaptureRun(id)
	if err != nil || !app.adminRoomBySlug(w, r, runSlug(run)) {
		if err != nil {
			http.NotFound(w, r)
		}
		return nil, false
	}
	return run, true
}
func runSlug(r *captureRun) string {
	if r == nil {
		return ""
	}
	return r.RoomSlug
}
func captureRedirect(w http.ResponseWriter, r *http.Request, n string) {
	http.Redirect(w, r, "/admin/captures?notice="+url.QueryEscape(n), http.StatusSeeOther)
}

func (app *App) handleApproveCapture(w http.ResponseWriter, r *http.Request) {
	run, ok := app.managedCapture(w, r)
	if !ok {
		return
	}
	if err := app.DB.ApproveCapture(run.ID); err != nil {
		http.Error(w, "No se puede procesar: termina la captura, espera el cierre de sus pistas y revisa su estado.", 409)
		return
	}
	app.wakeWorker()
	captureRedirect(w, r, "Procesamiento aprobado. Las llamadas de IA generan consumo.")
}
func (app *App) handleCancelCapture(w http.ResponseWriter, r *http.Request) {
	run, ok := app.managedCapture(w, r)
	if !ok {
		return
	}
	app.effectsMu.Lock()
	err := app.DB.CancelCapture(run.ID)
	var id int64
	if app.DB.conn.QueryRow("SELECT id FROM minutes WHERE room_slug=? AND session_id=?", run.RoomSlug, fmt.Sprintf("captura-%d", run.ID)).Scan(&id) == nil {
		app.cancelMinutesWork(id)
	}
	app.effectsMu.Unlock()
	if err != nil {
		app.serverError(w, "cancel capture", err)
		return
	}
	captureRedirect(w, r, "Análisis cancelado. El audio sigue sujeto a retención. Las llamadas y correos ya iniciados no se recuperan.")
}
func (app *App) handleCaptureDetails(w http.ResponseWriter, r *http.Request) {
	run, ok := app.managedCapture(w, r)
	if !ok {
		return
	}
	client := strings.TrimSpace(r.PostFormValue("client"))
	project := strings.TrimSpace(r.PostFormValue("project"))
	if len(client) > 120 || len(project) > 120 {
		http.Error(w, "Cliente o proyecto demasiado largo", 400)
		return
	}
	// Organizer is the authenticated room owner, never a caller-supplied identity.
	room, err := app.DB.GetRoom(run.RoomSlug)
	if err != nil {
		app.serverError(w, "capture room", err)
		return
	}
	if err := app.DB.UpdateCaptureDetails(run.ID, client, project, room.OwnerEmail); err != nil {
		app.serverError(w, "capture details", err)
		return
	}
	captureRedirect(w, r, "Referencia de cliente y proyecto guardada.")
}
func (app *App) handleCancelMinutes(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, err := app.DB.GetMinutes(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !app.adminRoomBySlug(w, r, m.RoomSlug) {
		return
	}
	app.effectsMu.Lock()
	if tail, ok := strings.CutPrefix(m.SessionID, "captura-"); ok {
		if runID, e := strconv.ParseInt(tail, 10, 64); e == nil {
			err = app.DB.CancelCapture(runID)
		}
	}
	if err == nil {
		err = app.DB.CancelMinutes(id)
	}
	app.cancelMinutesWork(id)
	app.effectsMu.Unlock()
	if err != nil {
		app.serverError(w, "cancel minutes", err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/admin/minutes/%d", id), 303)
}

type audioRow struct {
	ID     string
	Bytes  int64
	Purged bool
}

func (app *App) handleCaptureAudioList(w http.ResponseWriter, r *http.Request) {
	run, ok := app.managedCapture(w, r)
	if !ok {
		return
	}
	rows, err := app.DB.conn.Query("SELECT id,bytes,purged_at IS NOT NULL FROM audio_streams WHERE run_id=? ORDER BY started_at", run.ID)
	if err != nil {
		app.serverError(w, "audio list", err)
		return
	}
	defer rows.Close()
	var files []audioRow
	for rows.Next() {
		var a audioRow
		if err := rows.Scan(&a.ID, &a.Bytes, &a.Purged); err != nil {
			app.serverError(w, "audio rows", err)
			return
		}
		files = append(files, a)
	}
	if err := rows.Err(); err != nil {
		app.serverError(w, "audio rows", err)
		return
	}
	app.render(w, 200, "capture_audio.html", map[string]any{"Run": run, "Files": files})
}
func (app *App) handlePrivateAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("stream")
	if !streamIDPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	var room string
	var purged bool
	if err := app.DB.conn.QueryRow("SELECT room_slug,purged_at IS NOT NULL FROM audio_streams WHERE id=?", id).Scan(&room, &purged); err != nil {
		http.NotFound(w, r)
		return
	}
	if !app.adminRoomBySlug(w, r, room) {
		return
	}
	if purged {
		http.Error(w, "Audio expirado por retención", http.StatusGone)
		return
	}
	path := app.streamPath(id)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	setSecurityHeaders(w, false)
	w.Header().Set("Content-Type", "audio/webm")
	w.Header().Set("Content-Disposition", `attachment; filename="captura.webm"`)
	http.ServeContent(w, r, "captura.webm", time.Time{}, f)
}
func (app *App) handleVisualizations(w http.ResponseWriter, r *http.Request) {
	app.serveVisualization(w, r, "index.html", "text/html; charset=utf-8")
}
func (app *App) handleVisualizationAsset(w http.ResponseWriter, r *http.Request) {
	types := map[string]string{"app.js": "application/javascript; charset=utf-8", "app.css": "text/css; charset=utf-8"}
	name := r.PathValue("asset")
	ct, ok := types[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	app.serveVisualization(w, r, name, ct)
}
func (app *App) serveVisualization(w http.ResponseWriter, r *http.Request, name, ct string) {
	b, err := visualizationFS.ReadFile("visualization/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	setSecurityHeaders(w, false)
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}
