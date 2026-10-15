package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html/template"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"
)

var version = "dev"

// App holds application dependencies.
type App struct {
	Config         *Config
	DB             *DB
	DialIn         *DialInClient
	LLM            MinutesGenerator
	Mailer         Mailer      // nil when SMTP is not configured
	Transcriber    Transcriber // browser-capture speech-to-text
	DownloadClient *http.Client
	CFAccess       *CFAccessVerifier

	loginLimiter    *failureLimiter // per client IP
	hostLimiter     *failureLimiter // per client IP + room
	hostRoomLimiter *failureLimiter // per room, any IP

	wake      chan struct{}
	clock     func() time.Time
	templates *template.Template
}

// maxMeeting: how long a started room stays open to guests and how long a
// participant session (and its JaaS JWT) lasts. Default 24 h.
func (app *App) maxMeeting() time.Duration {
	if app.Config.MaxMeeting > 0 {
		return app.Config.MaxMeeting
	}
	return 4 * time.Hour
}

func (app *App) now() time.Time {
	if app.clock != nil {
		return app.clock()
	}
	return time.Now()
}

func contextWithAdmin(r *http.Request, s *AdminSession) context.Context {
	return context.WithValue(r.Context(), adminSessionKey, s)
}

func adminFrom(r *http.Request) *AdminSession {
	s, _ := r.Context().Value(adminSessionKey).(*AdminSession)
	if s == nil {
		return &AdminSession{}
	}
	return s
}

// NewApp wires an App with production defaults.
func NewApp(cfg *Config, db *DB) (*App, error) {
	app := &App{
		Config:          cfg,
		DB:              db,
		DialIn:          &DialInClient{HTTP: &http.Client{Timeout: 5 * time.Second}},
		DownloadClient:  newDownloadClient(cfg.DownloadAllowedHosts),
		CFAccess:        NewCFAccessVerifier(cfg),
		loginLimiter:    newFailureLimiter(5, 15*time.Minute),
		hostLimiter:     newFailureLimiter(5, 15*time.Minute),
		hostRoomLimiter: newFailureLimiter(20, time.Hour),
		wake:            make(chan struct{}, 1),
	}
	if cfg.OpenAIKey != "" {
		app.LLM = newOpenAIClient(cfg)
		app.Transcriber = &openAITranscriber{BaseURL: cfg.OpenAIBaseURL, Key: cfg.OpenAIKey, Model: cfg.TranscribeModel,
			Prompt: cfg.TranscribePrompt, Language: cfg.TranscribeLanguage, HTTP: &http.Client{Timeout: 10 * time.Minute}}
	}
	if cfg.SMTPHost != "" {
		app.Mailer = &smtpMailer{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Security: cfg.SMTPSecurity,
			User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.MailFrom, FromName: cfg.MailFromName}
	}
	if err := app.loadTemplates(); err != nil {
		return nil, err
	}
	return app, nil
}

func (app *App) Handler() http.Handler {
	mux := http.NewServeMux()
	app.RegisterRoutes(mux)
	return app.withMiddleware(mux)
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	var err error
	switch command {
	case "serve":
		err = cmdServe()
	case "healthcheck":
		err = cmdHealthcheck()
	case "backup":
		err = cmdBackup(os.Args[2:])
	case "verify-db":
		err = cmdVerifyDB(os.Args[2:])
	case "reset-admin":
		err = cmdResetAdmin()
	case "models":
		err = cmdModels()
	case "send-test-mail":
		err = cmdSendTestMail(os.Args[2:])
	case "regenerate-minutes":
		err = cmdRegenerateMinutes(os.Args[2:])
	case "send-preview":
		err = cmdSendPreview(os.Args[2:])
	case "update":
		err = Update(version)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		printUsage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", command, err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, "Usage: %s COMMAND\n\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "Commands:\n")
	fmt.Fprintf(os.Stderr, "  serve              Start the HTTP server and minutes worker (default)\n")
	fmt.Fprintf(os.Stderr, "  healthcheck        Exit 0 if the local server answers /healthz\n")
	fmt.Fprintf(os.Stderr, "  backup DEST        Write a consistent SQLite snapshot to DEST (VACUUM INTO)\n")
	fmt.Fprintf(os.Stderr, "  verify-db FILE     Integrity check and row counts of a database file\n")
	fmt.Fprintf(os.Stderr, "  reset-admin        Read a temporary admin password from stdin; revokes sessions\n")
	fmt.Fprintf(os.Stderr, "  models             List OpenAI models available to OPENAI_API_KEY\n")
	fmt.Fprintf(os.Stderr, "  send-test-mail TO  Send a test message through the configured SMTP server\n")
	fmt.Fprintf(os.Stderr, "  regenerate-minutes ID   Queue minutes ID again (stored transcript, no new notices)\n")
	fmt.Fprintf(os.Stderr, "  send-preview ID TO      E-mail the participant version of minutes ID to TO only\n")
	fmt.Fprintf(os.Stderr, "  update             Download the latest release, verify its SHA-256 and replace this binary\n")
	fmt.Fprintf(os.Stderr, "  version            Display version information\n")
}

func cmdServe() error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	db, err := InitDB(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()

	created, err := db.BootstrapAdmin(cfg.AdminInitialPasswordFile, cfg.AdminLegacyPassword)
	if err != nil {
		return fmt.Errorf("admin bootstrap: %w", err)
	}
	if created {
		slog.Info("admin credential initialised; it must be changed on first login")
	}

	app, err := NewApp(cfg, db)
	if err != nil {
		return err
	}
	if app.CFAccess == nil {
		slog.Warn("Cloudflare Access verification disabled (CF_ACCESS_TEAM_DOMAIN/CF_ACCESS_AUD empty)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(os.Stderr, "http: ", 0),
	}

	workerDone := make(chan struct{})
	go func() {
		app.runWorker(ctx)
		close(workerDone)
	}()
	go app.runRetention(ctx)

	errCh := make(chan error, 1)
	go func() {
		slog.Info("jaas-app listening", "version", version, "addr", cfg.ListenAddr, "model", cfg.OpenAIModel,
			"retention_transcripts", cfg.TranscriptRetention.String(), "retention_minutes", cfg.MinutesRetention.String(),
			"mail", cfg.SMTPHost != "", "ai_minutes", cfg.OpenAIKey != "", "transcribe_model", cfg.TranscribeModel)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	select {
	case <-workerDone:
	case <-shutdownCtx.Done():
	}
	return nil
}

func cmdHealthcheck() error {
	addr := envDefault("LISTEN_ADDR", ":8370")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func cmdBackup(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: backup DEST")
	}
	if _, err := os.Stat(args[0]); err == nil {
		return errors.New("destination already exists")
	}
	db, err := InitDB(envDefault("DB_PATH", "./jaas.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.conn.Exec("VACUUM INTO ?", args[0]); err != nil {
		return err
	}
	return os.Chmod(args[0], 0o600)
}

func cmdVerifyDB(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: verify-db FILE")
	}
	if _, err := os.Stat(args[0]); err != nil {
		return err
	}
	conn, err := sql.Open("sqlite", "file:"+args[0]+"?mode=ro")
	if err != nil {
		return err
	}
	defer conn.Close()
	var check string
	if err := conn.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		return err
	}
	var schema int
	conn.QueryRow("PRAGMA user_version").Scan(&schema)
	fmt.Printf("integrity_check=%s schema_version=%d\n", check, schema)
	for _, t := range []string{"rooms", "recordings", "transcriptions", "minutes", "webhook_events", "admin_credentials"} {
		var n int
		if err := conn.QueryRow("SELECT COUNT(*) FROM " + t).Scan(&n); err != nil {
			return fmt.Errorf("%s: %w", t, err)
		}
		fmt.Printf("%s=%d\n", t, n)
	}
	var ready, stuck int
	conn.QueryRow("SELECT COUNT(*) FROM minutes WHERE status = 'ready'").Scan(&ready)
	conn.QueryRow("SELECT COUNT(*) FROM minutes WHERE status IN ('queued','processing') AND created_at < ?",
		time.Now().Add(-2*time.Hour).Unix()).Scan(&stuck)
	fmt.Printf("minutes_ready=%d\nminutes_stuck=%d\n", ready, stuck)
	if check != "ok" {
		return errors.New("integrity check failed")
	}
	return nil
}

func cmdResetAdmin() error {
	fmt.Fprintln(os.Stderr, "Temporary admin password (read from stdin, not echoed by this program):")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return err
	}
	pw := strings.TrimSpace(line)
	if len(pw) < minAdminPasswordLen {
		return fmt.Errorf("password must be at least %d characters", minAdminPasswordLen)
	}
	db, err := InitDB(envDefault("DB_PATH", "./jaas.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.SetAdminPassword(pw, true); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Admin password reset; all admin sessions revoked; change required at next login.")
	return nil
}

func cmdModels() error {
	key, err := requireSecret("OPENAI_API_KEY")
	if err != nil {
		return err
	}
	c := &openAIClient{
		BaseURL: strings.TrimRight(envDefault("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/"),
		Key:     key,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
	ids, err := c.ListModels(context.Background())
	if err != nil {
		return err
	}
	for _, id := range ids {
		fmt.Println(id)
	}
	return nil
}

func cmdSendTestMail(args []string) error {
	if len(args) != 1 || normalizeEmail(args[0]) == "" {
		return errors.New("usage: send-test-mail ADDRESS")
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if cfg.SMTPHost == "" {
		return errors.New("SMTP_HOST is not configured")
	}
	m := &smtpMailer{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Security: cfg.SMTPSecurity,
		User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.MailFrom, FromName: cfg.MailFromName}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	err = m.Send(ctx, &Mail{
		To:      normalizeEmail(args[0]),
		Subject: "[jaas-app] Mail delivery test",
		Text: "Test message from the jaas-app meeting service (" + cfg.PublicOrigin + ").\n" +
			"If you received it, delivery of host-approved transcripts and minutes works.\n",
	})
	if err != nil {
		return err
	}
	fmt.Println("sent")
	return nil
}

// cmdRegenerateMinutes re-runs the model on the stored transcript (e.g. after
// a schema change). The host notice is not repeated and nothing is sent.
func cmdRegenerateMinutes(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: regenerate-minutes ID")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return err
	}
	db, err := InitDB(envDefault("DB_PATH", "./jaas.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.conn.Exec("UPDATE minutes SET status = 'failed' WHERE id = ? AND status = 'ready'", id); err != nil {
		return err
	}
	ok, err := db.RequeueMinutes(id)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("minutes not found or no stored transcript")
	}
	fmt.Println("queued; the running service picks it up within 30 s")
	return nil
}

// cmdSendPreview renders the participant e-mail for minutes ID and sends it
// to one address, outside the delivery queue and its records.
func cmdSendPreview(args []string) error {
	if len(args) != 2 || normalizeEmail(args[1]) == "" {
		return errors.New("usage: send-preview ID ADDRESS")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return err
	}
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	if cfg.SMTPHost == "" {
		return errors.New("SMTP_HOST is not configured")
	}
	db, err := InitDB(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	app := &App{Config: cfg, DB: db}
	mail, err := app.composeDelivery(&delivery{MinutesID: id, Email: normalizeEmail(args[1]), Name: "", ApprovedBy: normalizeEmail(args[1])})
	if err != nil {
		return err
	}
	mail.Subject = "[Vista previa] " + mail.Subject
	m := &smtpMailer{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Security: cfg.SMTPSecurity,
		User: cfg.SMTPUser, Password: cfg.SMTPPassword, From: cfg.MailFrom, FromName: cfg.MailFromName}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := m.Send(ctx, mail); err != nil {
		return err
	}
	fmt.Println("sent")
	return nil
}
