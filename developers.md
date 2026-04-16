# 🏗️ Developer Guide

Internal design documentation for the jaas-app codebase. See
[README.md](README.md) for setup and configuration.

## 🔐 Access Model

Three tiers of access, two passwords:

| Role      | Access                                                                          | Credential                                |
| --------- | ------------------------------------------------------------------------------- | ----------------------------------------- |
| **Admin** | Create/delete rooms, start/stop meetings, manage transcription, view recordings | Admin password (env var, one for the app) |
| **Host**  | Start a meeting, get moderator JWT                                              | Per-room host password (set at creation)  |
| **Guest** | Join an active meeting                                                          | Just the link `/m/{slug}`                 |

- Admin password is checked via a login form and stored in a signed session
  cookie (HttpOnly, 7-day lifetime).
- Host password can be entered on the join page — a POST validates it and
  returns a moderator JWT.
- Guests need nothing — once a host has started the meeting, guests join
  directly via the Jitsi iframe.

## 📄 Pages

1. **Home** (`/`) — public landing page directing visitors to use their
   host-provided meeting link.
2. **Login** (`/admin/login`) — single password field, sets session cookie,
   redirects to dashboard.
3. **Admin dashboard** (`/admin`) — create-room form (slug + host password),
   room list with status, start/stop controls, transcription toggle, recordings
   with download links and expiry, transcription links, and webhook setup info.
   Requires admin cookie.
4. **Join** (`/m/{slug}`) — asks for a display name; an "Are you the host?"
   toggle reveals the host password field.
5. **Waiting room** — shown when a guest joins before a host has started the
   meeting. Auto-resubmits the join form every 5 seconds; also shows a host
   password form for late-arriving hosts.
6. **Active meeting** — full-page Jitsi iframe via `JitsiMeetExternalAPI`.
   Dial-in number and PIN displayed below the iframe. Moderators get a
   `readyToClose` listener that marks the room inactive.
7. **Transcription viewer** (`/admin/transcriptions/{id}`) — parsed transcript
   with speaker name, timestamp, and content for each message. Requires admin
   cookie.

## 🛤️ Routes

| Method | Path                                | Auth   | Behavior                                                        |
| ------ | ----------------------------------- | ------ | --------------------------------------------------------------- |
| GET    | `/`                                 | none   | Public home page                                                |
| GET    | `/admin/login`                      | none   | Render login form                                               |
| POST   | `/admin/login`                      | none   | Validate admin password, set cookie, redirect to `/admin`       |
| POST   | `/admin/logout`                     | cookie | Clear cookie, redirect to `/admin/login`                        |
| GET    | `/admin`                            | cookie | Render admin dashboard                                          |
| POST   | `/admin/rooms`                      | cookie | Create room (slug + host password), redirect to `/admin`        |
| POST   | `/admin/rooms/{slug}/delete`        | cookie | Delete room, redirect to `/admin`                               |
| POST   | `/admin/rooms/{slug}/start`         | cookie | Admin-start a meeting, redirect to `/admin`                     |
| POST   | `/admin/rooms/{slug}/stop`          | cookie | Admin-stop a meeting, redirect to `/admin`                      |
| POST   | `/admin/rooms/{slug}/transcription` | cookie | Toggle transcription for a room                                 |
| GET    | `/admin/transcriptions/{id}`        | cookie | View a parsed transcription                                     |
| GET    | `/m/{slug}`                         | none   | Join page (or meeting iframe if JWT present)                    |
| POST   | `/m/{slug}/join`                    | none   | Validate host password or join as guest; start or enter meeting |
| POST   | `/m/{slug}/end`                     | none   | Mark room inactive (called by iframe `readyToClose`)            |
| POST   | `/webhook/recording`                | none   | Receive recording/transcription webhooks from JaaS              |

## 🗃️ Data Model

SQLite database with three tables:

```sql
CREATE TABLE rooms (
    id             INTEGER PRIMARY KEY,
    slug           TEXT UNIQUE NOT NULL,
    host_hash      TEXT NOT NULL,
    active         BOOLEAN DEFAULT FALSE,
    created_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
    transcription  BOOLEAN DEFAULT FALSE,
    activated_at   DATETIME
);

CREATE TABLE recordings (
    id           INTEGER PRIMARY KEY,
    room_slug    TEXT NOT NULL,
    kind         TEXT NOT NULL DEFAULT 'recording',
    download_url TEXT NOT NULL,
    expires_at   DATETIME NOT NULL,
    duration_sec INTEGER DEFAULT 0,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE transcriptions (
    id         INTEGER PRIMARY KEY,
    room_slug  TEXT NOT NULL,
    session_id TEXT NOT NULL,
    data       TEXT NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
```

- `slug` — URL path segment, unique, persistent for recurring meetings.
- `host_hash` — bcrypt hash of the per-room host password.
- `active` — set to true when a host starts the meeting, reset on end or after
  4-hour timeout.
- `activated_at` — tracks when the meeting was started; used for auto-expiry.
  NULLed when the meeting stops.
- `transcription` — per-room flag passed into the JWT to enable JaaS
  transcription.

## 🔑 JWT Generation

The server generates RS256 JWTs for the Jitsi iframe:

- **Guest JWT** — room name, display name, limited claims (join only).
- **Moderator JWT** — adds recording, mute-all, kick, and other moderator
  permissions.
- **Feature flags** — `recording` (moderator only), `transcription` (per-room
  setting), `livestreaming` (disabled).

## 📥 Recording and Transcription Webhooks

JaaS sends webhook events to `POST /webhook/recording`:

- **`RECORDING_UPLOADED`** — stores the download URL, duration, and expiry in
  the `recordings` table. Links default to 24-hour expiry if not provided.
  Expired recordings are filtered out of the dashboard.
- **`TRANSCRIPTION_UPLOADED`** — downloads the transcript JSON from the
  pre-authenticated link and stores it in the `transcriptions` table.

The room slug is extracted from the `fqn` field (`vpaas-xxx/room-slug`).
Configure the webhook URL in your JaaS console to point at
`https://your-domain/webhook/recording`.

## 🔄 Meeting Lifecycle

1. Admin creates a room with a slug and host password.
2. Admin shares `/m/{slug}` with participants.
3. Visitors see the join page; guests who join before a host see a waiting room
   that auto-refreshes.
4. A host enters the host password on the join page (or an admin clicks Start on
   the dashboard), which starts the meeting (`active=true`) and returns a
   moderator JWT.
5. Subsequent visitors enter a display name and join directly with a guest JWT.
6. When the meeting ends, the iframe `readyToClose` event marks the room
   inactive. A 4-hour server-side timeout acts as a fallback. Stale active rooms
   without an `activated_at` are deactivated on startup.

## 🔄 Self-Update

Run `jaas-app update` to check for a newer release on GitHub. If one exists, the
binary downloads the platform-appropriate asset and atomically replaces itself.
The version is embedded at build time via `-ldflags`.

## 📞 Phone Dial-In Internals

PSTN dial-in is built into JaaS — no SIP trunks or extra configuration needed.

- Each meeting room gets a deterministic PIN tied to the room name.
- PINs and numbers are fetched at room creation and displayed on the dashboard
  and join page.
- No JWT changes required for inbound dial-in.

API endpoints (no auth required):

- **DIDs**: `GET https://8x8.vc/v1/_jaas/vmms-conference-mapper/access/v1/dids`
- **PIN**:
  `GET https://8x8.vc/v1/_jaas/vmms-conference-mapper/v1/access?conference={room}@conference.{appId}.8x8.vc`

**Dial-out** (server calls a phone number to pull someone into a meeting) is a
separate paid feature requiring `"outbound-call"` in the JWT and billing on
file. Not currently implemented.

See:
[PSTN Dial In/Out docs](https://developer.8x8.com/jaas/docs/pstn-dial-in-and-out/)

## 📚 Reference

- [JaaS Console](https://jaas.8x8.vc/#/)
- [JaaS Onboarding Guide](https://developer.8x8.com/jaas/docs/jaas-onboarding/)
- [JaaS Developer Docs](https://developer.8x8.com/jaas/)
