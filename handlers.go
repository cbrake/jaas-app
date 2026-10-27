package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// assetVersions maps each embedded static file to a short content hash.
// Pages reference /static/<file>?v=<hash>, so a deploy changes the URL of
// every changed asset and the CDN never serves a stale script or stylesheet.
var assetVersions = func() map[string]string {
	out := map[string]string{}
	entries, _ := staticFS.ReadDir("static")
	for _, e := range entries {
		b, err := staticFS.ReadFile("static/" + e.Name())
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = hex.EncodeToString(sum[:])[:12]
	}
	return out
}()

func assetURL(name string) string {
	return "/static/" + name + "?v=" + assetVersions[name]
}

// staticHandler: the current versioned URL is cacheable for a year; any
// other request (unversioned or an old version) is cached briefly only.
func staticHandler() http.Handler {
	fs := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/static/")
		if v := r.URL.Query().Get("v"); v != "" && v == assetVersions[name] {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=60")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		fs.ServeHTTP(w, r)
	})
}

func (app *App) templateFuncs() template.FuncMap {
	return template.FuncMap{
		"asset":   assetURL,
		"version": func() string { return version },
		"audioRetentionDays": func() int {
			if app.Config.AudioRetentionDays > 0 {
				return app.Config.AudioRetentionDays
			}
			return 30
		},
		"sessionStatus": func(s string) string {
			labels := map[string]string{"pending": "Pendiente de procesar", "queued": "En cola", "processing": "Procesando", "ready": "Lista para revisar", "failed": "Revisión requerida", "cancelled": "Cancelada"}
			if label, ok := labels[s]; ok {
				return label
			}
			return "Revisión requerida"
		},
		"local": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(app.Config.DisplayTimezone).Format("2006-01-02 15:04")
		},
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
	}
}

func (app *App) loadTemplates() error {
	t, err := template.New("").Funcs(app.templateFuncs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	app.templates = t
	return nil
}

func (app *App) render(w http.ResponseWriter, status int, name string, data map[string]any) {
	setSecurityHeaders(w, name == "meeting.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := app.templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render failed", "template", name)
	}
}

func (app *App) serverError(w http.ResponseWriter, where string, err error) {
	slog.Error("internal error", "where", where, "error", err.Error())
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

func (app *App) RegisterRoutes(mux *http.ServeMux) {
	admin := func(h http.HandlerFunc) http.Handler { return app.requireAdmin(h, false) }
	adminPw := func(h http.HandlerFunc) http.Handler { return app.requireAdmin(h, true) }
	cf := app.requireCFAccess

	mux.Handle("GET /static/", staticHandler())
	mux.HandleFunc("GET /healthz", app.handleHealth)
	mux.HandleFunc("GET /{$}", app.handleHome)

	mux.Handle("GET /admin/login", cf(http.HandlerFunc(app.handleLoginPage)))
	mux.Handle("POST /admin/login", cf(http.HandlerFunc(app.handleLogin)))
	mux.Handle("POST /admin/logout", cf(http.HandlerFunc(app.handleLogout)))
	mux.Handle("GET /admin/password", cf(adminPw(app.handlePasswordPage)))
	mux.Handle("POST /admin/password", cf(adminPw(app.handlePasswordChange)))
	mux.Handle("GET /admin", cf(admin(app.handleDashboard)))
	mux.Handle("GET /admin/captures", cf(admin(app.handleCaptureSearch)))
	mux.Handle("POST /admin/captures/{id}/approve", cf(admin(app.handleApproveCapture)))
	mux.Handle("POST /admin/captures/{id}/cancel", cf(admin(app.handleCancelCapture)))
	mux.Handle("POST /admin/captures/{id}/details", cf(admin(app.handleCaptureDetails)))
	mux.Handle("GET /admin/captures/{id}/audio", cf(admin(app.handleCaptureAudioList)))
	mux.Handle("GET /admin/audio/{stream}", cf(admin(app.handlePrivateAudio)))
	mux.Handle("POST /admin/minutes/{id}/cancel", cf(admin(app.handleCancelMinutes)))
	mux.Handle("GET /visualizations", cf(admin(app.handleVisualizations)))
	mux.Handle("GET /visualizations/{$}", cf(admin(app.handleVisualizations)))
	mux.Handle("GET /visualizations/assets/{asset}", cf(admin(app.handleVisualizationAsset)))
	mux.Handle("POST /admin/rooms", cf(admin(app.handleCreateRoom)))
	mux.Handle("POST /admin/rooms/{slug}/delete", cf(admin(app.handleDeleteRoom)))
	mux.Handle("POST /admin/rooms/{slug}/start", cf(admin(app.handleAdminStart)))
	mux.Handle("POST /admin/rooms/{slug}/stop", cf(admin(app.handleAdminStop)))
	mux.Handle("POST /admin/rooms/{slug}/transcription", cf(admin(app.handleToggleTranscription)))
	mux.Handle("GET /admin/transcriptions/{id}", cf(admin(app.handleViewTranscription)))
	mux.Handle("GET /admin/minutes/{id}", cf(admin(app.handleViewMinutes)))
	mux.Handle("GET /admin/minutes/{id}/download", cf(admin(app.handleDownloadMinutes)))
	mux.Handle("POST /admin/minutes/{id}/retry", cf(admin(app.handleRetryMinutes)))
	mux.Handle("POST /admin/minutes/{id}/send", cf(admin(app.handleSendMinutes)))

	mux.HandleFunc("GET /m/{slug}", app.handleMeeting)
	mux.HandleFunc("POST /m/{slug}/join", app.handleJoinMeeting)
	mux.HandleFunc("POST /m/{slug}/end", app.handleEndMeeting)
	mux.HandleFunc("POST /m/{slug}/audio", app.handleAudioChunk)
	mux.HandleFunc("POST /m/{slug}/audio/finalize", app.handleFinalizeAudio)

	mux.HandleFunc("POST /webhook/recording", app.handleRecordingWebhook)
}

// --- middleware ---

func (app *App) requireCFAccess(next http.Handler) http.Handler {
	if app.CFAccess == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email, err := app.CFAccess.Verify(r.Context(), r.Header.Get("Cf-Access-Jwt-Assertion"))
		if err != nil {
			slog.Warn("admin: cloudflare access rejected", "ip", app.clientIP(r))
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accessEmailKey, email)))
	})
}

type ctxKey int

const (
	adminSessionKey ctxKey = iota + 1
	accessEmailKey
)

// identityMode: Cloudflare Access authenticates each person and the app
// scopes rooms to their e-mail. Without Access the panel falls back to the
// single admin password (break-glass / local development).
func (app *App) identityMode() bool { return app.CFAccess != nil }

func accessEmail(r *http.Request) string {
	e, _ := r.Context().Value(accessEmailKey).(string)
	return e
}

func (app *App) isSuperadmin(email string) bool {
	return slices.Contains(app.Config.AdminEmails, email)
}

// requireAdmin validates the server-side session, enforces the CSRF token on
// every POST and, until the temporary password is replaced, confines the
// session to the password page. In identity mode the session is created from
// the verified Access identity and discarded if that identity changes.
func (app *App) requireAdmin(next http.HandlerFunc, passwordPage bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.identityMode() {
			app.requireIdentity(w, r, next, passwordPage)
			return
		}
		s, err := app.DB.GetAdminSession(app.cookieValue(r, "tgm_admin"))
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				app.serverError(w, "admin session", err)
				return
			}
			app.clearCookie(w, "tgm_admin")
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && !tokensEqual(r.PostFormValue("csrf_token"), s.CSRFToken) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if s.MustChange && !passwordPage {
			http.Redirect(w, r, "/admin/password", http.StatusSeeOther)
			return
		}
		s.Superadmin = true // password mode has a single administrator
		next.ServeHTTP(w, r.WithContext(contextWithAdmin(r, s)))
	})
}

func (app *App) requireIdentity(w http.ResponseWriter, r *http.Request, next http.HandlerFunc, passwordPage bool) {
	email := accessEmail(r)
	if email == "" { // requireCFAccess must run first
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if passwordPage {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	token := app.cookieValue(r, "tgm_admin")
	s, err := app.DB.GetAdminSession(token)
	if err != nil && !errors.Is(err, ErrNotFound) {
		app.serverError(w, "admin session", err)
		return
	}
	if err == nil && s.Email != email {
		app.DB.DeleteAdminSession(token) // Access identity changed: drop the old session
		s = nil
	}
	if s == nil || err != nil {
		if r.Method == http.MethodPost { // no valid CSRF token can exist yet
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		token, err = app.DB.CreateAdminSession(email)
		if err != nil {
			app.serverError(w, "admin session", err)
			return
		}
		if s, err = app.DB.GetAdminSession(token); err != nil {
			app.serverError(w, "admin session", err)
			return
		}
		app.setCookie(w, "tgm_admin", token, adminAbsoluteTimeout)
		slog.Info("admin: session", "user", email)
	}
	if r.Method == http.MethodPost && !tokensEqual(r.PostFormValue("csrf_token"), s.CSRFToken) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	s.MustChange = false
	s.Superadmin = app.isSuperadmin(email)
	next.ServeHTTP(w, r.WithContext(contextWithAdmin(r, s)))
}

// canManage reports whether the current admin may see or act on a room.
func canManage(s *AdminSession, room *Room) bool {
	return s.Superadmin || (s.Email != "" && room.OwnerEmail == s.Email)
}

// adminRoom loads the room in the path and enforces ownership. Rooms of
// other users answer 404, not 403, so their names are not confirmed.
func (app *App) adminRoom(w http.ResponseWriter, r *http.Request) (*Room, bool) {
	room, ok := app.roomFromPath(w, r)
	if !ok {
		return nil, false
	}
	if !canManage(adminFrom(r), room) {
		app.render(w, http.StatusNotFound, "notfound.html", nil)
		return nil, false
	}
	return room, true
}

// adminRoomBySlug enforces ownership for objects (minutes, transcripts) that
// reference a room by slug.
func (app *App) adminRoomBySlug(w http.ResponseWriter, r *http.Request, slug string) bool {
	room, err := app.DB.GetRoom(slug)
	if err != nil || !canManage(adminFrom(r), room) {
		http.NotFound(w, r)
		return false
	}
	return true
}

// formToken implements a double-submit token for forms used before any
// session exists (login, join). It complements the Origin check.
func (app *App) formToken(w http.ResponseWriter, r *http.Request) string {
	if v := app.cookieValue(r, "tgm_form"); len(v) >= 32 {
		return v
	}
	v := randomToken()
	app.setCookie(w, "tgm_form", v, 12*time.Hour)
	return v
}

func (app *App) checkFormToken(r *http.Request) bool {
	return tokensEqual(r.PostFormValue("form_token"), app.cookieValue(r, "tgm_form"))
}

// --- public pages ---

func (app *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := app.DB.Ping(r.Context()); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte("ok"))
}

func (app *App) handleHome(w http.ResponseWriter, r *http.Request) {
	app.render(w, http.StatusOK, "home.html", nil)
}

// --- admin auth ---

func (app *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if app.identityMode() {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	app.render(w, http.StatusOK, "login.html", map[string]any{"FormToken": app.formToken(w, r)})
}

func (app *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if app.identityMode() {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	ip := app.clientIP(r)
	if !app.checkFormToken(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if app.loginLimiter.Blocked(ip) {
		slog.Warn("admin: login rate limited", "ip", ip)
		app.render(w, http.StatusTooManyRequests, "login.html", map[string]any{
			"FormToken": app.formToken(w, r),
			"Error":     "Demasiados intentos. Espera 15 minutos.",
		})
		return
	}
	password := r.PostFormValue("password")
	hash := dummyHash
	cred, err := app.DB.GetAdminCredential()
	if err == nil {
		hash = []byte(cred.Hash)
	} else if !errors.Is(err, ErrNotFound) {
		app.serverError(w, "login", err)
		return
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(password)) != nil || cred == nil {
		app.loginLimiter.Fail(ip)
		slog.Warn("admin: login failed", "ip", ip)
		app.render(w, http.StatusUnauthorized, "login.html", map[string]any{
			"FormToken": app.formToken(w, r),
			"Error":     "Contraseña incorrecta.",
		})
		return
	}
	app.loginLimiter.Reset(ip)
	token, err := app.DB.CreateAdminSession("")
	if err != nil {
		app.serverError(w, "login", err)
		return
	}
	slog.Info("admin: login", "ip", ip)
	app.setCookie(w, "tgm_admin", token, adminAbsoluteTimeout)
	if cred.MustChange {
		http.Redirect(w, r, "/admin/password", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := app.cookieValue(r, "tgm_admin")
	s, err := app.DB.GetAdminSession(token)
	if err == nil && tokensEqual(r.PostFormValue("csrf_token"), s.CSRFToken) {
		app.DB.DeleteAdminSession(token)
	}
	app.clearCookie(w, "tgm_admin")
	if app.identityMode() {
		// Also end the Cloudflare Access session for this hostname.
		http.Redirect(w, r, "/cdn-cgi/access/logout", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (app *App) handlePasswordPage(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	app.render(w, http.StatusOK, "password.html", map[string]any{"CSRF": s.CSRFToken, "MustChange": s.MustChange})
}

func (app *App) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	s := adminFrom(r)
	ip := app.clientIP(r)
	fail := func(status int, msg string) {
		app.render(w, status, "password.html", map[string]any{"CSRF": s.CSRFToken, "MustChange": s.MustChange, "Error": msg})
	}
	if app.loginLimiter.Blocked(ip) {
		fail(http.StatusTooManyRequests, "Demasiados intentos. Espera 15 minutos.")
		return
	}
	cred, err := app.DB.GetAdminCredential()
	if err != nil {
		app.serverError(w, "password", err)
		return
	}
	current, next := r.PostFormValue("current_password"), r.PostFormValue("new_password")
	if bcrypt.CompareHashAndPassword([]byte(cred.Hash), []byte(current)) != nil {
		app.loginLimiter.Fail(ip)
		fail(http.StatusUnauthorized, "La contraseña actual no es correcta.")
		return
	}
	if next != r.PostFormValue("confirm_password") {
		fail(http.StatusBadRequest, "La confirmación no coincide.")
		return
	}
	if err := validateNewAdminPassword(current, next); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	// SetAdminPassword revokes every session; issue a fresh one.
	if err := app.DB.SetAdminPassword(next, false); err != nil {
		app.serverError(w, "password", err)
		return
	}
	token, err := app.DB.CreateAdminSession("")
	if err != nil {
		app.serverError(w, "password", err)
		return
	}
	slog.Info("admin: password changed", "ip", ip)
	app.setCookie(w, "tgm_admin", token, adminAbsoluteTimeout)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// --- dashboard ---

type roomView struct {
	Slug           string
	Owner          string
	Active         bool
	Transcription  bool
	DialIn         *DialInInfo
	Recordings     []Recording
	Transcriptions []Transcription
	Minutes        []Minutes
}

func (app *App) dashboardData(r *http.Request, extra map[string]any) (map[string]any, error) {
	app.DB.DeactivateExpiredRooms(app.maxMeeting())
	s := adminFrom(r)
	var rooms []Room
	var err error
	if s.Superadmin {
		rooms, err = app.DB.ListRooms()
	} else {
		rooms, err = app.DB.ListRoomsOwnedBy(s.Email)
	}
	if err != nil {
		return nil, err
	}
	numbers, err := app.DialIn.FetchDIDs()
	if err != nil {
		slog.Warn("dial-in DIDs unavailable")
	}
	var views []roomView
	for _, room := range rooms {
		rv := roomView{Slug: room.Slug, Owner: room.OwnerEmail, Active: room.Active, Transcription: room.Transcription}
		if numbers != nil {
			if pin, err := app.DialIn.FetchPIN(app.Config.JaaSAppID, room.Slug); err == nil {
				rv.DialIn = &DialInInfo{PIN: pin, Numbers: numbers}
			}
		}
		if rv.Recordings, err = app.DB.ListRecordings(room.Slug); err != nil {
			return nil, err
		}
		if rv.Transcriptions, err = app.DB.ListTranscriptions(room.Slug); err != nil {
			return nil, err
		}
		if rv.Minutes, err = app.DB.ListMinutes(room.Slug); err != nil {
			return nil, err
		}
		views = append(views, rv)
	}
	data := map[string]any{
		"Rooms":        views,
		"CSRF":         s.CSRFToken,
		"PublicOrigin": app.Config.PublicOrigin,
		"Email":        s.Email,
		"Superadmin":   s.Superadmin,
		"IdentityMode": app.identityMode(),
	}
	for k, v := range extra {
		data[k] = v
	}
	return data, nil
}

func (app *App) renderDashboard(w http.ResponseWriter, r *http.Request, status int, extra map[string]any) {
	data, err := app.dashboardData(r, extra)
	if err != nil {
		app.serverError(w, "dashboard", err)
		return
	}
	app.render(w, status, "dashboard.html", data)
}

func (app *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	app.renderDashboard(w, r, http.StatusOK, nil)
}

func (app *App) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PostFormValue("slug"))
	hostPassword := r.PostFormValue("host_password")
	if !validSlug(slug) {
		app.renderDashboard(w, r, http.StatusBadRequest, map[string]any{
			"Error": "El identificador debe tener de 3 a 80 caracteres: minúsculas, números y guiones, sin guion inicial.",
		})
		return
	}
	if len(hostPassword) < minHostPasswordLen {
		app.renderDashboard(w, r, http.StatusBadRequest, map[string]any{
			"Error": "La contraseña de anfitrión debe tener al menos 10 caracteres.",
		})
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(hostPassword), bcryptCost)
	if err != nil {
		app.serverError(w, "create room", err)
		return
	}
	owner := adminFrom(r).Email
	if err := app.DB.CreateRoom(slug, string(hash), owner); err != nil {
		app.renderDashboard(w, r, http.StatusConflict, map[string]any{"Error": "Ya existe una sala con ese identificador."})
		return
	}
	slog.Info("admin: room created", "room", slug, "user", owner)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// roomFromPath validates the slug before touching the database.
func (app *App) roomFromPath(w http.ResponseWriter, r *http.Request) (*Room, bool) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		app.render(w, http.StatusNotFound, "notfound.html", nil)
		return nil, false
	}
	room, err := app.DB.GetRoom(slug)
	if errors.Is(err, ErrNotFound) {
		app.render(w, http.StatusNotFound, "notfound.html", map[string]any{"Slug": slug})
		return nil, false
	}
	if err != nil {
		app.serverError(w, "room", err)
		return nil, false
	}
	return room, true
}

func (app *App) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	room, ok := app.adminRoom(w, r)
	if !ok {
		return
	}
	app.DB.captureMu.Lock()
	defer app.DB.captureMu.Unlock()
	app.deleteRoomAudio(room.Slug)
	if err := app.DB.DeleteRoom(room.Slug); err != nil {
		app.serverError(w, "delete room", err)
		return
	}
	slog.Info("admin: room deleted with its recordings, transcripts and minutes", "room", room.Slug, "user", adminFrom(r).Email)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleAdminStart(w http.ResponseWriter, r *http.Request) {
	room, ok := app.adminRoom(w, r)
	if !ok {
		return
	}
	name := cleanDisplayName(r.PostFormValue("display_name"))
	if name == "" {
		name = "Anfitrión"
	}
	if err := app.startParticipant(w, room.Slug, name, adminFrom(r).Email, true); err != nil {
		app.serverError(w, "admin start", err)
		return
	}
	http.Redirect(w, r, "/m/"+room.Slug, http.StatusSeeOther)
}

func (app *App) handleAdminStop(w http.ResponseWriter, r *http.Request) {
	room, ok := app.adminRoom(w, r)
	if !ok {
		return
	}
	app.DB.SetRoomActive(room.Slug, false)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleToggleTranscription(w http.ResponseWriter, r *http.Request) {
	room, ok := app.adminRoom(w, r)
	if !ok {
		return
	}
	app.DB.SetTranscription(room.Slug, r.PostFormValue("enabled") == "on")
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

// --- meetings ---

// cleanDisplayName strips control characters and bounds the length; the
// name ends up inside the JaaS JWT and the meeting page.
func cleanDisplayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > 80 {
		s = string(r[:80])
	}
	return strings.TrimSpace(s)
}

func (app *App) startParticipant(w http.ResponseWriter, slug, name, email string, moderator bool) error {
	if moderator {
		if err := app.DB.SetRoomActive(slug, true); err != nil {
			return err
		}
	}
	token, err := app.DB.CreateParticipantSession(slug, name, email, moderator, app.maxMeeting())
	if err != nil {
		return err
	}
	if err := app.DB.RecordJoin(slug, email, name, moderator); err != nil {
		return err
	}
	app.setCookie(w, "tgm_part", token, app.maxMeeting())
	return nil
}

func (app *App) handleMeeting(w http.ResponseWriter, r *http.Request) {
	app.DB.DeactivateExpiredRooms(app.maxMeeting())
	room, ok := app.roomFromPath(w, r)
	if !ok {
		return
	}
	p, err := app.DB.GetParticipantSession(app.cookieValue(r, "tgm_part"))
	if err == nil && p.RoomSlug == room.Slug && (room.Active || p.Moderator) {
		if p.Moderator && !room.Active {
			app.DB.SetRoomActive(room.Slug, true)
		}
		browserCapture := room.Transcription && app.Transcriber != nil
		// Browser capture owns ASR when its transcriber is available. Keep the
		// gpt-transcribe pilot closed to JaaS ASR even if that dependency is absent.
		jaasTranscription := room.Transcription && app.Transcriber == nil && app.Config.TranscribeModel != transcriptionModel
		jwt, err := GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID,
			room.Slug, p.DisplayName, p.Email, p.Moderator, jaasTranscription, app.maxMeeting())
		if err != nil {
			app.serverError(w, "jwt", err)
			return
		}
		app.render(w, http.StatusOK, "meeting.html", map[string]any{
			"Slug":        room.Slug,
			"AppID":       app.Config.JaaSAppID,
			"JWT":         jwt,
			"IsModerator": p.Moderator,
			"DisplayName": p.DisplayName,
			"CSRF":        p.CSRFToken,
			"Capture":     browserCapture,
		})
		return
	}
	app.render(w, http.StatusOK, "join.html", map[string]any{"Slug": room.Slug, "FormToken": app.formToken(w, r)})
}

func (app *App) handleJoinMeeting(w http.ResponseWriter, r *http.Request) {
	room, ok := app.roomFromPath(w, r)
	if !ok {
		return
	}
	if !app.checkFormToken(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	formToken := app.formToken(w, r)
	name := cleanDisplayName(r.PostFormValue("display_name"))
	email := normalizeEmail(r.PostFormValue("email"))
	form := func(extra map[string]any) map[string]any {
		data := map[string]any{"Slug": room.Slug, "FormToken": formToken, "Name": name, "Email": r.PostFormValue("email")}
		for k, v := range extra {
			data[k] = v
		}
		return data
	}
	switch {
	case name == "":
		app.render(w, http.StatusBadRequest, "join.html", form(map[string]any{"Error": "Escribe tu nombre."}))
		return
	case email == "":
		app.render(w, http.StatusBadRequest, "join.html", form(map[string]any{"Error": "Escribe un correo válido: ahí recibirás la transcripción y la minuta."}))
		return
	case r.PostFormValue("consent") != "on":
		app.render(w, http.StatusBadRequest, "join.html", form(map[string]any{"Error": "Para entrar debes aceptar el aviso de transcripción y envío por correo."}))
		return
	}

	if hostPassword := r.PostFormValue("host_password"); hostPassword != "" {
		ip := app.clientIP(r)
		ipKey, roomKey := ip+"|"+room.Slug, room.Slug
		if app.hostLimiter.Blocked(ipKey) || app.hostRoomLimiter.Blocked(roomKey) {
			slog.Warn("join: host password rate limited", "room", room.Slug, "ip", ip)
			app.render(w, http.StatusTooManyRequests, "join.html", form(map[string]any{"Error": "Demasiados intentos. Espera 15 minutos.", "HostOpen": true}))
			return
		}
		if bcrypt.CompareHashAndPassword([]byte(room.HostHash), []byte(hostPassword)) != nil {
			app.hostLimiter.Fail(ipKey)
			app.hostRoomLimiter.Fail(roomKey)
			slog.Warn("join: wrong host password", "room", room.Slug, "ip", ip)
			app.render(w, http.StatusUnauthorized, "join.html", form(map[string]any{"Error": "Contraseña de anfitrión incorrecta.", "HostOpen": true}))
			return
		}
		app.hostLimiter.Reset(ipKey)
		if err := app.startParticipant(w, room.Slug, name, email, true); err != nil {
			app.serverError(w, "join host", err)
			return
		}
		slog.Info("join: host", "room", room.Slug)
		http.Redirect(w, r, "/m/"+room.Slug, http.StatusSeeOther)
		return
	}

	if !room.Active {
		app.render(w, http.StatusOK, "waiting.html", map[string]any{
			"Slug": room.Slug, "DisplayName": name, "Email": email, "FormToken": formToken,
		})
		return
	}
	if err := app.startParticipant(w, room.Slug, name, email, false); err != nil {
		app.serverError(w, "join guest", err)
		return
	}
	http.Redirect(w, r, "/m/"+room.Slug, http.StatusSeeOther)
}

// handleEndMeeting is called by the moderator page (sendBeacon). Only a
// moderator session for this room, with its CSRF token, can end it.
func (app *App) handleEndMeeting(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	p, err := app.DB.GetParticipantSession(app.cookieValue(r, "tgm_part"))
	if err != nil || p.RoomSlug != slug || !p.Moderator || !tokensEqual(r.PostFormValue("csrf_token"), p.CSRFToken) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	app.DB.SetRoomActive(slug, false)
	w.WriteHeader(http.StatusNoContent)
}

// --- transcripts and minutes ---

type transcriptMessage struct {
	Name      string
	Content   string
	Timestamp time.Time
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (app *App) handleViewTranscription(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	t, err := app.DB.GetTranscription(id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		app.serverError(w, "transcription", err)
		return
	}
	if !app.adminRoomBySlug(w, r, t.RoomSlug) {
		return
	}
	var raw struct {
		Messages []struct {
			Name      string `json:"name"`
			Content   string `json:"content"`
			Timestamp int64  `json:"timestamp"`
		} `json:"messages"`
	}
	json.Unmarshal([]byte(t.Data), &raw)
	var messages []transcriptMessage
	for _, m := range raw.Messages {
		messages = append(messages, transcriptMessage{Name: m.Name, Content: m.Content, Timestamp: time.UnixMilli(m.Timestamp)})
	}
	app.render(w, http.StatusOK, "transcription.html", map[string]any{"Transcription": t, "Messages": messages})
}

func (app *App) loadMinutes(w http.ResponseWriter, r *http.Request) (*Minutes, *MinutesResult, bool) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return nil, nil, false
	}
	m, err := app.DB.GetMinutes(id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return nil, nil, false
	}
	if err != nil {
		app.serverError(w, "minutes", err)
		return nil, nil, false
	}
	if !app.adminRoomBySlug(w, r, m.RoomSlug) {
		return nil, nil, false
	}
	var res *MinutesResult
	if m.Status == "ready" {
		res = &MinutesResult{}
		if err := json.Unmarshal([]byte(m.ResultJSON), res); err != nil {
			app.serverError(w, "minutes decode", err)
			return nil, nil, false
		}
	}
	return m, res, true
}

func (app *App) handleViewMinutes(w http.ResponseWriter, r *http.Request) {
	m, res, ok := app.loadMinutes(w, r)
	if !ok {
		return
	}
	var recipients []Recipient
	if res != nil {
		var err error
		if recipients, err = app.DB.ProposedRecipients(m); err != nil {
			app.serverError(w, "recipients", err)
			return
		}
	}
	app.render(w, http.StatusOK, "minutes.html", map[string]any{
		"M": m, "R": res, "CSRF": adminFrom(r).CSRFToken, "TZ": app.Config.DisplayTimezone.String(),
		"Recipients": recipients, "MailEnabled": app.Mailer != nil, "Sent": r.URL.Query().Get("enviado"),
	})
}

func (app *App) handleDownloadMinutes(w http.ResponseWriter, r *http.Request) {
	m, res, ok := app.loadMinutes(w, r)
	if !ok {
		return
	}
	if res == nil {
		http.Error(w, "La minuta aún no está lista.", http.StatusConflict)
		return
	}
	setSecurityHeaders(w, false)
	base := "minuta-" + m.RoomSlug + "-" + m.MeetingAt.In(app.Config.DisplayTimezone).Format("2006-01-02") + "-" + strconv.FormatInt(m.ID, 10)
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+base+`.json"`)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{
			"room_slug": m.RoomSlug, "session_id": m.SessionID, "status": "draft",
			"meeting_at": m.MeetingAt.Format(time.RFC3339), "model": m.Model, "minutes": res,
		})
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+base+`.md"`)
	w.Write([]byte(minutesMarkdown(m, res, app.Config.DisplayTimezone)))
}

// handleSendMinutes queues the host-approved deliveries. Only addresses from
// the proposed list (people who took part in this session) can be chosen.
func (app *App) handleSendMinutes(w http.ResponseWriter, r *http.Request) {
	if app.Mailer == nil {
		http.Error(w, "El envío de correo no está configurado.", http.StatusServiceUnavailable)
		return
	}
	m, res, ok := app.loadMinutes(w, r)
	if !ok {
		return
	}
	if res == nil {
		http.Error(w, "La minuta aún no está lista.", http.StatusConflict)
		return
	}
	proposed, err := app.DB.ProposedRecipients(m)
	if err != nil {
		app.serverError(w, "recipients", err)
		return
	}
	allowed := map[string]Recipient{}
	for _, p := range proposed {
		allowed[p.Email] = p
	}
	r.ParseForm()
	var chosen []Recipient
	for _, raw := range r.PostForm["recipient"] {
		p, ok := allowed[normalizeEmail(raw)]
		if !ok {
			http.Error(w, "Destinatario fuera de la lista de participantes.", http.StatusBadRequest)
			return
		}
		chosen = append(chosen, p)
	}
	if len(chosen) == 0 {
		http.Redirect(w, r, "/admin/minutes/"+strconv.FormatInt(m.ID, 10), http.StatusSeeOther)
		return
	}
	approver := adminFrom(r).Email
	if approver == "" {
		approver = "admin"
	}
	n, err := app.DB.QueueDeliveries(m.ID, chosen, approver)
	if err != nil {
		app.serverError(w, "queue deliveries", err)
		return
	}
	slog.Info("admin: minutes delivery approved", "minutes", m.ID, "queued", n, "user", adminFrom(r).Email)
	app.wakeWorker()
	http.Redirect(w, r, "/admin/minutes/"+strconv.FormatInt(m.ID, 10)+"?enviado="+strconv.Itoa(n), http.StatusSeeOther)
}

func (app *App) handleRetryMinutes(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	m, err := app.DB.GetMinutes(id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		app.serverError(w, "retry", err)
		return
	}
	if !app.adminRoomBySlug(w, r, m.RoomSlug) {
		return
	}
	requeued, err := app.DB.RequeueMinutes(id)
	if err != nil {
		app.serverError(w, "retry", err)
		return
	}
	if requeued {
		slog.Info("admin: minutes requeued", "id", id)
		app.wakeWorker()
	}
	http.Redirect(w, r, "/admin/minutes/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
