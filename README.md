# Castlet

> ⚠️ **Work in progress.** This is early and incomplete — expect rough edges,
> missing pieces, and breaking changes.

A minimal, self-hostable **podcast server** written in Go.

## What it's for

Castlet is meant for **closed environments** — a company intranet, a team, a
homelab — rather than the public internet. It is deliberately minimal and has
**no community features** (no comments, likes, follows, or discovery): just
channels, episodes (audio or video), per-channel RSS feeds, and click-to-seek
transcripts, behind a small admin area with local or OIDC sign-in.

It runs as a single binary with an embedded SQLite database and local-filesystem
media storage; every external dependency (Postgres, object storage, a hosted
transcriber) sits behind an interface you can swap.

## Test-run it

```sh
go build -o castlet ./cmd/castlet
./castlet user-create --email you@example.com --password 'choose-one' --name 'You'
CASTLET_SESSION_KEY="$(head -c32 /dev/urandom | base64)" \
  ./castlet serve --addr :8080 --base-url http://localhost:8080
```

Open <http://localhost:8080>, log in at `/login`, create a channel, and upload an
episode. Pages are addressed by opaque id: channels at `/c/{id}/` (feed at
`/c/{id}/feed.xml`) and episodes at `/e/{id}/`. Run `./castlet serve -h` for all
options.

For a self-contained demo with single sign-on and automatic whisper.cpp
transcription, see [`examples/standalone/`](examples/standalone/).

## Transcription

Transcripts are produced by **shelling out to an external tool per episode** —
the bundled setup runs [whisper.cpp](https://github.com/ggerganov/whisper.cpp).
This is the simple, dependency-free path, **not the fastest one possible**:
each upload spins the model up and transcribes the whole file, so a transcript
**does not appear instantly** — expect a delay (seconds to minutes, depending on
the model and episode length) before it shows up. Transcription runs in the
background, so the episode is usable meanwhile.

## License

[PolyForm Noncommercial License 1.0.0](LICENSE) — free to use, modify, and share
for **noncommercial** purposes.
