# NOTICE

## Upstream project

This project adapts ideas and portions of code from
[lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp),
licensed under the MIT License, Copyright (c) 2025 Luke Harries.

Adapted areas:

- the message store schema (chats and messages tables),
- the whatsmeow event handling approach,
- the media download approach.

The upstream project is a Go bridge plus a Python MCP server. This project
is a single Go binary.

A reference copy of the upstream source is kept in
`reference/lharries-whatsapp-mcp/`, together with its `LICENSE`. That copy
is a separate Go module and is not built by this project.

## Third-party dependencies

The main direct dependencies and their licenses:

| Module | Purpose | License |
|---|---|---|
| `go.mau.fi/whatsmeow` | WhatsApp multi-device client | MPL-2.0 |
| `github.com/modelcontextprotocol/go-sdk` | MCP server SDK | MIT / Apache-2.0 (mixed during the upstream relicensing, see its `LICENSE`) |
| `modernc.org/sqlite` | Pure Go SQLite driver (no CGO) | BSD-3-Clause |
| `github.com/skip2/go-qrcode` | QR code rendering (PNG and terminal) | MIT |
| `golang.org/x/term` | Terminal detection | BSD-3-Clause |

Transitive dependencies keep their own licenses. Each module ships its
own `LICENSE` file in the Go module cache.
