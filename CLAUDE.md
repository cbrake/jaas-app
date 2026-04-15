# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Jitsi as a Service (JaaS) web application for hosting and managing Jitsi meetings via the 8x8 JaaS platform (https://jaas.8x8.vc). Key features: meeting URL generation with custom slugs, no-login-required usage, meeting recording, and phone call-in support.

## Tech Stack

- **Go** — backend and server-rendered HTML pages (html/template)
- **SQLite** via `modernc.org/sqlite` (CGo-free)
- **JWT** via `golang-jwt/jwt/v5` (RS256 signing for JaaS)
- No frontend framework; pages are server-rendered

## Build & Run

- `go run .` — run in development
- `go build -o jaas-app .` — build binary
- `go test ./...` — run all tests

## Formatting

- Go: `gofmt` / `goimports` (standard Go formatting)
- Prettier (for non-Go files): tabs, no semicolons, always-parens for arrows, ES5 trailing commas, prose wrap always
