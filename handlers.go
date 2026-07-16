package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

func (app *App) loadTemplates() error {
	t, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return err
	}
	app.templates = t
	return nil
}

func (app *App) render(w http.ResponseWriter, name string, data any) {
	if app.templates == nil {
		if err := app.loadTemplates(); err != nil {
			log.Printf("load templates: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := app.templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (app *App) RegisterRoutes(mux *http.ServeMux) {
	admin := RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleDashboard))

	mux.Handle("GET /static/", http.FileServer(http.FS(staticFS)))
	mux.HandleFunc("GET /{$}", app.handleHome)
	mux.HandleFunc("GET /admin/login", app.handleLoginPage)
	mux.HandleFunc("POST /admin/login", app.handleLogin)
	mux.HandleFunc("POST /admin/logout", app.handleLogout)
	mux.Handle("GET /admin", admin)
	mux.Handle("POST /admin/rooms", RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleCreateRoom)))
	mux.Handle("POST /admin/rooms/{slug}/delete", RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleDeleteRoom)))
	mux.Handle("POST /admin/rooms/{slug}/start", RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleAdminStart)))
	mux.Handle("POST /admin/rooms/{slug}/stop", RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleAdminStop)))
	mux.Handle("POST /admin/rooms/{slug}/transcription", RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleToggleTranscription)))
	mux.HandleFunc("GET /m/{slug}", app.handleMeeting)
	mux.HandleFunc("POST /m/{slug}/join", app.handleJoinMeeting)
	mux.HandleFunc("POST /m/{slug}/end", app.handleEndMeeting)
	mux.Handle("GET /admin/transcriptions/{id}", RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleViewTranscription)))
	mux.HandleFunc("POST /webhook/recording", app.handleRecordingWebhook)
}

func (app *App) handleHome(w http.ResponseWriter, r *http.Request) {
	app.render(w, "home.html", nil)
}

func (app *App) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	app.render(w, "login.html", nil)
}

func (app *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	password := r.FormValue("password")
	if password != app.Config.AdminPassword {
		app.render(w, "login.html", map[string]string{"Error": "Invalid password"})
		return
	}
	http.SetCookie(w, SignSession(app.Config.SessionSecret))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, ClearSession())
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

type roomView struct {
	Slug           string
	Active         bool
	Transcription  bool
	DialIn         *DialInInfo
	Recordings     []Recording
	Transcriptions []Transcription
}

func (app *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Auto-expire rooms older than 4 hours
	app.DB.DeactivateExpiredRooms(4 * time.Hour)

	rooms, err := app.DB.ListRooms()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Fetch DIDs once for all rooms
	numbers, err := app.DialIn.FetchDIDs()
	if err != nil {
		log.Printf("dial-in DIDs fetch: %v", err)
	}

	var views []roomView
	for _, room := range rooms {
		rv := roomView{Slug: room.Slug, Active: room.Active, Transcription: room.Transcription}
		pin, err := app.DialIn.FetchPIN(app.Config.JaaSAppID, room.Slug)
		if err != nil {
			log.Printf("dial-in PIN for %s: %v", room.Slug, err)
		} else if numbers != nil {
			rv.DialIn = &DialInInfo{PIN: pin, Numbers: numbers}
		}
		recs, err := app.DB.ListRecordings(room.Slug)
		if err != nil {
			log.Printf("recordings for %s: %v", room.Slug, err)
		} else {
			rv.Recordings = recs
		}
		trans, err := app.DB.ListTranscriptions(room.Slug)
		if err != nil {
			log.Printf("transcriptions for %s: %v", room.Slug, err)
		} else {
			rv.Transcriptions = trans
		}
		views = append(views, rv)
	}

	app.render(w, "dashboard.html", map[string]any{
		"Rooms": views,
	})
}

func (app *App) handleCreateRoom(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.FormValue("slug"))
	hostPassword := r.FormValue("host_password")

	if slug == "" || hostPassword == "" {
		app.render(w, "dashboard.html", map[string]any{
			"Error": "Slug and host password are required",
		})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(hostPassword), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := app.DB.CreateRoom(slug, string(hash)); err != nil {
		rooms, _ := app.DB.ListRooms()
		var views []roomView
		for _, room := range rooms {
			views = append(views, roomView{Slug: room.Slug})
		}
		app.render(w, "dashboard.html", map[string]any{
			"Error": "Room slug already exists",
			"Rooms": views,
		})
		return
	}

	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	app.DB.DeleteRoom(slug)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleAdminStart(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	room, err := app.DB.GetRoom(slug)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	displayName := strings.TrimSpace(r.FormValue("display_name"))
	if displayName == "" {
		displayName = "Admin"
	}

	app.DB.SetRoomActive(slug, true)

	jwt, err := GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID, slug, displayName, true, room.Transcription)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/m/"+slug+"?jwt="+jwt+"&mod=1&name="+url.QueryEscape(displayName), http.StatusSeeOther)
}

func (app *App) handleToggleTranscription(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	enabled := r.FormValue("enabled") == "on"
	app.DB.SetTranscription(slug, enabled)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleAdminStop(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	app.DB.SetRoomActive(slug, false)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (app *App) handleMeeting(w http.ResponseWriter, r *http.Request) {
	app.DB.DeactivateExpiredRooms(4 * time.Hour)

	slug := r.PathValue("slug")
	room, err := app.DB.GetRoom(slug)
	if errors.Is(err, ErrNotFound) {
		w.WriteHeader(http.StatusNotFound)
		app.render(w, "notfound.html", map[string]string{"Slug": slug})
		return
	}
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Check for JWT in query param (set by /join redirect)
	jwtToken := r.URL.Query().Get("jwt")
	isModerator := r.URL.Query().Get("mod") == "1"
	displayName := r.URL.Query().Get("name")

	if jwtToken == "" || (!room.Active && !isModerator) {
		// No JWT or meeting ended — show the join form
		app.render(w, "join.html", map[string]any{
			"Slug": slug,
		})
		return
	}

	app.render(w, "meeting.html", map[string]any{
		"Slug":        slug,
		"AppID":       app.Config.JaaSAppID,
		"JWT":         jwtToken,
		"IsModerator": isModerator,
		"DisplayName": displayName,
	})
}

func (app *App) handleJoinMeeting(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	room, err := app.DB.GetRoom(slug)
	if errors.Is(err, ErrNotFound) {
		w.WriteHeader(http.StatusNotFound)
		app.render(w, "notfound.html", map[string]string{"Slug": slug})
		return
	}
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	displayName := strings.TrimSpace(r.FormValue("display_name"))
	if displayName == "" {
		app.render(w, "join.html", map[string]any{
			"Slug":  slug,
			"Error": "Name is required",
		})
		return
	}

	hostPassword := r.FormValue("host_password")

	// If host password provided, validate and join as moderator
	if hostPassword != "" {
		if err := bcrypt.CompareHashAndPassword([]byte(room.HostHash), []byte(hostPassword)); err != nil {
			app.render(w, "join.html", map[string]any{
				"Slug":  slug,
				"Error": "Invalid host password",
			})
			return
		}

		app.DB.SetRoomActive(slug, true)

		jwt, err := GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID, slug, displayName, true, room.Transcription)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/m/"+slug+"?jwt="+jwt+"&mod=1&name="+url.QueryEscape(displayName), http.StatusSeeOther)
		return
	}

	// No host password — guest flow
	if !room.Active {
		// Meeting not started yet, show waiting page
		app.render(w, "waiting.html", map[string]any{
			"Slug":        slug,
			"DisplayName": displayName,
		})
		return
	}

	jwt, err := GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID, slug, displayName, false, room.Transcription)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/m/"+slug+"?jwt="+jwt+"&name="+url.QueryEscape(displayName), http.StatusSeeOther)
}

func (app *App) handleEndMeeting(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	app.DB.SetRoomActive(slug, false)
	w.WriteHeader(http.StatusOK)
}

type transcriptMessage struct {
	Name      string
	Content   string
	Timestamp time.Time
}

func (app *App) handleViewTranscription(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	var id int64
	fmt.Sscanf(idStr, "%d", &id)

	t, err := app.DB.GetTranscription(id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Parse the transcription JSON
	var raw struct {
		ConferenceName string `json:"conferenceName"`
		Messages       []struct {
			Name      string `json:"name"`
			Content   string `json:"content"`
			Timestamp int64  `json:"timestamp"`
		} `json:"messages"`
	}
	json.Unmarshal([]byte(t.Data), &raw)

	var messages []transcriptMessage
	for _, m := range raw.Messages {
		messages = append(messages, transcriptMessage{
			Name:      m.Name,
			Content:   m.Content,
			Timestamp: time.UnixMilli(m.Timestamp),
		})
	}

	app.render(w, "transcription.html", map[string]any{
		"Transcription": t,
		"Messages":      messages,
		"Room":          raw.ConferenceName,
	})
}

func (app *App) handleRecordingWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("webhook: read body: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	log.Printf("webhook: received: %s", string(body))

	var payload struct {
		EventType string `json:"eventType"`
		Data      struct {
			PreAuthenticatedLink string `json:"preAuthenticatedLink"`
			DurationSec          int    `json:"durationSec"`
			LinkExpiration       int64  `json:"linkExpiration"`
		} `json:"data"`
		FQN string `json:"fqn"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("webhook: invalid payload: %v", err)
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Extract room slug from FQN: "vpaas-xxx/room-slug"
	roomSlug := ""
	parts := strings.Split(payload.FQN, "/")
	if len(parts) >= 2 {
		roomSlug = parts[len(parts)-1]
	}

	switch payload.EventType {
	case "RECORDING_UPLOADED":
		if payload.Data.PreAuthenticatedLink == "" {
			log.Printf("webhook: no download link in recording payload")
			w.WriteHeader(http.StatusOK)
			return
		}
		expiresAt := time.Now().Add(24 * time.Hour)
		if payload.Data.LinkExpiration > 0 {
			expiresAt = time.Unix(payload.Data.LinkExpiration/1000, 0)
		}
		if err := app.DB.AddRecording(roomSlug, "recording", payload.Data.PreAuthenticatedLink, expiresAt, payload.Data.DurationSec); err != nil {
			log.Printf("webhook: save recording: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		log.Printf("webhook: recording saved for room %s (expires %s)", roomSlug, expiresAt.Format(time.RFC3339))

	case "TRANSCRIPTION_UPLOADED":
		if payload.Data.PreAuthenticatedLink == "" {
			log.Printf("webhook: no download link in transcription payload")
			w.WriteHeader(http.StatusOK)
			return
		}
		// Download the transcription JSON
		resp, err := http.Get(payload.Data.PreAuthenticatedLink)
		if err != nil {
			log.Printf("webhook: download transcription: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()
		transcriptBody, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("webhook: read transcription: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		// Extract sessionId from the transcription JSON
		var tData struct {
			SessionID string `json:"sessionId"`
		}
		json.Unmarshal(transcriptBody, &tData)

		if err := app.DB.AddTranscription(roomSlug, tData.SessionID, string(transcriptBody)); err != nil {
			log.Printf("webhook: save transcription: %v", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		log.Printf("webhook: transcription saved for room %s (session %s)", roomSlug, tData.SessionID)

	default:
		w.WriteHeader(http.StatusOK)
		return
	}

	w.WriteHeader(http.StatusOK)
}
