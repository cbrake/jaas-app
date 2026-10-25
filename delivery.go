package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"net/textproto"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// --- e-mail addresses ---

var emailPattern = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)+$`)

// normalizeEmail returns the lower-cased address or "" when it is not a
// plain addr-spec. Rejecting anything else (spaces, CR/LF, angle brackets,
// display names) also rules out header injection.
func normalizeEmail(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) > 254 || !emailPattern.MatchString(s) {
		return ""
	}
	return s
}

// --- participants ---

// RecordJoin stores an e-mail given on our join form (or the Access
// identity of a host). It is the fallback when JaaS PARTICIPANT_JOINED
// events are not available for a session.
func (db *DB) RecordJoin(slug, email, name string, moderator bool) error {
	if email == "" {
		return nil
	}
	_, err := db.conn.Exec(
		"INSERT INTO session_participants (room_slug, session_id, email, display_name, moderator, source, joined_at) VALUES (?, '', ?, ?, ?, 'join', ?)",
		slug, email, name, moderator, db.unix(),
	)
	return err
}

// RecordJaaSParticipant stores who actually joined a JaaS session, from a
// signed PARTICIPANT_JOINED webhook.
func (db *DB) RecordJaaSParticipant(slug, sessionID, email, name string, moderator bool, at time.Time) error {
	_, err := db.conn.Exec(
		`INSERT INTO session_participants (room_slug, session_id, email, display_name, moderator, source, joined_at)
		 VALUES (?, ?, ?, ?, ?, 'jaas', ?)
		 ON CONFLICT (room_slug, session_id, email) WHERE source = 'jaas' DO NOTHING`,
		slug, sessionID, email, name, moderator, at.Unix(),
	)
	return err
}

type Recipient struct {
	Email     string
	Name      string
	InSession bool   // confirmed by JaaS for this session
	Status    string // delivery status, "" when not sent yet
	Error     string
}

// ProposedRecipients lists who took part in the meeting behind minutes m:
// participants JaaS reported for the session, plus people who used our join
// form in the room during the meeting window (session start, or 4 h before
// the transcript event, until the event). The host reviews this list.
func (db *DB) ProposedRecipients(m *Minutes) ([]Recipient, error) {
	byEmail := map[string]*Recipient{}
	start := m.MeetingAt.Add(-4 * time.Hour)
	if runID, ok := strings.CutPrefix(m.SessionID, "captura-"); ok {
		var started int64
		if db.conn.QueryRow("SELECT started_at FROM capture_runs WHERE id = ?", runID).Scan(&started) == nil {
			start = time.Unix(started, 0)
		}
	}

	rows, err := db.conn.Query(
		"SELECT email, display_name, joined_at FROM session_participants WHERE room_slug = ? AND session_id = ? AND source = 'jaas'",
		m.RoomSlug, m.SessionID)
	if err != nil {
		return nil, err
	}
	first := time.Time{}
	for rows.Next() {
		var email, name string
		var joined int64
		if err := rows.Scan(&email, &name, &joined); err != nil {
			rows.Close()
			return nil, err
		}
		byEmail[email] = &Recipient{Email: email, Name: name, InSession: true}
		if t := time.Unix(joined, 0); first.IsZero() || t.Before(first) {
			first = t
		}
	}
	rows.Close()
	if !first.IsZero() {
		start = first
	}

	speakers, err := db.captureSpeakers(m.SessionID)
	if err != nil {
		return nil, err
	}
	for _, sp := range speakers {
		byEmail[sp[0]] = &Recipient{Email: sp[0], Name: sp[1], InSession: true}
	}
	// Browser-capture sessions have no JaaS session id: JaaS participants are
	// matched by time window, like join-form entries.
	sources := "'join'"
	if len(byEmail) == 0 || len(speakers) > 0 {
		sources = "'join','jaas'"
	}
	rows, err = db.conn.Query(
		`SELECT email, display_name FROM session_participants
		 WHERE room_slug = ? AND source IN (`+sources+`) AND joined_at BETWEEN ? AND ? ORDER BY joined_at`,
		m.RoomSlug, start.Add(-15*time.Minute).Unix(), m.MeetingAt.Add(5*time.Minute).Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var email, name string
		if err := rows.Scan(&email, &name); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := byEmail[email]; !ok {
			byEmail[email] = &Recipient{Email: email, Name: name}
		}
	}
	rows.Close()

	rows, err = db.conn.Query("SELECT email, display_name, status, COALESCE(error, '') FROM minutes_deliveries WHERE minutes_id = ?", m.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var email, name, status, errMsg string
		if err := rows.Scan(&email, &name, &status, &errMsg); err != nil {
			rows.Close()
			return nil, err
		}
		r, ok := byEmail[email]
		if !ok {
			r = &Recipient{Email: email, Name: name}
			byEmail[email] = r
		}
		r.Status, r.Error = status, errMsg
	}
	rows.Close()

	out := make([]Recipient, 0, len(byEmail))
	for _, r := range byEmail {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

// QueueDeliveries enqueues one delivery per address; an address that was
// already queued or sent for these minutes is never sent twice.
func (db *DB) QueueDeliveries(minutesID int64, recipients []Recipient, approvedBy string) (int, error) {
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := db.unix()
	queued := 0
	var cancelled sql.NullInt64
	if err := tx.QueryRow("SELECT cancelled_at FROM minutes WHERE id=?", minutesID).Scan(&cancelled); err != nil {
		return 0, err
	}
	if cancelled.Valid {
		return 0, errAnalysisCancelled
	}
	for _, r := range recipients {
		res, err := tx.Exec(
			`INSERT INTO minutes_deliveries (minutes_id, email, display_name, status, next_attempt_at, approved_by, created_at)
			 VALUES (?, ?, ?, 'queued', ?, ?, ?) ON CONFLICT (minutes_id, email) DO NOTHING`,
			minutesID, r.Email, r.Name, now, approvedBy, now)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		queued += int(n)
	}
	return queued, tx.Commit()
}

type delivery struct {
	ID         int64
	MinutesID  int64
	Email      string
	Name       string
	Attempts   int
	ApprovedBy string
}

func (db *DB) ClaimNextDelivery(lease time.Duration) (*delivery, error) {
	now := db.now()
	var d delivery
	err := db.conn.QueryRow(
		`UPDATE minutes_deliveries SET attempts = attempts + 1, next_attempt_at = ?
		 WHERE id = (SELECT d.id FROM minutes_deliveries d JOIN minutes m ON m.id=d.minutes_id WHERE d.status = 'queued' AND d.next_attempt_at <= ? AND m.cancelled_at IS NULL ORDER BY d.next_attempt_at, d.id LIMIT 1)
		 RETURNING id, minutes_id, email, display_name, attempts, approved_by`,
		now.Add(lease).Unix(), now.Unix(),
	).Scan(&d.ID, &d.MinutesID, &d.Email, &d.Name, &d.Attempts, &d.ApprovedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

func (db *DB) FinishDelivery(id int64, status, errMsg string, retryIn time.Duration) error {
	now := db.now()
	switch status {
	case "sent":
		_, err := db.conn.Exec("UPDATE minutes_deliveries SET status = 'sent', error = NULL, sent_at = ? WHERE id = ?", now.Unix(), id)
		return err
	case "queued":
		_, err := db.conn.Exec("UPDATE minutes_deliveries SET status = 'queued', error = ?, next_attempt_at = ? WHERE id = ?", errMsg, now.Add(retryIn).Unix(), id)
		return err
	default:
		_, err := db.conn.Exec("UPDATE minutes_deliveries SET status = 'failed', error = ? WHERE id = ?", errMsg, id)
		return err
	}
}

// --- mail ---

type Attachment struct {
	Name        string
	ContentType string
	Data        []byte
	ContentID   string // set for images shown inside the HTML (cid:...)
}

type Mail struct {
	To          string
	ReplyTo     string
	Subject     string
	Text        string
	HTML        string       // optional alternative to Text
	Inline      []Attachment // images referenced by the HTML as cid:<ContentID>
	Attachments []Attachment
}

type Mailer interface {
	Send(ctx context.Context, m *Mail) error
}

// smtpMailer sends through an authenticated submission server. STARTTLS is
// mandatory on port 587 ("starttls"); "tls" is implicit TLS (465).
type smtpMailer struct {
	Host     string
	Port     int
	Security string
	User     string
	Password string
	From     string
	FromName string
}

func (s *smtpMailer) Send(ctx context.Context, m *Mail) error {
	if normalizeEmail(m.To) == "" || (m.ReplyTo != "" && normalizeEmail(m.ReplyTo) == "") {
		return permanent("smtp: invalid address")
	}
	msg, err := buildMIME(s.From, s.FromName, m, time.Now())
	if err != nil {
		return permanent("smtp: cannot build message")
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConf := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if s.Security == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, tlsConf)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return errors.New("smtp: connection failed")
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return smtpError(err)
	}
	defer c.Close()
	if s.Security != "tls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return permanent("smtp: server does not offer STARTTLS")
		}
		if err := c.StartTLS(tlsConf); err != nil {
			return errors.New("smtp: STARTTLS failed")
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Password, s.Host)); err != nil {
			return smtpError(err)
		}
	}
	if err := c.Mail(s.From); err != nil {
		return smtpError(err)
	}
	if err := c.Rcpt(m.To); err != nil {
		return smtpError(err)
	}
	w, err := c.Data()
	if err != nil {
		return smtpError(err)
	}
	if _, err := w.Write(msg); err != nil {
		return errors.New("smtp: write failed")
	}
	if err := w.Close(); err != nil {
		return smtpError(err)
	}
	return c.Quit()
}

// smtpError keeps only the reply code: server texts often echo addresses.
func smtpError(err error) error {
	var tp *textproto.Error
	if errors.As(err, &tp) {
		e := fmt.Errorf("smtp: %d", tp.Code)
		if tp.Code >= 500 {
			return permanent(e.Error())
		}
		return e
	}
	return errors.New("smtp: protocol error")
}

func buildMIME(from, fromName string, m *Mail, now time.Time) ([]byte, error) {
	var b bytes.Buffer
	boundary := randomHex(16)
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", mime.QEncoding.Encode("utf-8", fromName)+" <"+from+">")
	h("To", "<"+m.To+">")
	if m.ReplyTo != "" {
		h("Reply-To", "<"+m.ReplyTo+">")
	}
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", now.UTC().Format(time.RFC1123Z))
	h("Message-ID", "<"+randomHex(16)+"@"+from[strings.LastIndex(from, "@")+1:]+">")
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")
	h("Content-Type", `multipart/mixed; boundary="`+boundary+`"`)
	b.WriteString("\r\n")

	writeQP := func(contentType, body string) error {
		qp := quotedprintable.NewWriter(&b)
		if _, err := qp.Write([]byte(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n"))); err != nil {
			return err
		}
		qp.Close()
		b.WriteString("\r\n")
		return nil
	}
	if m.HTML == "" {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary)
		if err := writeQP("text/plain", m.Text); err != nil {
			return nil, err
		}
	} else {
		// mixed > related > (alternative > text, html) + inline images
		rel := randomHex(16)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: multipart/related; type=\"multipart/alternative\"; boundary=\"%s\"\r\n\r\n", boundary, rel)
		alt := randomHex(16)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", rel, alt)
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", alt)
		if err := writeQP("text/plain", m.Text); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", alt)
		if err := writeQP("text/html", m.HTML); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "--%s--\r\n", alt)
		for _, img := range m.Inline {
			fmt.Fprintf(&b, "--%s\r\nContent-Type: %s\r\nContent-Transfer-Encoding: base64\r\nContent-ID: <%s>\r\nContent-Disposition: inline; filename=\"%s\"\r\n\r\n",
				rel, img.ContentType, img.ContentID, img.Name)
			writeBase64(&b, img.Data)
		}
		fmt.Fprintf(&b, "--%s--\r\n", rel)
	}

	for _, a := range m.Attachments {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s\r\nContent-Transfer-Encoding: base64\r\nContent-Disposition: attachment; filename=\"%s\"\r\n\r\n",
			boundary, a.ContentType, a.Name)
		writeBase64(&b, a.Data)
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), nil
}

func writeBase64(b *bytes.Buffer, data []byte) {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 76 {
		b.WriteString(enc[:76] + "\r\n")
		enc = enc[76:]
	}
	b.WriteString(enc + "\r\n")
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- composing the delivery ---

func (app *App) composeDelivery(d *delivery) (*Mail, error) {
	m, err := app.DB.GetMinutes(d.MinutesID)
	if err != nil {
		return nil, permanent("la minuta ya no existe")
	}
	if m.Status != "ready" {
		return nil, permanent("la minuta no está lista")
	}
	var res MinutesResult
	if err := json.Unmarshal([]byte(m.ResultJSON), &res); err != nil {
		return nil, permanent("minuta ilegible")
	}
	t, err := app.DB.GetTranscriptionBySession(m.RoomSlug, m.SessionID)
	if err != nil {
		return nil, permanent("la transcripción ya no está disponible")
	}
	transcript, _ := renderTranscript(t.Data, app.Config.DisplayTimezone)

	loc := app.Config.DisplayTimezone
	day := m.MeetingAt.In(loc).Format("2006-01-02")
	when := m.MeetingAt.In(loc).Format("2006-01-02 15:04")
	name := d.Name
	if name == "" {
		name = d.Email
	}
	// The copy never names the approving host: the review is attributed to a
	// role ("un administrador de Transgenia") and replies go to a shared
	// mailbox. Who approved stays in the delivery record for audit.
	reviewer := app.Config.MailReviewer
	if reviewer == "" {
		reviewer = "un administrador de Transgenia"
	}
	replyTo := app.Config.MailReplyTo
	if replyTo == "" {
		replyTo = normalizeEmail(d.ApprovedBy)
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Hola %s:\n\n", name)
	fmt.Fprintf(&body, "Gracias por participar en la reunión %s del %s (hora del centro de México).\n\n", m.RoomSlug, when)
	fmt.Fprintf(&body, "Adjuntamos la transcripción completa y la minuta. La minuta se generó con apoyo de inteligencia artificial a partir de la transcripción y %s la revisó antes de este envío. Si encuentras un error, responde a este correo.\n\n", reviewer)
	fmt.Fprintf(&body, "RESUMEN\n%s\n\n", res.Summary)
	fmt.Fprintf(&body, "SENTIMIENTO: %s. %s\n\n", res.Sentiment.Overall, res.Sentiment.Explanation)
	if len(res.KeyPoints) > 0 {
		body.WriteString("PUNTOS CLAVE\n")
		for _, k := range res.KeyPoints {
			fmt.Fprintf(&body, "- %s\n", k)
		}
		body.WriteString("\n")
	}
	if len(res.ActionItems) > 0 {
		body.WriteString("TAREAS\n")
		for _, it := range res.ActionItems {
			owner, due := "sin responsable", "sin fecha"
			if it.Owner != nil {
				owner = *it.Owner
			}
			if it.DueDate != nil {
				due = *it.DueDate
			}
			fmt.Fprintf(&body, "- %s (%s, %s)\n", it.Task, owner, due)
		}
		body.WriteString("\n")
	}
	fmt.Fprintf(&body, "Adjuntos: transcripción (.txt) y minuta completa (.md).\n\n")
	fmt.Fprintf(&body, "Aviso de privacidad: https://transgenia.org/legal-privacidad.html\n-- \n")
	if app.Config.MailSignatureName != "" {
		fmt.Fprintf(&body, "%s\n", app.Config.MailSignatureName)
		if app.Config.MailSignatureRole != "" {
			fmt.Fprintf(&body, "%s\n", app.Config.MailSignatureRole)
		}
	}
	body.WriteString("Transgenia: IA aplicada\n")

	subject := fmt.Sprintf("Transcripción y minuta: %s (%s)", m.RoomSlug, day)
	html, err := renderEmail(minutesEmail, minutesEmailData{
		Subject: subject, Preheader: firstSentence(res.Summary, 140), Eyebrow: "Minuta de reunión",
		Closing: "Si algo no es exacto o falta un acuerdo, responde a este correo y lo corregimos.",
		Room:    m.RoomSlug, When: meetingWhen(m.MeetingAt, loc), Name: name, Reviewer: reviewer,
		R: &res, Metrics: res.Metrics,
		SignatureName: app.Config.MailSignatureName, SignatureRole: app.Config.MailSignatureRole,
	})
	if err != nil {
		return nil, permanent("no se pudo armar el correo")
	}
	return &Mail{
		To:      d.Email,
		ReplyTo: replyTo,
		Subject: subject,
		Text:    body.String(),
		HTML:    html,
		Inline:  genInline(),
		Attachments: []Attachment{
			{Name: "transcripcion-" + m.RoomSlug + "-" + day + ".txt", ContentType: "text/plain; charset=utf-8", Data: []byte(transcript)},
			{Name: "minuta-" + m.RoomSlug + "-" + day + ".md", ContentType: "text/markdown; charset=utf-8", Data: []byte(minutesMarkdown(m, &res, loc))},
		},
	}, nil
}

const maxDeliveryAttempts = 3

// processDelivery sends one queued e-mail. Recipients never appear in logs.
func (app *App) processDelivery(ctx context.Context) bool {
	if app.Mailer == nil {
		return false
	}
	d, err := app.DB.ClaimNextDelivery(10 * time.Minute)
	if errors.Is(err, ErrNotFound) {
		return false
	}
	if err != nil {
		slog.Error("delivery: claim failed")
		return false
	}
	mail, err := app.composeDelivery(d)
	if err == nil {
		app.effectsMu.Lock()
		if app.DB.minutesCancelled(d.MinutesID) {
			err = errAnalysisCancelled
		} else {
			err = app.Mailer.Send(ctx, mail)
		}
		app.effectsMu.Unlock()
	}
	switch {
	case err == nil:
		app.DB.FinishDelivery(d.ID, "sent", "", 0)
		slog.Info("delivery: sent", "minutes", d.MinutesID, "delivery", d.ID)
	case isPermanent(err) || d.Attempts >= maxDeliveryAttempts:
		app.DB.FinishDelivery(d.ID, "failed", err.Error(), 0)
		slog.Warn("delivery: failed", "minutes", d.MinutesID, "delivery", d.ID, "attempt", d.Attempts, "error", err.Error())
	default:
		app.DB.FinishDelivery(d.ID, "queued", err.Error(), retryDelay(d.Attempts))
		slog.Warn("delivery: retry scheduled", "minutes", d.MinutesID, "delivery", d.ID, "attempt", d.Attempts, "error", err.Error())
	}
	return true
}

// --- "minutes ready" notice to the room owner ---

type hostNotice struct {
	MinutesID int64
	Email     string
	Attempts  int
}

func (db *DB) ClaimNextHostNotice(lease time.Duration) (*hostNotice, error) {
	now := db.now()
	var n hostNotice
	err := db.conn.QueryRow(
		`UPDATE host_notifications SET attempts = attempts + 1, next_attempt_at = ?
		 WHERE rowid = (SELECT h.rowid FROM host_notifications h JOIN minutes m ON m.id=h.minutes_id WHERE h.status = 'queued' AND h.next_attempt_at <= ? AND m.cancelled_at IS NULL ORDER BY h.next_attempt_at LIMIT 1)
		 RETURNING minutes_id, email, attempts`,
		now.Add(lease).Unix(), now.Unix(),
	).Scan(&n.MinutesID, &n.Email, &n.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &n, err
}

func (db *DB) FinishHostNotice(minutesID int64, email, status, errMsg string, retryIn time.Duration) error {
	now := db.now()
	switch status {
	case "sent":
		_, err := db.conn.Exec("UPDATE host_notifications SET status = 'sent', error = NULL, sent_at = ? WHERE minutes_id = ? AND email = ?", now.Unix(), minutesID, email)
		return err
	case "queued":
		_, err := db.conn.Exec("UPDATE host_notifications SET status = 'queued', error = ?, next_attempt_at = ? WHERE minutes_id = ? AND email = ?", errMsg, now.Add(retryIn).Unix(), minutesID, email)
		return err
	default:
		_, err := db.conn.Exec("UPDATE host_notifications SET status = 'failed', error = ? WHERE minutes_id = ? AND email = ?", errMsg, minutesID, email)
		return err
	}
}

// composeHostNotice deliberately carries no meeting content: only the room,
// the date, how many participants are proposed and the link to review.
func (app *App) composeHostNotice(n *hostNotice) (*Mail, error) {
	if app.DB.minutesCancelled(n.MinutesID) {
		return nil, errAnalysisCancelled
	}
	m, err := app.DB.GetMinutes(n.MinutesID)
	if err != nil {
		return nil, permanent("la minuta ya no existe")
	}
	recipients, err := app.DB.ProposedRecipients(m)
	if err != nil {
		return nil, err
	}
	pending := 0
	for _, r := range recipients {
		if r.Status == "" {
			pending++
		}
	}
	loc := app.Config.DisplayTimezone
	link := app.Config.PublicOrigin + "/admin/minutes/" + strconv.FormatInt(m.ID, 10)
	var b strings.Builder
	fmt.Fprintf(&b, "La minuta de la reunión %s del %s (hora del centro de México) está lista.\n\n", m.RoomSlug, m.MeetingAt.In(loc).Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "Revísala y, si está bien, aprueba a quién se envía la transcripción y la minuta:\n%s\n\n", link)
	switch pending {
	case 0:
		b.WriteString("No hay participantes con correo registrados para esta reunión.\n\n")
	case 1:
		b.WriteString("Hay 1 participante propuesto para recibir la copia.\n\n")
	default:
		fmt.Fprintf(&b, "Hay %d participantes propuestos para recibir la copia.\n\n", pending)
	}
	b.WriteString("Nada se envía a los participantes hasta que lo apruebes en el panel. Este aviso no incluye contenido de la reunión.\n-- \nTransgenia Reuniones\n")
	subject := fmt.Sprintf("Minuta lista para revisar: %s (%s)", m.RoomSlug, m.MeetingAt.In(loc).Format("2006-01-02"))
	pendingText := "No hay participantes con correo registrados para esta reunión."
	switch {
	case pending == 1:
		pendingText = "Hay 1 participante propuesto para recibir la copia."
	case pending > 1:
		pendingText = fmt.Sprintf("Hay %d participantes propuestos para recibir la copia.", pending)
	}
	html, err := renderEmail(hostEmail, hostEmailData{
		Subject: subject, Preheader: "Revisa la minuta y aprueba a quién se envía.", Eyebrow: "Reuniones · aviso al anfitrión",
		Closing: "Este aviso llega solo al anfitrión de la sala.",
		Room:    m.RoomSlug, When: meetingWhen(m.MeetingAt, loc), Pending: pendingText, Link: link,
	})
	if err != nil {
		return nil, permanent("no se pudo armar el aviso")
	}
	return &Mail{To: n.Email, Subject: subject, Text: b.String(), HTML: html, Inline: genInline()}, nil
}

func (app *App) processHostNotification(ctx context.Context) bool {
	if app.Mailer == nil {
		return false
	}
	n, err := app.DB.ClaimNextHostNotice(10 * time.Minute)
	if errors.Is(err, ErrNotFound) {
		return false
	}
	if err != nil {
		slog.Error("host notice: claim failed")
		return false
	}
	mail, err := app.composeHostNotice(n)
	if err == nil {
		app.effectsMu.Lock()
		if app.DB.minutesCancelled(n.MinutesID) {
			err = errAnalysisCancelled
		} else {
			err = app.Mailer.Send(ctx, mail)
		}
		app.effectsMu.Unlock()
	}
	switch {
	case err == nil:
		app.DB.FinishHostNotice(n.MinutesID, n.Email, "sent", "", 0)
		slog.Info("host notice: sent", "minutes", n.MinutesID)
	case isPermanent(err) || n.Attempts >= maxDeliveryAttempts:
		app.DB.FinishHostNotice(n.MinutesID, n.Email, "failed", err.Error(), 0)
		slog.Warn("host notice: failed", "minutes", n.MinutesID, "attempt", n.Attempts, "error", err.Error())
	default:
		app.DB.FinishHostNotice(n.MinutesID, n.Email, "queued", err.Error(), retryDelay(n.Attempts))
		slog.Warn("host notice: retry scheduled", "minutes", n.MinutesID, "attempt", n.Attempts, "error", err.Error())
	}
	return true
}
