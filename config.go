package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// PublicOrigin is the scheme://host the browser sees (e.g.
	// https://meet.transgenia.org). Every state-changing POST must carry a
	// matching Origin or Referer.
	PublicOrigin string
	ListenAddr   string
	DBPath       string

	JaaSAppID string
	JaaSKeyID string
	JaaSKey   *rsa.PrivateKey

	// WebhookSecret is the per-endpoint signing secret shown by the JaaS
	// console ("Reveal secret"). Webhooks without a valid signature are
	// rejected.
	WebhookSecret    []byte
	WebhookTolerance time.Duration

	// AdminInitialPasswordFile is read only when no admin credential exists
	// yet; the password is stored hashed and must be changed on first login.
	AdminInitialPasswordFile string

	OpenAIKey            string
	OpenAIModel          string
	OpenAIBaseURL        string
	TranscribeModel      string        // browser-capture speech-to-text (OpenAI audio API)
	MaxMeeting           time.Duration // how long a started room, its participant sessions and JWTs last
	TranscribePrompt     string
	AudioRetentionDays   int
	AudioMaxStorageBytes int64
	WorkerMaxAttempts    int
	MaxTranscriptSize    int64 // bytes downloaded from JaaS
	MaxTranscriptChars   int   // characters sent to the model

	DownloadAllowedHosts []string
	TrustedProxies       []*net.IPNet

	TranscriptRetention Retention
	MinutesRetention    Retention
	DisplayTimezone     *time.Location

	// Cloudflare Access in front of /admin. When TeamDomain and AUD are set,
	// every /admin request must carry a valid Cf-Access-Jwt-Assertion.
	CFAccessTeamDomain     string
	CFAccessAUD            string
	CFAccessAllowedEmails  []string
	CFAccessAllowedDomains []string

	// AdminEmails see and manage every room. Everyone else authenticated by
	// Cloudflare Access only sees the rooms they created.
	AdminEmails []string

	// Outgoing mail for host-approved deliveries of transcripts and minutes.
	// Empty SMTPHost disables sending.
	SMTPHost     string
	SMTPPort     int
	SMTPSecurity string // starttls | tls
	SMTPUser     string
	SMTPPassword string
	MailFrom     string
	MailFromName string

	// How participant copies attribute the review and who signs them. The
	// approving host's own address is never shown; replies go to MailReplyTo.
	MailReviewer      string // e.g. "un administrador de Transgenia"
	MailSignatureName string // e.g. "Aitana Yarandiz"
	MailSignatureRole string // e.g. "Agente Quack IA de Transgenia"
	MailReplyTo       string // e.g. admin@transgenia.org

	// SecureCookies is false only for local development over plain HTTP.
	SecureCookies bool
}

// readSecret returns the value of NAME_FILE (preferred) or NAME. Files keep
// secrets out of the process environment and `docker inspect`.
func readSecret(name string) (string, error) {
	if path := os.Getenv(name + "_FILE"); path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("reading %s_FILE: %w", name, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(os.Getenv(name)), nil
}

func requireSecret(name string) (string, error) {
	v, err := readSecret(name)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", fmt.Errorf("%s (or %s_FILE) is required", name, name)
	}
	return v, nil
}

func envDefault(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return n, nil
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseRSAKey(keyData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in the JaaS private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		rsaKey, err1 := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err1 != nil {
			return nil, fmt.Errorf("parsing private key: %w", err)
		}
		return rsaKey, nil
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}
	return rsaKey, nil
}

func parseCIDRs(list []string) ([]*net.IPNet, error) {
	var nets []*net.IPNet
	for _, s := range list {
		if !strings.Contains(s, "/") {
			if strings.Contains(s, ":") {
				s += "/128"
			} else {
				s += "/32"
			}
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q", s)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// loadRetention reads <PREFIX>_RETENTION_MONTHS (calendar months, takes
// precedence) or <PREFIX>_RETENTION_DAYS. 0 in both keeps data forever.
func loadRetention(prefix string, defaultDays int) (Retention, error) {
	months, err := envInt(prefix+"_RETENTION_MONTHS", 0)
	if err != nil {
		return Retention{}, err
	}
	if months > 0 {
		return Retention{Months: months}, nil
	}
	days, err := envInt(prefix+"_RETENTION_DAYS", defaultDays)
	return Retention{Days: days}, err
}

func LoadConfig() (*Config, error) {
	cfg := &Config{
		ListenAddr:               envDefault("LISTEN_ADDR", ":8370"),
		DBPath:                   envDefault("DB_PATH", "./jaas.db"),
		OpenAIBaseURL:            strings.TrimRight(envDefault("OPENAI_BASE_URL", "https://api.openai.com/v1"), "/"),
		AdminInitialPasswordFile: os.Getenv("ADMIN_INITIAL_PASSWORD_FILE"),
		CFAccessTeamDomain:       strings.TrimSpace(os.Getenv("CF_ACCESS_TEAM_DOMAIN")),
		CFAccessAUD:              strings.TrimSpace(os.Getenv("CF_ACCESS_AUD")),
		CFAccessAllowedEmails:    splitList(strings.ToLower(os.Getenv("CF_ACCESS_ALLOWED_EMAILS"))),
		CFAccessAllowedDomains:   splitList(strings.ToLower(os.Getenv("CF_ACCESS_ALLOWED_DOMAINS"))),
		AdminEmails:              splitList(strings.ToLower(os.Getenv("ADMIN_EMAILS"))),
		SecureCookies:            os.Getenv("DEV_INSECURE_COOKIES") != "true",
	}

	origin := strings.TrimRight(os.Getenv("PUBLIC_ORIGIN"), "/")
	u, err := url.Parse(origin)
	if origin == "" || err != nil || u.Host == "" || u.Path != "" {
		return nil, fmt.Errorf("PUBLIC_ORIGIN must be scheme://host, e.g. https://meet.transgenia.org")
	}
	if u.Scheme != "https" && cfg.SecureCookies {
		return nil, fmt.Errorf("PUBLIC_ORIGIN must be https (set DEV_INSECURE_COOKIES=true only for local development)")
	}
	cfg.PublicOrigin = origin

	if cfg.JaaSAppID = strings.TrimSpace(os.Getenv("JAAS_APP_ID")); cfg.JaaSAppID == "" {
		return nil, fmt.Errorf("JAAS_APP_ID is required")
	}
	if cfg.JaaSKeyID = strings.TrimSpace(os.Getenv("JAAS_API_KEY_ID")); cfg.JaaSKeyID == "" {
		return nil, fmt.Errorf("JAAS_API_KEY_ID is required")
	}
	pemKey, err := requireSecret("JAAS_PRIVATE_KEY")
	if err != nil {
		return nil, err
	}
	if cfg.JaaSKey, err = parseRSAKey([]byte(pemKey)); err != nil {
		return nil, err
	}

	webhookSecret, err := requireSecret("JAAS_WEBHOOK_SECRET")
	if err != nil {
		return nil, err
	}
	cfg.WebhookSecret = []byte(webhookSecret)

	// JaaS retries a failed delivery with its original signature timestamp,
	// so the window must cover the retry schedule. Replays inside it are
	// harmless: actionable events are de-duplicated by idempotencyKey.
	tolerance, err := envInt("WEBHOOK_TOLERANCE_SECONDS", 86400)
	if err != nil {
		return nil, err
	}
	cfg.WebhookTolerance = time.Duration(tolerance) * time.Second

	if cfg.OpenAIKey, err = requireSecret("OPENAI_API_KEY"); err != nil {
		return nil, err
	}
	// No default model: the deployer picks one the account actually has
	// (see `jaas-app models`).
	if cfg.OpenAIModel = strings.TrimSpace(os.Getenv("OPENAI_MODEL")); cfg.OpenAIModel == "" {
		return nil, fmt.Errorf("OPENAI_MODEL is required (run `jaas-app models` to list the ones available)")
	}

	hours, err := envInt("MAX_MEETING_HOURS", 24)
	if err != nil || hours < 1 || hours > 48 {
		return nil, fmt.Errorf("MAX_MEETING_HOURS must be between 1 and 48")
	}
	cfg.MaxMeeting = time.Duration(hours) * time.Hour
	cfg.TranscribeModel = envDefault("TRANSCRIBE_MODEL", "gpt-transcribe")
	if cfg.TranscribeModel != "gpt-transcribe" {
		return nil, fmt.Errorf("TRANSCRIBE_MODEL must be gpt-transcribe for this pilot; no automatic fallback is enabled")
	}
	cfg.TranscribePrompt = strings.TrimSpace(os.Getenv("TRANSCRIBE_PROMPT"))
	if cfg.AudioRetentionDays, err = envInt("AUDIO_RETENTION_DAYS", 30); err != nil || cfg.AudioRetentionDays < 1 || cfg.AudioRetentionDays > 90 {
		return nil, fmt.Errorf("AUDIO_RETENTION_DAYS must be between 1 and 90")
	}
	audioStorageMB, err := envInt("AUDIO_MAX_STORAGE_MB", 10240)
	if err != nil || audioStorageMB < 100 || audioStorageMB > 1048576 {
		return nil, fmt.Errorf("AUDIO_MAX_STORAGE_MB must be between 100 and 1048576")
	}
	cfg.AudioMaxStorageBytes = int64(audioStorageMB) << 20
	if cfg.WorkerMaxAttempts, err = envInt("WORKER_MAX_ATTEMPTS", 5); err != nil {
		return nil, err
	}
	maxMB, err := envInt("MAX_TRANSCRIPT_MB", 20)
	if err != nil {
		return nil, err
	}
	cfg.MaxTranscriptSize = int64(maxMB) << 20
	if cfg.MaxTranscriptChars, err = envInt("MAX_TRANSCRIPT_CHARS", 500000); err != nil {
		return nil, err
	}

	cfg.DownloadAllowedHosts = splitList(envDefault("DOWNLOAD_ALLOWED_HOSTS", "objectstorage.*.oraclecloud.com"))
	if cfg.TrustedProxies, err = parseCIDRs(splitList(envDefault("TRUSTED_PROXIES", "127.0.0.0/8,::1,172.16.0.0/12"))); err != nil {
		return nil, err
	}

	if cfg.TranscriptRetention, err = loadRetention("TRANSCRIPT", 90); err != nil {
		return nil, err
	}
	if cfg.MinutesRetention, err = loadRetention("MINUTES", 365); err != nil {
		return nil, err
	}
	if cfg.DisplayTimezone, err = time.LoadLocation(envDefault("DISPLAY_TIMEZONE", "America/Mexico_City")); err != nil {
		return nil, fmt.Errorf("DISPLAY_TIMEZONE: %w", err)
	}

	if cfg.SMTPHost = strings.TrimSpace(os.Getenv("SMTP_HOST")); cfg.SMTPHost != "" {
		if cfg.SMTPPort, err = envInt("SMTP_PORT", 587); err != nil {
			return nil, err
		}
		cfg.SMTPSecurity = envDefault("SMTP_SECURITY", "starttls")
		if cfg.SMTPSecurity != "starttls" && cfg.SMTPSecurity != "tls" {
			return nil, fmt.Errorf("SMTP_SECURITY must be starttls or tls")
		}
		cfg.SMTPUser = strings.TrimSpace(os.Getenv("SMTP_USER"))
		if cfg.SMTPPassword, err = readSecret("SMTP_PASSWORD"); err != nil {
			return nil, err
		}
		if cfg.MailFrom = normalizeEmail(os.Getenv("MAIL_FROM")); cfg.MailFrom == "" {
			return nil, fmt.Errorf("MAIL_FROM must be a valid address when SMTP_HOST is set")
		}
		cfg.MailFromName = envDefault("MAIL_FROM_NAME", "Transgenia Reuniones")
		cfg.MailReviewer = envDefault("MAIL_REVIEWER", "un administrador de Transgenia")
		cfg.MailSignatureName = strings.TrimSpace(os.Getenv("MAIL_SIGNATURE_NAME"))
		cfg.MailSignatureRole = strings.TrimSpace(os.Getenv("MAIL_SIGNATURE_ROLE"))
		if v := strings.TrimSpace(os.Getenv("MAIL_REPLY_TO")); v != "" {
			if cfg.MailReplyTo = normalizeEmail(v); cfg.MailReplyTo == "" {
				return nil, fmt.Errorf("MAIL_REPLY_TO must be a valid address")
			}
		}
	}

	if (cfg.CFAccessTeamDomain == "") != (cfg.CFAccessAUD == "") {
		return nil, fmt.Errorf("CF_ACCESS_TEAM_DOMAIN and CF_ACCESS_AUD must be set together")
	}
	if os.Getenv("REQUIRE_CF_ACCESS") == "true" && cfg.CFAccessTeamDomain == "" {
		return nil, fmt.Errorf("REQUIRE_CF_ACCESS=true but CF_ACCESS_TEAM_DOMAIN/CF_ACCESS_AUD are empty")
	}

	return cfg, nil
}
