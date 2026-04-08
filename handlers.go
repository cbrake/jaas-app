package main

import (
	"errors"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

func (app *App) loadTemplates() error {
	t, err := template.ParseGlob(filepath.Join("templates", "*.html"))
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

	mux.HandleFunc("GET /login", app.handleLoginPage)
	mux.HandleFunc("POST /login", app.handleLogin)
	mux.HandleFunc("POST /logout", app.handleLogout)
	mux.Handle("GET /{$}", admin)
	mux.Handle("POST /rooms", RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleCreateRoom)))
	mux.Handle("POST /rooms/{slug}/delete", RequireAdmin(app.Config.SessionSecret, http.HandlerFunc(app.handleDeleteRoom)))
	mux.HandleFunc("GET /m/{slug}", app.handleMeeting)
	mux.HandleFunc("POST /m/{slug}/start", app.handleStartMeeting)
	mux.HandleFunc("POST /m/{slug}/end", app.handleEndMeeting)
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, ClearSession())
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type roomView struct {
	Slug   string
	DialIn *DialInInfo
}

func (app *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	rooms, err := app.DB.ListRooms()
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var views []roomView
	for _, room := range rooms {
		rv := roomView{Slug: room.Slug}
		info, err := app.DialIn.FetchDialInInfo(app.Config.JaaSAppID, room.Slug)
		if err == nil {
			rv.DialIn = info
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

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *App) handleDeleteRoom(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	app.DB.DeleteRoom(slug)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (app *App) handleMeeting(w http.ResponseWriter, r *http.Request) {
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

	if !room.Active {
		app.render(w, "waiting.html", map[string]any{
			"Slug": slug,
		})
		return
	}

	// Check for moderator JWT in query param (set by /start redirect)
	jwtToken := r.URL.Query().Get("jwt")
	isModerator := jwtToken != ""

	if !isModerator {
		var err error
		jwtToken, err = GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID, slug, "Guest", false)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	var dialIn *DialInInfo
	info, err := app.DialIn.FetchDialInInfo(app.Config.JaaSAppID, slug)
	if err == nil {
		dialIn = info
	}

	app.render(w, "meeting.html", map[string]any{
		"Slug":        slug,
		"AppID":       app.Config.JaaSAppID,
		"JWT":         jwtToken,
		"IsModerator": isModerator,
		"DialIn":      dialIn,
	})
}

func (app *App) handleStartMeeting(w http.ResponseWriter, r *http.Request) {
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

	hostPassword := r.FormValue("host_password")
	displayName := strings.TrimSpace(r.FormValue("display_name"))
	if err := bcrypt.CompareHashAndPassword([]byte(room.HostHash), []byte(hostPassword)); err != nil {
		app.render(w, "waiting.html", map[string]any{
			"Slug":  slug,
			"Error": "Invalid host password",
		})
		return
	}

	app.DB.SetRoomActive(slug, true)

	jwt, err := GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID, slug, displayName, true)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/m/"+slug+"?jwt="+jwt, http.StatusSeeOther)
}

func (app *App) handleEndMeeting(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	app.DB.SetRoomActive(slug, false)
	w.WriteHeader(http.StatusOK)
}
