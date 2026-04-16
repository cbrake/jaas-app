# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.0.6] - 2025-04-16

### Fixed

- Recording and transcription timestamps display in user's local timezone
  instead of server UTC

## [0.0.5] - 2025-04-16

### Added

- Per-room transcription toggle on admin dashboard (controls JWT feature flag)
- Webhook endpoint (`POST /webhook/recording`) for JaaS recording and
  transcription events
- Recordings displayed on dashboard with 24-hour download links from JaaS
- Transcriptions downloaded from JaaS and stored permanently in SQLite
- Transcription viewer page (`GET /admin/transcriptions/{id}`) with speaker
  names and timestamps
- Webhook setup instructions with copyable URL on admin dashboard
- Raw webhook payload logging for debugging
- Responsive mobile layout for admin dashboard
- Inline copy icons for meeting links and dial-in info

### Changed

- Dashboard room cards restructured: header line with all info, single action
  row, expandable recordings/transcriptions sections

## [0.0.4] - 2025-04-16

### Added

- Room active/inactive status indicator on admin dashboard
- Stop button on dashboard to manually deactivate active meetings
- Auto-expire rooms after 4 hours of inactivity (matches JWT expiry)
- `activated_at` column to track when rooms were activated
- "Are you the host?" toggle on waiting page for late-arriving hosts
- Start button opens meeting in a new tab

### Changed

- Join page redesigned: clean guest view with collapsible host section
- `navigator.sendBeacon` used for meeting end signal (survives page teardown)
- `beforeunload` handler added as fallback to deactivate room on tab close

### Fixed

- Stale rooms from before `activated_at` migration auto-deactivated on startup

## [0.0.3] - 2025-04-15

### Added

- Deploy script (`envsetup.sh`) with `jaas_deploy` function for building,
  deploying, and restarting the service on the target server
- Systemd service file and deployment configuration
- BEC Systems logo on home, login, and join pages
- Home page at `/` directing users to contact BEC Systems for meeting links
- "Not found" page with contact info when meeting ID doesn't exist
- Join page with name and optional host password fields
- Admin name setting on dashboard (saved to localStorage)
- "Start" button on dashboard to join meetings directly as moderator
- "Copy Link" and "Copy Dial-in" buttons with visual feedback on dashboard
- Show/hide toggle for host password on room creation

### Changed

- Admin dashboard moved from `/` to `/admin` (login at `/admin/login`)
- Templates embedded in binary via `go:embed` instead of read from disk
- Reworked meeting join flow: name entry, optional host password, waiting room
  with auto-refresh, multiple moderators supported
- Dial-in API parsing updated to match actual 8x8 response format (array of
  objects)
- DIDs fetched once per dashboard load instead of per room
- Default listen port changed to 8370

### Fixed

- Waiting room auto-refresh no longer clears form inputs
- Jitsi iframe container visibility for moderator path
- Display name now passed through to Jitsi via `userInfo`

### Security

- JWT stripped from URL bar after meeting page loads via `history.replaceState`
- Stale JWT URLs rejected when meeting is no longer active
- Meeting join forms use `autocomplete` attributes to prevent credential
  cross-contamination between admin login and meeting passwords

## [0.0.2] - 2025-04-15

### Added

- CLI subcommands: `serve` (default), `update`, `version`
- Self-update command that downloads latest release from GitHub
- GoReleaser configuration for cross-platform binary releases
- CI/CD workflows for build/test and automated releases
- Changelog extraction script for release notes

## [0.0.1] - 2025-04-15

### Added

- Initial release
- SQLite-backed room management (CRUD)
- JaaS JWT generation (RS256) for guest and moderator tokens
- HMAC session cookie authentication with admin middleware
- 8x8 dial-in number and PIN lookup
- Server-rendered HTML pages: login, dashboard, waiting room, meeting (Jitsi
  iframe)
- Meeting URL generation with custom slugs
- Integration test suite
