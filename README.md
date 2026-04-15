# 📹 Jitsi as a Service Application

A web application for hosting [Jitsi as a Service](https://jaas.8x8.vc/#/)
meetings. It provides a simple interface for creating and managing video
meetings powered by the 8x8 JaaS platform.

## ✨ Features

- **Meeting URL generation** — create shareable meeting links with custom slugs
- **No login required** — guests can join meetings without creating an account
- **Recording** — record meetings for later playback
- **Phone dial-in** — allow participants to join via phone call (provided by
  8x8, no SIP configuration required)

## 🚀 Getting Started

### 📋 Prerequisites

- [Go](https://go.dev/) 1.22 or later
- A JaaS account and API key from [8x8](https://jaas.8x8.vc/#/)

### ⚙️ Setup

Copy the example environment file and fill in your JaaS credentials:

```sh
cp .env.example .env
```

### 💻 Development

```sh
go run .
```

### 🔨 Build

```sh
go build -o jaas-app .
```

## 🔧 Configuration

The application requires the following environment variables:

| Variable         | Description                               |
| ---------------- | ----------------------------------------- |
| `JAAS_APP_ID`    | Your JaaS application ID                  |
| `JAAS_API_KEY`   | Your JaaS private key (RS256, PEM format) |
| `ADMIN_PASSWORD` | Password to access the admin dashboard    |
| `SESSION_SECRET` | Secret used to sign session cookies       |

## 🏗️ Design

### 🔐 Access Model

Three tiers of access, two passwords:

| Role      | Access                                     | Credential                                |
| --------- | ------------------------------------------ | ----------------------------------------- |
| **Admin** | Create/delete rooms, view room list at `/` | Admin password (env var, one for the app) |
| **Host**  | Start a meeting, get moderator JWT         | Per-room host password (set at creation)  |
| **Guest** | Join an active meeting                     | Just the link `/m/{slug}`                 |

- Admin password is checked via a login form and stored in a signed session
  cookie (HttpOnly).
- Host password is checked inline on the waiting room page — a POST validates it
  and returns a moderator JWT.
- Guests need nothing — once a host has started the meeting, guests join
  directly via the Jitsi iframe.

### 📄 Pages

1. **Login** (`/login`) — single password field, sets session cookie, redirects
   to dashboard.
2. **Admin dashboard** (`/`) — create-room form (slug + host password) and a
   list of existing rooms with copy-link, dial-in number/PIN, and delete
   actions. Requires admin cookie.
3. **Meeting** (`/m/{slug}`) — two states:
   - **Waiting room** — shown when no host is present. Displays room name,
     "waiting for host" message, and a host-password field to start the meeting.
     Auto-refreshes every 5 seconds via `<meta http-equiv="refresh">`.
   - **Active meeting** — prompts for a display name, then loads the Jitsi
     iframe. Dial-in number and PIN displayed below the iframe.

### 🛤️ Routes

| Method | Path                   | Auth   | Behavior                                                        |
| ------ | ---------------------- | ------ | --------------------------------------------------------------- |
| GET    | `/login`               | none   | Render login form                                               |
| POST   | `/login`               | none   | Validate admin password, set cookie, redirect to `/`            |
| POST   | `/logout`              | cookie | Clear cookie, redirect to `/login`                              |
| GET    | `/`                    | cookie | Render admin dashboard                                          |
| POST   | `/rooms`               | cookie | Create room (slug + host password), redirect to `/`             |
| POST   | `/rooms/{slug}/delete` | cookie | Delete room, redirect to `/`                                    |
| GET    | `/m/{slug}`            | none   | If active: Jitsi iframe (guest JWT). If not: waiting room       |
| POST   | `/m/{slug}/start`      | none   | Validate host password, set active, redirect with moderator JWT |

### 🗃️ Data Model

Single SQLite table:

```sql
CREATE TABLE rooms (
    id         INTEGER PRIMARY KEY,
    slug       TEXT UNIQUE NOT NULL,
    host_hash  TEXT NOT NULL,
    active     BOOLEAN DEFAULT FALSE,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

- `slug` — URL path segment, unique, persistent for recurring meetings.
- `host_hash` — bcrypt hash of the per-room host password.
- `active` — set to true when a host starts the meeting, reset to false when the
  Jitsi session ends (via iframe `readyToClose` event) or after a server-side
  timeout (4 hours).

### 🔑 JWT Generation

The server generates RS256 JWTs for the Jitsi iframe:

- **Guest JWT** — room name, display name, limited claims (join only).
- **Moderator JWT** — adds recording, mute-all, kick, and other moderator
  permissions.

### 🔄 Meeting Lifecycle

1. Admin creates a room with a slug and host password.
2. Admin shares `/m/{slug}` with participants.
3. Visitors see the waiting room until a host enters the host password.
4. Host password validation starts the meeting (`active=true`) and returns a
   moderator JWT embedded in the Jitsi iframe URL.
5. Subsequent visitors get a guest JWT and join directly.
6. When the meeting ends, the iframe `readyToClose` event marks the room
   inactive. A 4-hour server-side timeout acts as a fallback.

## 📞 Phone Dial-In

PSTN dial-in is built into JaaS and enabled by default on all accounts — no SIP
trunks or extra configuration needed.

- **8x8 provides phone numbers** in multiple countries, auto-selected based on
  caller location.
- **Each meeting room gets a deterministic PIN** tied to the room name. A phone
  caller dials the number, enters the PIN, and is routed to the correct room.
- **PINs and numbers can be fetched ahead of time** — no need to wait for the
  meeting to start. The app fetches them at room creation and displays them on
  the admin dashboard and the meeting join page.
- **No JWT changes required** for inbound dial-in.

API endpoints (no auth required):

- **DIDs**: `GET https://8x8.vc/v1/_jaas/vmms-conference-mapper/access/v1/dids`
- **PIN**: `GET https://8x8.vc/v1/_jaas/vmms-conference-mapper/v1/access?conference={room}@conference.{appId}.8x8.vc`

**Dial-out** (server calls a phone number to pull someone into a meeting) is a
separate paid feature requiring `"outbound-call"` in the JWT and billing on
file. Not currently implemented.

See:
[PSTN Dial In/Out docs](https://developer.8x8.com/jaas/docs/pstn-dial-in-and-out/)

## 📚 Reference

- [JaaS Console](https://jaas.8x8.vc/#/)
- [JaaS Onboarding Guide](https://developer.8x8.com/jaas/docs/jaas-onboarding/)
- [JaaS Developer Docs](https://developer.8x8.com/jaas/)
