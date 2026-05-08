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
- **Phone dial-in** — allow participants to join via phone call (provided by
  8x8, no SIP configuration required)
- **Self-update** — `jaas-app update` checks GitHub Releases and replaces the
  binary in-place

## 📦 Install

Download the latest binary from
[GitHub Releases](https://github.com/cbrake/jaas-app/releases/latest):

```sh
# Linux (x86_64)
curl -Lo jaas-app https://github.com/cbrake/jaas-app/releases/latest/download/jaas-app-$(curl -s https://api.github.com/repos/cbrake/jaas-app/releases/latest | grep tag_name | cut -d'"' -f4)-linux-x86_64
chmod +x jaas-app
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

| Variable            | Required | Default     | Description                                       |
| ------------------- | -------- | ----------- | ------------------------------------------------- |
| `JAAS_APP_ID`       | Yes      |             | Your JaaS application ID                          |
| `JAAS_API_KEY_ID`   | Yes      |             | Your JaaS API key ID (used as JWT `kid` header)   |
| `JAAS_API_KEY_PATH` | Yes      |             | Path to RSA private key PEM file (PKCS8 or PKCS1) |
| `ADMIN_PASSWORD`    | Yes      |             | Password to access the admin dashboard            |
| `SESSION_SECRET`    | Yes      |             | Secret used to sign session cookies (HMAC-SHA256) |
| `LISTEN_ADDR`       | No       | `:8370`     | TCP address the HTTP server binds to              |
| `DB_PATH`           | No       | `./jaas.db` | Path to the SQLite database file                  |

## 🔐 How It Works

- **Admin** logs in at `/admin` with the admin password to create and manage
  rooms.
- **Host** receives a meeting link (`/m/{slug}`) and starts the meeting by
  entering the room's host password.
- **Guests** join with just the link — no account or password needed once a host
  has started the meeting.
- **Phone dial-in** is built into JaaS. Each room gets a phone number and PIN
  displayed on the join page — no extra setup required.
- **Recordings** are delivered via JaaS webhooks and shown in the admin
  dashboard with time-limited download links.
- **Transcriptions** can be enabled per-room; parsed transcripts are viewable in
  the admin panel.
- **Self-update** — run `jaas-app update` to check GitHub Releases and replace
  the binary in-place.

## 📖 Developer Documentation

See [developers.md](developers.md) for architecture details: routes, data model,
JWT generation, webhook handling, and meeting lifecycle.

## 🔒 Security

See [security.md](security.md) for a security audit of the application.

## 📚 Reference

- [JaaS Console](https://jaas.8x8.vc/#/)
- [JaaS Onboarding Guide](https://developer.8x8.com/jaas/docs/jaas-onboarding/)
- [JaaS Developer Docs](https://developer.8x8.com/jaas/)
