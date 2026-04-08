package main

import (
	"html/template"
	"log"
	"net/http"
	"os"
)

// App holds application dependencies.
type App struct {
	Config    *Config
	DB        *DB
	DialIn    *DialInClient
	templates *template.Template
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	db, err := InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	app := &App{Config: cfg, DB: db, DialIn: &DialInClient{}}
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, mux); err != nil {
		log.Fatalf("server: %v", err)
		os.Exit(1)
	}
}
