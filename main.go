package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
)

var version = "dev"

// App holds application dependencies.
type App struct {
	Config    *Config
	DB        *DB
	DialIn    *DialInClient
	templates *template.Template
}

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}

	switch command {
	case "serve":
		cmdServe()
	case "update":
		cmdUpdate()
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: %s COMMAND\n\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  serve     Start the HTTP server (default)\n")
	fmt.Fprintf(os.Stderr, "  update    Update to the latest version\n")
	fmt.Fprintf(os.Stderr, "  version   Display version information\n")
}

func cmdServe() {
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

	log.Printf("jaas-app %s listening on %s", version, cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, mux); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func cmdUpdate() {
	if err := Update(version); err != nil {
		fmt.Fprintf(os.Stderr, "Update failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Update completed successfully")
}
