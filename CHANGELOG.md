# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
