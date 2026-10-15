# 📹 Jitsi as a Service Application

A web application for hosting [Jitsi as a Service](https://jaas.8x8.vc/#/)
meetings. It provides a simple interface for creating and managing video
meetings powered by the 8x8 JaaS platform.

The meeting flow is simple:

- A "room" is created by an admin with a URL and password specific to that room.
- Anyone with the room password can start the meeting and join as moderator. The
  meeting can also be started from the admin dashboard.
- Everyone else can join with just the URL once the meeting has been started.

## 🤔 Why I built this

- 🔗 Want a system where people can join a call with only a URL in a browser. No
  app to install, no holding pen, no passwords.
- 👥 Multiple people can host the call if the original admin can’t show up.
- 🐢 On rare instances, the free Jitsi servers (what I had been using) get
  overloaded, which is inconvenient. The 8x8 JAAS service is more reliable.
- 📝 Use 8x8 transcribing service. Easy to summarize by pasting into Claude, but
  could automate this too.
- 🖥️ Can host it on any small server that can run a Go app. 8x8 does the heavy
  lifting videoconferencing in an iframe.
- 🔐 I control the authentication, storing recordings/transcriptions, etc.

## ✨ Features

- **Meeting URL generation** — create shareable meeting links with custom slugs
- **No login required** — guests can join meetings without creating an account
- **Recording** — record meetings; download links delivered via webhook
- **Transcription** — enable per-room transcription; view parsed transcripts in
  the admin panel
- **AI meeting minutes (optional)** — with an OpenAI API key, each transcript
  becomes a draft with summary, key points, decisions, action items, risks, open
  questions and a text-plus-participation sentiment analysis; the host reviews
  it and approves who receives it by e-mail
- **Transcription without the JaaS premium feature (optional)** — each
  participant's browser records only their own microphone (never while muted)
  and OpenAI speech-to-text turns it into a speaker-labelled transcript
- **Phone dial-in** — allow participants to join via phone call (provided by
  8x8, no SIP configuration required)
- **Per-user rooms behind Cloudflare Access (optional)** — each signed-in person
  sees only their own rooms; `ADMIN_EMAILS` see all of them
- **Hardened by default** — signed webhooks, CSRF protection, server-side
  sessions, rate limits and a strict CSP; see [security.md](security.md)
- **Self-update** — `jaas-app update` checks GitHub Releases, verifies the
  binary's SHA-256 against the release checksums and replaces it in-place

## 📦 Install

Download the latest binary from
[GitHub Releases](https://github.com/cbrake/jaas-app/releases/latest):

```sh
# Linux (x86_64)
TAG=$(curl -s https://api.github.com/repos/cbrake/jaas-app/releases/latest | grep tag_name | cut -d'"' -f4)
curl -LO "https://github.com/cbrake/jaas-app/releases/download/$TAG/jaas-app-$TAG-Linux-x86_64"
curl -LO "https://github.com/cbrake/jaas-app/releases/download/$TAG/jaas-app_${TAG#v}_checksums.txt"
sha256sum --ignore-missing -c "jaas-app_${TAG#v}_checksums.txt"
mv "jaas-app-$TAG-Linux-x86_64" jaas-app && chmod +x jaas-app
```

To update an existing install:

```sh
jaas-app update
```

## 🚀 Getting Started

### 📋 Prerequisites

- A JaaS account and API key from [8x8](https://jaas.8x8.vc/#/)

### ⚙️ Setup

Copy the example environment file and fill in your JaaS credentials:

```sh
cp .env.example .env
```

## 🔧 Configuration

Every secret can be given inline (`NAME`) or, preferably, as a file
(`NAME_FILE`), which keeps it out of the process environment and
`docker inspect`.

### Required

| Variable                                           | Description                                                                                                                        |
| -------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| `PUBLIC_ORIGIN`                                    | `https://` origin browsers use, e.g. `https://mtg.example.com`; every POST must come from it                                       |
| `JAAS_APP_ID`                                      | Your JaaS application ID                                                                                                           |
| `JAAS_API_KEY_ID`                                  | Your JaaS API key ID (used as JWT `kid` header)                                                                                    |
| `JAAS_PRIVATE_KEY_FILE` / `JAAS_PRIVATE_KEY`       | RSA private key PEM (PKCS8 or PKCS1). `JAAS_API_KEY_PATH` / `JAAS_API_KEY` are still accepted                                      |
| `JAAS_WEBHOOK_SECRET_FILE` / `JAAS_WEBHOOK_SECRET` | Webhook signing secret from the JaaS console (Webhooks, "Reveal secret")                                                           |
| `ADMIN_INITIAL_PASSWORD_FILE`                      | First start only: at least 14 characters, stored hashed, must be changed at first login. `ADMIN_PASSWORD` is accepted the same way |

### General

| Variable                                | Default                           | Description                                                      |
| --------------------------------------- | --------------------------------- | ---------------------------------------------------------------- |
| `LISTEN_ADDR`                           | `:8370`                           | TCP address the HTTP server binds to                             |
| `DB_PATH`                               | `./jaas.db`                       | Path to the SQLite database file                                 |
| `DISPLAY_TIMEZONE`                      | `UTC`                             | IANA zone for times in the dashboard, minutes and e-mails        |
| `MAX_MEETING_HOURS`                     | `4`                               | How long a started room, its sessions and JWTs last (1–48)       |
| `TRUSTED_PROXIES`                       | `127.0.0.0/8,::1,172.16.0.0/12`   | Peers whose `X-Real-IP` is trusted for rate limiting             |
| `WEBHOOK_TOLERANCE_SECONDS`             | `86400`                           | Accepted signature age; JaaS retries keep the original timestamp |
| `DOWNLOAD_ALLOWED_HOSTS`                | `objectstorage.*.oraclecloud.com` | Hosts the server may download JaaS transcripts from (https only) |
| `TRANSCRIPT_RETENTION_MONTHS` / `_DAYS` | keep forever                      | Delete transcripts older than this                               |
| `MINUTES_RETENTION_MONTHS` / `_DAYS`    | keep forever                      | Delete minutes older than this                                   |
| `DEV_INSECURE_COOKIES`                  |                                   | `true` only for local development over plain HTTP                |

### AI minutes (optional)

Without `OPENAI_API_KEY` the app serves meetings, recordings and JaaS
transcripts exactly as before.

| Variable                                     | Default                     | Description                                                            |
| -------------------------------------------- | --------------------------- | ---------------------------------------------------------------------- |
| `OPENAI_API_KEY_FILE` / `OPENAI_API_KEY`     |                             | Enables minutes and browser-microphone transcription                   |
| `OPENAI_MODEL`                               |                             | Required with a key; `jaas-app models` lists the ones your key can use |
| `MINUTES_LANGUAGE`                           | `English`                   | Language the minutes are written in, e.g. `Spanish`                    |
| `TRANSCRIBE_MODEL`                           | `whisper-1`                 | Speech-to-text model; only `whisper-1` returns per-segment timestamps  |
| `TRANSCRIBE_LANGUAGE`                        | auto-detect                 | ISO-639-1 hint, e.g. `en`                                              |
| `TRANSCRIBE_PROMPT`                          |                             | Vocabulary hint: names, products, jargon                               |
| `OPENAI_BASE_URL`                            | `https://api.openai.com/v1` | Compatible endpoint                                                    |
| `WORKER_MAX_ATTEMPTS`                        | `5`                         | Retries with backoff before a draft is marked failed                   |
| `MAX_TRANSCRIPT_MB` / `MAX_TRANSCRIPT_CHARS` | `20` / `500000`             | Download and prompt size limits                                        |

### E-mail (optional)

Needed to notify hosts that minutes are ready and to send host-approved copies
to participants.

| Variable                                     | Default                     | Description                                                                   |
| -------------------------------------------- | --------------------------- | ----------------------------------------------------------------------------- |
| `SMTP_HOST`, `SMTP_PORT`                     | port `587`                  | Submission server                                                             |
| `SMTP_SECURITY`                              | `starttls`                  | `starttls` or `tls`                                                           |
| `SMTP_USER`, `SMTP_PASSWORD[_FILE]`          |                             | Credentials                                                                   |
| `MAIL_FROM`, `MAIL_FROM_NAME`                | name `BEC Systems Meetings` | Sender address and display name                                               |
| `MAIL_REPLY_TO`                              | approving host              | Shared mailbox for replies to participant copies                              |
| `MAIL_REVIEWER`                              | `the meeting host`          | Who the copy says reviewed the minutes                                        |
| `MAIL_SIGNATURE_NAME`, `MAIL_SIGNATURE_ROLE` |                             | Optional signature lines                                                      |
| `PRIVACY_URL`                                |                             | `https://` link in the join notice of transcribed rooms and in e-mail footers |

### Cloudflare Access (optional)

With Access in front of `/admin`, the app verifies the Access JWT itself and
switches to per-user rooms: each person sees and manages only the rooms they
created, and `ADMIN_EMAILS` see all of them. Without it, the dashboard uses the
single admin password.

| Variable                    | Description                                         |
| --------------------------- | --------------------------------------------------- |
| `CF_ACCESS_TEAM_DOMAIN`     | e.g. `example.cloudflareaccess.com`                 |
| `CF_ACCESS_AUD`             | Application audience tag                            |
| `CF_ACCESS_ALLOWED_DOMAINS` | Comma-separated e-mail domains allowed in           |
| `CF_ACCESS_ALLOWED_EMAILS`  | Comma-separated addresses allowed in                |
| `ADMIN_EMAILS`              | Addresses that see every room                       |
| `REQUIRE_CF_ACCESS`         | `true` refuses to start unless Access is configured |

## 🤖 AI minutes

1. Set `OPENAI_API_KEY` and `OPENAI_MODEL` (and SMTP to e-mail them).
2. Turn on **Transcribe** for a room. Participants then give their name, e-mail
   and consent before joining; rooms without transcription still need only a
   name.
3. The transcript comes either from JaaS (`TRANSCRIPTION_UPLOADED` webhook) or,
   with a key, from each participant's own microphone: the browser uploads
   15-second chunks while the participant is unmuted, the worker transcribes
   them with timestamps and merges the speakers by wall clock. Audio is deleted
   once transcribed.
4. A durable worker in the same process produces one draft per session through
   the Responses API with a strict JSON schema, re-validates it, and adds
   measured participation (talk time, words per minute, turns, interruptions).
   The transcript is treated as data, never as instructions.
5. The room owner gets an e-mail with a link (no meeting content). Nothing goes
   to participants until the host approves the recipients on the minutes page;
   each then receives an HTML summary plus the transcript (`.txt`) and minutes
   (`.md`).

## ⬆️ Upgrading from 0.1.x

- Add `PUBLIC_ORIGIN` and the JaaS webhook signing secret
  (`JAAS_WEBHOOK_SECRET`); unsigned webhooks are now rejected.
- `ADMIN_PASSWORD` keeps working, but only to create the first stored credential
  (at least 14 characters); you are asked to change it at first login. Later
  changes go through the dashboard or `jaas-app reset-admin`.
- `SESSION_SECRET` is no longer used: sessions are server-side.
- `JAAS_API_KEY_PATH` / `JAAS_API_KEY` keep working.
- The database migrates itself on start; take a backup first.

## 🏠 Hosting

The application is a single static Go binary with templates and assets embedded,
and it keeps all state in one SQLite file. That makes it a good fit for any host
that offers a small amount of persistent disk, and a poor fit for serverless
platforms with ephemeral or shared storage.

What a host needs to provide:

- **One long-running process.** Meeting state and webhook delivery both assume
  the process is up; there is no external queue or cache.
- **A persistent writable directory** for `DB_PATH`. SQLite cannot be shared
  across replicas, so run exactly one instance.
- **HTTPS on a stable public hostname.** JaaS posts recording and transcription
  webhooks to the app, and browsers require HTTPS for camera and microphone
  access.
- **~64 MB of RAM and a fraction of a CPU.** The video itself flows directly
  between participants and 8x8; this application only serves pages and tokens.

### Options

| Option                                   | Persistence            | Effort | Notes                                                                          |
| ---------------------------------------- | ---------------------- | ------ | ------------------------------------------------------------------------------ |
| VPS with systemd (Hetzner, Linode, etc.) | Local disk             | Low    | The setup this repository ships; see below. Roughly $5/month.                  |
| Docker or Podman on a VPS                | Named volume           | Low    | Use `Dockerfile` and `deploy/docker-compose.yml`. Pairs well with Caddy.       |
| Fly.io                                   | Fly volume             | Low    | See `deploy/fly.toml.example`. Keep `min_machines_running = 1`.                |
| Render, Railway, Koyeb                   | Attached disk          | Low    | Point `DB_PATH` at the mounted disk and hold the service at a single instance. |
| Home server or Raspberry Pi              | Local disk             | Medium | Add a tunnel (Cloudflare Tunnel, Tailscale Funnel) so webhooks can reach it.   |
| Kubernetes                               | `ReadWriteOnce` volume | Medium | A single-replica `StatefulSet`; `Recreate` rather than rolling updates.        |

Serverless function platforms (Lambda, Cloud Run with scale-to-zero, Vercel,
Workers) are not recommended: the SQLite file does not survive instance
recycling, and webhooks may arrive while no instance is warm.

### VPS with systemd

`deploy/jaas-app.service` runs the binary as a dedicated `jaas` user out of
`/opt/jaas-app`, and `envsetup.sh` provides a `jaas_deploy` helper that builds
for linux/amd64, copies the files over, and restarts the service:

```sh
. envsetup.sh && jaas_deploy
```

Put a reverse proxy in front for TLS. With Caddy the whole configuration is:

```caddyfile
mtg.example.com {
	reverse_proxy 127.0.0.1:8370 {
		header_up X-Real-IP {remote_host}
	}
}
```

The app trusts `X-Real-IP` only from `TRUSTED_PROXIES`, so rate limits apply to
the real client address.

### Docker

```sh
docker build -t jaas-app .
docker run -d --name jaas-app \
	-p 8370:8370 \
	-v jaas-data:/data \
	--env-file deploy/env \
	--restart unless-stopped \
	jaas-app
```

The image defaults to `DB_PATH=/data/jaas.db` and runs as a non-root user, so
mount a volume at `/data`. `deploy/docker-compose.yml` does the same thing with
Compose.

Note that `jaas-app update` replaces the binary in place, which suits the VPS
install. On container hosts, deploy a new image instead.

### Backups

The database holds rooms, host password hashes, recording links, transcripts and
minutes. `jaas-app backup` writes a consistent snapshot (`VACUUM INTO`) while
the service is up, and `jaas-app verify-db` checks one:

```sh
jaas-app backup "/var/backups/jaas-$(date +%F).db"
jaas-app verify-db "/var/backups/jaas-$(date +%F).db"
```

Keep the JaaS private key and the values in the environment file backed up
separately.

## 🔐 How It Works

- **Admin** logs in at `/admin` (admin password, or Cloudflare Access identity)
  to create/manage rooms and start meetings.
- **Host** receives a meeting link (`/m/{slug}`) and starts the meeting by
  entering the room's host password.
- **Guests** join with just the link — no account or password needed once a host
  has started the meeting.
- **Phone dial-in** is built into JaaS. Each room gets a phone number and PIN
  displayed on the join page — no extra setup required.
- **Recordings** are delivered via signed JaaS webhooks and shown in the admin
  dashboard with time-limited download links.
- **Transcriptions** can be enabled per-room; parsed transcripts are viewable in
  the admin panel.
- **Minutes** (optional) are drafted by the worker and sent only after the host
  approves the recipients.
- **Self-update** — run `jaas-app update` to check GitHub Releases, verify the
  SHA-256 checksum and replace the binary in-place.

## 📖 Developer Documentation

See [developers.md](developers.md) for architecture details: routes, data model,
JWT generation, webhook handling, and meeting lifecycle.

## 🔒 Security

See [security.md](security.md) for a security audit of the application.

## 📚 Reference

- [JaaS Console](https://jaas.8x8.vc/#/)
- [JaaS Onboarding Guide](https://developer.8x8.com/jaas/docs/jaas-onboarding/)
- [JaaS Developer Docs](https://developer.8x8.com/jaas/)
