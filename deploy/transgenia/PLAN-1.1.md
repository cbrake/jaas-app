# Plan 1.1: controlled release contract

This branch is a prepared implementation, not proof of deployment. It targets
meet.transgenia.org only. Do not use the upstream CLAUDE.md deployment target.

## Delivered behavior

- One ASR, gpt-transcribe through the pinned official Go SDK. GPT-5.4 mini remains
  the distinct summary stage. The SDK never retries requests automatically.
- Pre-join microphone and es/en/mixed selection; shared-microphone flag. Capture
  is opt-in, 48 kHz mono requested, Opus 96 kbps requested, ten-minute rotation.
  Effective browser settings are observations, not a promise of native fidelity.
- Local four-second signal test (RMS/clipping), no upload or model call. It is not
  trained speech VAD and does not prove silence or recognition accuracy.
- Hashed, sequenced chunks and an explicit final manifest. Inactivity alone does
  not prove a complete track. No final manifest means no processing approval.
- Capture approval is required before ASR. Cancelling preserves audio under TTL,
  blocks pending stages and stops contexts. A request/send already in flight may
  have incurred cost or delivered mail; cancellation cannot recall it.
- Audio survives ASR and is retained privately (proposed defaults: 30 days,
  10 GiB global cap). Durable ASR results recover without a second request after
  a database-write failure. An ambiguous dispatched request requires review.
- Owner-scoped search at /admin/captures, explicit client/project references,
  inclusive CDMX day filters, pagination, audio downloads and draft cancellation.
- /visualizations serves the approved simulation with protected assets and the
  same authentication middleware as /admin. This is not a live Odoo dashboard.
- Usage records distinguish attempts, uncertainty and provider counters. Costs
  remain NULL until measured/reconciled; counts of drafts are not costs.
- Evidence checks and post-ASR quality gates reject unsupported citations,
  insufficient captures and extreme repetition before issuing a summary.
- Window timestamps are labeled as windows in TXT and model input. Human
  identity is unverified; individual metrics are withheld for these captures.

## Not delivered / prerequisites for pilot

1. Live OpenAI smoke test with the production project's existing secret and
   <=2 minutes of consented or public audio. No real model call was performed
   during local development.
2. Test a real browser, chosen microphone, mute, device removal, reconnection,
   page close and shared-microphone recording. Node tests use browser mocks.
3. Validate local diarization resources/license and speaker-to-text alignment;
   it is not installed. The shared flag cannot manufacture speaker labels.
4. Confirm audio backup, off-host recovery and the final quota/retention policy.
   The existing daily backup script covers SQLite, not retained source audio.
5. Authenticate the exact Odoo CE instance and inspect its actual dashboard
   module. No Odoo module/record/menu has been created by this branch.
6. Register actual deployed version, model/prompt versions and seven-day start/end
   only after the release and smoke gates pass. Do not set the start to the date
   of source preparation.
7. Align the linked public privacy description with retained audio before rollout.
   The app's join and opt-in screens now display the configured audio retention;
   this source change does not publish an update to transgenia.org's legal page.

## Migration and release

The additive SQLite migration is version 6. Capture records gain approval and
cancellation, stream metadata/finalization, and usage/chunk ledgers. Old streams
have no final manifest or chunk ledger: they must not be processed automatically
or marked complete by fabricating either. Historical minutas remain accessible.

Before replacing the container, obtain a consistent verified SQLite backup using
the application's supported snapshot command. Never copy a live db file while
ignoring its WAL. The root diagnostic intentionally skipped WAL databases; that
is not evidence of corruption. Validate migration against a private copy first.

Expected source baseline: 472dbfe95bbdf7a0f489c075f56fce0a48f2bb2e. The supplied
root report observed runtime image ID
sha256:ec2bc930069d1084d8be8194ef8276861c82d549f8761103e123d0eb5b2257ff.
A tag is not proof of binary identity; check the current state immediately before
release and do not overwrite unexpected drift.

Set TRANSCRIBE_MODEL=gpt-transcribe and remove the inherited speech prompt;
Whisper is deliberately rejected by the new config. Preserve the current secret
file mounts, mail configuration, text retention and Access identity policy. Do
not put credentials in env files, repository, logs or shell arguments.

Cloudflare Access must cover /visualizations and /visualizations/* as well as
/admin paths. Test anonymous and authorized requests against direct origin and
the public hostname. Application JWT/session checks protect the route even if
the edge policy is missing, but the user still needs an Access login flow.

Build and test the release image with a unique version. This branch's builder
digest resolves to Go 1.26.9 in the local test environment. No builder upgrade is
needed based only on the older-looking go.mod minimum.

Release requires a short controlled replacement of the application container;
avoid an active capture. Check healthz locally, private search/visualization,
unauthorized access, capture-to-finalize, explicit approval, cancellation and
retention. No automatic email should be sent for this validation.

## Rollback

Do not simply point at the previous image with its old Whisper configuration:
the old worker does not enforce approval/cancel columns and could process pending
captures. First stop the new application, preserve its database/audio, and inspect
pending work. Restore a consistent pre-migration snapshot only with explicit
operator review of records created since the snapshot. Keep transcription
disabled until the restored worker state is proven safe. Restoring a database
does not undo provider calls or delivered emails. No destructive rollback script
is included.

## Evidence boundaries

The user supplied timer output for tgn-meetings-backup.timer: loaded, active,
waiting. The prior diagnostic queried the wrong transgenia-meetings-backup.timer
name. ExecMain timestamps were n/a; Result=success alone does not prove a usable
backup. Verify restoration separately.

WebMCP: no public tool is created, modified or removed. Administration, audio and
simulated visualizations remain authenticated, and no client meeting content is
published as a public tool.
