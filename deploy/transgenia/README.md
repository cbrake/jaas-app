# Transgenia meetings (meet.transgenia.org)

Hardened fork of `cbrake/jaas-app` (used with the author's permission) that
adds signed JaaS webhooks, server-side sessions, CSRF, rate limits, a durable
minutes queue and an OpenAI Responses API worker. Minutes are **drafts for
human review**: participant e-mail requires host approval and the service does
not create tasks. See [PLAN-1.1.md](PLAN-1.1.md) for the current migration gates.

## Layout on the server

```
/opt/transgenia-meetings/            root:root 0755
├── compose.yaml                     from deploy/transgenia/compose.yaml
├── .env                             IMAGE_TAG, TGN_UID, TGN_GID (non-secret)
├── meetings.env                     from meetings.env.example, root:tgn-meetings 0640
├── secrets/                         root:tgn-meetings 0750, files 0440
│   ├── jaas_private_key             JaaS API key (PEM)
│   ├── jaas_webhook_secret          JaaS webhook endpoint secret ("Reveal secret")
│   ├── openai_api_key               dedicated project key for this service
│   └── admin_initial_password       only read on first start (bootstrap)
├── data/                            tgn-meetings 0700 — SQLite (WAL)
├── backups/                         root 0700 — verified snapshots + sha256
└── bin/                             scripts from deploy/transgenia/scripts
```

Container `tgn-meetings` (Compose project `transgenia-meetings`): non-root
(uid of the `tgn-meetings` system user), read-only root filesystem, all
capabilities dropped, `no-new-privileges`, 512 MB / 128 pids, published only on
`127.0.0.1:8370`.

## Build and deploy a commit

```sh
cd <checkout del fork> && git checkout <sha>
docker build --target test .                                # must pass
docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t transgenia-meetings:$(git rev-parse --short HEAD) .
sed -i "s/^IMAGE_TAG=.*/IMAGE_TAG=$(git rev-parse --short HEAD)/" /opt/transgenia-meetings/.env
cd /opt/transgenia-meetings && docker compose up -d && docker compose ps
```

Never use the upstream `update` command (removed here): updates go through a
reviewed commit, the test stage and a tagged image. Roll back by setting the
previous `IMAGE_TAG` only after reviewing the database/work queues and the
rollback contract in PLAN-1.1.md. An older worker does not enforce the new
approval/cancellation columns; a blind image rollback can start unwanted work.

## Secrets

Load each one with `bin/set-secret.sh <name>` (no echo, no argv, no history),
then `docker compose up -d --force-recreate`. Nothing secret lives in
`meetings.env`, Compose files, Git or logs.

## nginx and TLS (first time)

1. DNS: `A meet → <IP del origen>`, **DNS only** while the certificate is issued.
2. `bin/update-cloudflare-ips.sh` (writes the geo/zone/log-format conf and the real-IP snippet, `nginx -t`, reload).
3. Temporary port-80-only server for ACME, `certbot certonly --webroot -w /var/www/acme -d meet.transgenia.org`.
4. Install `nginx/meet.transgenia.org.conf`, `nginx -t && systemctl reload nginx`.
5. Switch the record to **Proxied**; SSL mode Full (strict) for the hostname.
6. Cloudflare Access application covering `meet.transgenia.org/admin*` and
   `meet.transgenia.org/visualizations*`, preserving the approved identity policy.
   Put the team domain and AUD tag in `meetings.env`, set `REQUIRE_CF_ACCESS=true`, recreate.
7. JaaS console → Webhooks: `https://meet.transgenia.org/webhook/recording`,
   events `TRANSCRIPTION_UPLOADED` and `RECORDING_UPLOADED`; copy the endpoint
   secret into `secrets/jaas_webhook_secret` **before** enabling the endpoint.

## Backups

`bin/backup.sh` (daily via `tgn-meetings-backup.timer`): `VACUUM INTO` inside
the container, `verify-db` (integrity check + counts) with the same image and
no network, sha256, keeps 14. Restore:

```sh
cd /opt/transgenia-meetings && docker compose stop
cp -p data/jaas.db data/jaas.db.before-restore-$(date -u +%Y%m%dT%H%MZ)
rm -f data/jaas.db-wal data/jaas.db-shm
install -o tgn-meetings -g tgn-meetings -m 0600 backups/meetings-<ts>.db data/jaas.db
docker compose up -d
```

## Admin access

**Identity mode (production).** With `CF_ACCESS_TEAM_DOMAIN`/`CF_ACCESS_AUD`
set, Cloudflare Access authenticates each person (one-time PIN to their
`@transgenia.org` mailbox) and the app verifies the Access JWT on every
`/admin` request. There is no shared password: each person creates and sees
only their own rooms, transcripts and minutes (`rooms.owner_email`);
`ADMIN_EMAILS` see and manage everything. Rooms of other users answer 404.
If the Access identity changes, the old panel session is revoked. Logout also
ends the Access session (`/cdn-cgi/access/logout`).

**Password mode (break-glass / local).** Without Access the panel falls back
to the single admin password:

* First login uses the temporary password from `secrets/admin_initial_password`
  and forces a change (min. 14 chars); every change revokes all sessions.
* Lost password: `docker exec -i tgn-meetings /app/jaas-app reset-admin` (reads
  a new temporary password from stdin).
* Sessions: random 256-bit tokens stored hashed, `__Host-` cookies (Secure,
  HttpOnly, SameSite=Lax), 2 h idle / 12 h absolute, revoked on logout.

## Retention

Deleting a room deletes its recordings, transcripts, minutes and participant
sessions (webhook idempotency keys are kept 30 days without room/session).
Recording links expire with JaaS (24 h; the videos stay at 8x8, not here).
Transcripts and minutes: `TRANSCRIPT_RETENTION_MONTHS` / `MINUTES_RETENTION_MONTHS`
(calendar months, Transgenia uses 9) or the `*_DAYS` variants (defaults 90 and
365). The effective values are logged at start-up.

## Transcription (browser capture)

Participants explicitly opt into capture before joining. The pre-join controls
select language (Spanish default), microphone and whether it is shared. A local
signal test does not upload audio or call a model. Each browser captures its own
microphone while unmuted, requesting mono 48 kHz and Opus 96 kbps; effective
settings depend on hardware/browser. Jitsi device IDs are not copied across
origins. Pistas rotate every ten minutes and upload original WebM chunks every
15 seconds with session, origin and CSRF checks.

An explicit final manifest proves received chunk count, size and hashes. Idle
time alone does not establish completeness. The host approves processing at
`/admin/captures`; before approval, no STT or summary request is started. The
single ASR is `gpt-transcribe`, without Whisper fallback or parallel JaaS
transcription processing. Results have window precision, not word timestamps;
speaker identity is unverified and individual metrics are withheld.

Source audio is stored privately in `/data/audio`, default 30 days with a
configurable global cap. It is not deleted immediately after STT. Cancelling
analysis blocks subsequent work and preserves audio under the retention policy;
already initiated requests may incur cost. Room deletion remains destructive.
Audio backup and real microphone validation are release prerequisites. See
PLAN-1.1.md for limits, migration, ambiguity handling and rollback.

## Sending transcripts and minutes to participants

Guests must give an e-mail and accept the transcription notice (with a link
to the privacy notice) to join; hosts from the team use their Access e-mail.
The e-mail goes into the JaaS JWT, so the signed `PARTICIPANT_JOINED` webhook
tells exactly who was in each session (add that event to the JaaS webhook).
When the minutes are ready the room owner gets one internal e-mail ("Minuta
lista para revisar") with the link to the minutes page — no meeting content,
only room, date and how many participants are proposed; rooms without owner
notify `ADMIN_EMAILS`. On that page the host sees the proposed recipients (confirmed
in the session, or joined through the link during the meeting), unticks
anyone and approves. Only addresses from that list can be chosen; each
address receives the minutes and the full transcript (attachments) once,
with Reply-To set to the host. Delivery is queued with 3 attempts; SMTP
replies are reduced to their code. Recipient addresses never appear in logs.
Participant e-mails follow the transcript retention and are deleted with the
room; deliveries are deleted with their minutes.

## Logs

Only method, path, status, timing and client IP; never query strings, bodies,
webhook payloads, pre-authenticated links, JWTs, transcripts or minutes.
`TestNoSecretsInLogs` enforces this.
