# relayport

Store-and-forward file relay for slow or lossy links. A browser uploads to a relay host (VPS); a puller at
the destination (NAS) fetches the files over its own outbound connection, verifies them, and tells the
relay to delete its copy. Single Go binary, standard library only, one Docker image (`scratch`).

Private details of the author's own deployment live in `CLAUDE.local.md` (gitignored). Read it too if present.

## Layout

| File | Role |
|---|---|
| `main.go` | CLI entry: `serve` / `pull` / `push` / `version`, `envOr`, `requireSecret` (refuses `change-me*`), build-stamped `version`/`commit` |
| `server.go` | HTTP routes, auth (password cookie + pull bearer token), login rate limit, `parseSize` |
| `store.go` | staging area on disk: uploads (`tmp/<id>.part` + `.json` bitmap), staged files (`inbox/`), quota, sweep |
| `pull.go` | puller: list, per-file state, parallel Range segments, sha256 verify, no-clobber placement, confirm/delete |
| `push.go` | CLI uploader over the browser chunk API; same upload key as the page (name, size, mtime in ms) so both resume each other |
| `web/index.html` | the whole UI (embedded with `go:embed`): login, drag and drop, chunk upload, progress |
| `relay_test.go` | end to end tests (httptest server + real puller) |

## API (all JSON unless noted)

Browser (cookie `relayport_session`, set by `POST /api/login {"Password":...}`):
`GET /api/state`, `POST /api/uploads {Key,Name,Size}` (idempotent per Key, returns `id`, `chunkSize`, `done[]`),
`PUT /api/uploads/{id}/chunks/{n}` (raw bytes, optional `X-Chunk-Sha256`), `POST /api/uploads/{id}/complete`.

Puller (`Authorization: Bearer <RELAYPORT_PULL_TOKEN>`): `GET /pull/list`, `GET /pull/file?name=` (Range),
`POST /pull/progress {Name,Bytes}`, `POST /pull/done {Name,SHA256}` (relay deletes only if the hash matches).

## Invariants worth keeping

- Standard library only. Do not add dependencies without a strong reason (the tiny image is a feature).
- A chunk is marked done only after all its bytes arrived and its checksum matched; the final file is hashed
  on the relay, and the puller must prove the same hash before the relay deletes anything.
- The puller never exposes a half-written file under its final name: partials are `.<name>.part` plus
  `.<name>.state`. Keep hidden names reserved: `CleanName` rejects any path element starting with `.`.
- The pull side makes only outbound connections.
- Run the puller container as the destination user (`user:` in compose), or files end up owned by root.
- `X-Forwarded-For` is believed only from loopback/private peers and only its last entry, so an exposed port
  cannot be used to spoof the rate-limit key. Login and pull-token failures are counted under separate keys.

## Build and test

Go is NOT installed on the Windows host. Follow the global rule: build and test through WSL.

```
MSYS_NO_PATHCONV=1 pwsh.exe -NoProfile -File 'D:\WSL\Scripts\linux-task.ps1' -Mode build -Project 'D:\Project\relayport' -Command 'M=~/.local/bin/mise; $M exec -- gofmt -l .; $M exec -- go vet ./... && $M exec -- go test -race -count=1 ./...'
```

From Claude Code's Git Bash `pwsh.exe` may not be on PATH; calling the script from the PowerShell tool
(`& 'D:\WSL\Scripts\linux-task.ps1' -Mode build ...`) works.

`mise.toml` pins Go 1.24 (installed on first use). `gofmt -w` in the mirror changes only the mirror: copy the
formatted files back to the Windows source deliberately, never sync the whole mirror. Docker builds work the
same way (`docker build .` in build mode); Docker Desktop must be running.

CI (`.github/workflows/ci.yml`) runs gofmt, vet, `go test -race` and a multi-arch image build. `release.yml`
publishes `ghcr.io/<owner>/relayport` (`:edge` from main, `:X.Y.Z` / `:X.Y` / `:latest` from `vX.Y.Z` tags),
stamping `VERSION`/`COMMIT` build args into the binary; a `v*` tag also creates a GitHub release with standalone
binaries (linux/darwin/windows x amd64/arm64) and `SHA256SUMS`. The example compose files point at
`ghcr.io/saki340/relayport:0.1` and take everything installation-specific from a `.env` next to them
(`RELAYPORT_PUBLISH` on the relay; `RELAYPORT_SERVER`, `RELAYPORT_INBOX`, `RELAYPORT_UID/GID` on the puller), so the
README install is copy-paste. Both IP:port (plain HTTP) and domain + reverse proxy setups are supported.

## Known gaps / ideas

- A chunk is buffered in memory (8 MiB x 4 per upload); fine for small VPSs but worth streaming to disk.
- Browser page was only tested in Chrome with a 9 MB file (the automation upload limit is 10 MB). Large
  files, many files, folder drops and the retry/resume path in the page itself are untested in a browser.
- No cancel or delete button for staged or in-flight uploads; no notifications (ntfy/webhook idea);
  no bandwidth limit; segment/parallelism is static (adaptive parallelism would help on lossy links).
- `scanInbox` rehashes every staged file on restart (slow for many GB).
- Login failure map is in memory and never pruned for idle IPs.
- Real cross-border throughput versus the old rsync relay has not been measured.
