package main

import (
	"embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

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
	mux.HandleFunc("GET /m/{slug}", app.handleMeeting)
	mux.HandleFunc("POST /m/{slug}/join", app.handleJoinMeeting)
	mux.HandleFunc("POST /m/{slug}/end", app.handleEndMeeting)
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
	Slug   string
	DialIn *DialInInfo
}

func (app *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
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
		rv := roomView{Slug: room.Slug}
		pin, err := app.DialIn.FetchPIN(app.Config.JaaSAppID, room.Slug)
		if err != nil {
			log.Printf("dial-in PIN for %s: %v", room.Slug, err)
		} else if numbers != nil {
			rv.DialIn = &DialInInfo{PIN: pin, Numbers: numbers}
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
	_, err := app.DB.GetRoom(slug)
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

	jwt, err := GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID, slug, displayName, true)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/m/"+slug+"?jwt="+jwt+"&mod=1&name="+url.QueryEscape(displayName), http.StatusSeeOther)
}

func (app *App) handleMeeting(w http.ResponseWriter, r *http.Request) {
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
		"DisplayName": displayName,
		"DialIn":      dialIn,
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

		jwt, err := GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID, slug, displayName, true)
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

	jwt, err := GenerateJWT(app.Config.JaaSKey, app.Config.JaaSAppID, app.Config.JaaSKeyID, slug, displayName, false)
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
