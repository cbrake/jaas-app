# 🔒 Security Audit

Audit of the jaas-app codebase as of 2026-04-16.

## Summary

| Severity | Count |
| -------- | ----- |
| High     | 3     |
| Medium   | 6     |
| Low      | 4     |

## High Severity

### H1: Unauthenticated meeting end endpoint

`POST /m/{slug}/end` (`handlers.go:332`) has no authentication. Anyone who knows
or guesses a room slug can terminate an active meeting.

**Impact:** Denial of service — an attacker can end any meeting at any time.

**Fix:** Restrict to the moderator's session.

### H2: Unauthenticated and unverified webhook endpoint

`POST /webhook/recording` (`handlers.go:386`) has no signature verification.
JaaS supports webhook signing, but the app does not validate signatures. Any
external party can POST fabricated events.

**Impact:**

- Inject arbitrary recording URLs into the database, including `javascript:`
  URIs that execute when an admin clicks the download link in the dashboard
  (`dashboard.html:153` renders `<a href="{{.DownloadURL}}">`).
- Trigger SSRF via `TRANSCRIPTION_UPLOADED` — the handler fetches
  `payload.Data.PreAuthenticatedLink` (`handlers.go:442`) with no URL
  validation, allowing an attacker to make the server issue requests to internal
  hosts.
- Fill the database with junk recordings/transcriptions (DoS).

**Fix:**

1. Validate the JaaS webhook signature on every request.
2. Validate that `PreAuthenticatedLink` is an HTTPS URL on an expected domain
   (e.g., `*.oovoo.8x8.com` or `*.oovoo.cloud.oovoo.com`).
3. Validate that `DownloadURL` has an `https://` scheme before storing.

### H3: No CSRF protection on any form

No POST endpoint uses CSRF tokens. All forms — admin room management, login,
join, meeting end — rely solely on the session cookie or are entirely
unauthenticated.

**Impact:** An attacker can craft a page that, when visited by an authenticated
admin, silently creates rooms, deletes rooms, starts/stops meetings, or toggles
transcription. Combined with H1, a malicious page visited by any user can end
meetings.

**Fix:** Add a CSRF token (e.g., double-submit cookie or synchronizer token) to
all POST forms. At minimum, check the `Origin` or `Referer` header against the
expected host.

## Medium Severity

### M1: JWT exposed in URL query parameters

After login or join, the JWT is passed via redirect query parameter
(`handlers.go:209,310,329`):

```
/m/{slug}?jwt=...&mod=1&name=...
```

While `history.replaceState` strips it client-side (`meeting.html:32`), the JWT
may leak via:

- Server access logs
- Browser history (before JS executes)
- Referrer headers if the page links to external resources
- Proxy/CDN logs

**Fix:** Use a short-lived server-side session or POST-based token exchange
instead of URL parameters.

### M2: Admin password compared with `!=` (not constant-time)

`handlers.go:79` compares the admin password with `!=`:

```go
if password != app.Config.AdminPassword {
```

This is vulnerable to timing side-channels. While hard to exploit over the
network, it is a deviation from best practice for password comparison.

**Fix:** Use `subtle.ConstantTimeCompare` or, better, hash the admin password
with bcrypt at startup (like host passwords) and use
`bcrypt.CompareHashAndPassword`.

### M3: Session cookie missing `Secure` flag

`auth.go:18` sets the session cookie without `Secure: true`:

```go
return &http.Cookie{
    Name:     sessionCookieName,
    ...
    HttpOnly: true,
    SameSite: http.SameSiteLaxMode,
    // Secure: missing
}
```

**Impact:** The cookie is sent over plain HTTP, enabling session hijacking on
non-TLS connections or mixed-content scenarios.

**Fix:** Set `Secure: true` when not in development mode.

### M4: No rate limiting on authentication endpoints

Neither admin login (`POST /admin/login`) nor host password validation
(`POST /m/{slug}/join`) have rate limiting or account lockout.

**Impact:** Brute-force attacks against the admin password or room host
passwords.

**Fix:** Add rate limiting (e.g., per-IP with a token bucket) or exponential
backoff after failed attempts.

### M5: No Content-Security-Policy headers

No CSP headers are set on any response. The app loads an external script from
`8x8.vc` and uses inline scripts and styles.

**Impact:** Reduces defense-in-depth against XSS. If an injection vector is
found, there is no CSP to limit damage.

**Fix:** Add a CSP header. At minimum:

```
Content-Security-Policy: default-src 'self'; script-src 'self' https://8x8.vc 'unsafe-inline'; style-src 'self' 'unsafe-inline'; frame-src https://8x8.vc
```

### M6: No request body size limit on webhook

`handlers.go:387` calls `io.ReadAll(r.Body)` with no size limit.

**Impact:** An attacker can send a very large body to exhaust memory (DoS).
Similarly, the transcription download (`handlers.go:449`) reads the full
response body without a size cap.

**Fix:** Use `io.LimitReader` (e.g., 10 MB) for both the webhook body and the
transcription download.

## Low Severity

### L1: Webhook logs full request body

`handlers.go:393` logs the entire webhook body:

```go
log.Printf("webhook: received: %s", string(body))
```

**Impact:** May log sensitive URLs (pre-authenticated download links) to disk.

**Fix:** Log only event type and room slug, not the full payload.

### L2: Self-update has no checksum or signature verification

`update.go` downloads a binary from GitHub over HTTPS and replaces the running
executable without verifying a checksum or cryptographic signature.

**Impact:** A compromised GitHub account, DNS hijack, or MITM (unlikely with TLS
but not impossible) could deliver a malicious binary.

**Fix:** Publish checksums or GPG signatures with releases and verify them
before replacing the binary.

### L3: Slug validation only on frontend

The slug input has an HTML `pattern="[a-z0-9\-]+"` attribute
(`dashboard.html:118`), but there is no server-side validation in
`handleCreateRoom` (`handlers.go:146`). An API call bypassing the browser can
create slugs with arbitrary characters.

**Impact:** Unexpected characters in slugs could cause issues in URL routing, JS
contexts in templates, or filesystem paths if slugs are ever used in file names.

**Fix:** Validate the slug server-side with a regex like `^[a-z0-9\-]+$`.

### L4: Go template escaping in JS contexts

Go's `html/template` provides context-aware escaping that handles JS string
contexts inside `<script>` blocks. Values like `{{.JWT}}`, `{{.DisplayName}}`,
and `{{.Slug}}` in `meeting.html` and `{{.Slug}}` in `dashboard.html` inline JS
are escaped appropriately by the template engine.

This is currently safe, but is fragile — if templates are ever switched to
`text/template` or values are passed through `template.JS()`, the protection
breaks. The current approach relies on an implicit guarantee that is easy to
violate in future changes.

**Fix:** Consider passing dynamic values via `data-*` attributes and reading
them in JS, rather than interpolating directly into `<script>` blocks.

## Positive Findings

- **bcrypt** is used for host password hashing with default cost.
- **HMAC-SHA256** session signing with constant-time comparison (`hmac.Equal`).
- **Parameterized SQL** — all database queries use `?` placeholders; no SQL
  injection vectors found.
- **html/template** — the app uses Go's auto-escaping HTML template engine, not
  `text/template`.
- **JWT removal from URL** — `history.replaceState` strips the token from the
  address bar immediately on page load.
- **HttpOnly and SameSite=Lax** on session cookies.
- **4-hour meeting auto-expiry** prevents rooms from staying active
  indefinitely.
- **Embedded static files** — no directory traversal risk from the filesystem.
