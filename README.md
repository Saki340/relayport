# relayport

[中文](#中文)

A small store-and-forward file relay for links that are slow, lossy, or both.

You drop files on a web page hosted on a well-connected VPS. A puller running at home
(a NAS, say) fetches them from the VPS over its own outbound connection, verifies them,
and tells the VPS to delete its copy. Uploads and pulls are resumable, split into chunks
and run in parallel, so a flaky cross-border link still makes steady progress.

```
browser ──HTTPS──▶ relay (VPS) ◀──HTTPS── puller (home / NAS)
 chunked, resumable   staging area         parallel Range requests, sha256 check,
 upload with progress                      then asks the relay to delete the file
```

- One static Go binary, no dependencies, one port, one Docker image (`scratch`-based).
- Web page: drag files or folders, per-file progress and speed, both hops visible
  (browser → relay, relay → home).
- Uploads: 8 MiB chunks, 4 in flight, each chunk checksummed and retried with backoff; closing the tab
  and dropping the same file again resumes where it stopped.
- Pulls: one file is split into parallel `Range` segments (helps a single huge file, unlike
  per-file parallelism), progress is saved so a restart resumes, and the whole file is verified
  against a SHA-256 before it is moved into place and the relay copy is deleted.
- Safety: one upload password (failed logins are rate limited), a separate pull token,
  a staging quota, automatic cleanup of abandoned uploads, no path traversal, hidden names refused.
- The pull side only makes outbound connections, so the destination needs no port forwarding.
- `relayport push` uploads files or folders from a script or terminal, with the same resume as the page.

## Quick start

Images for amd64 and arm64 are published as `ghcr.io/saki340/relayport` (`:0.1`, `:latest`, and `:edge`
for the main branch).

**On the relay host**: download `compose.relay.yaml`, create the secrets next to it, start it.

```sh
printf 'RELAYPORT_UPLOAD_PASSWORD=%s\nRELAYPORT_PULL_TOKEN=%s\n' "pick-a-password" "$(openssl rand -hex 32)" > .env
chmod 600 .env
docker compose -f compose.relay.yaml up -d
```

Put a TLS reverse proxy in front, e.g. Caddy:

```
relay.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

**At the destination**: download `compose.puller.yaml`, set `RELAYPORT_SERVER`, your `user:` uid:gid and the
folder to fill, put the same token in a `.env` (`RELAYPORT_PULL_TOKEN=...`), then
`docker compose -f compose.puller.yaml up -d`.

Open `https://relay.example.com`, sign in, and drop files.

**Updating** either side: `docker compose pull && docker compose up -d`. `docker compose logs` shows the
running version on the first line. To build the image yourself instead: `docker build -t relayport .`
(add `--build-arg VERSION=... --build-arg COMMIT=...` to stamp it) and change `image:` accordingly.

### Uploading from the command line

```sh
export RELAYPORT_SERVER=https://relay.example.com RELAYPORT_UPLOAD_PASSWORD=...
relayport push ./photos ./notes.txt            # or with Docker:
docker run --rm -e RELAYPORT_SERVER -e RELAYPORT_UPLOAD_PASSWORD -v "$PWD:/w" -w /w \
  ghcr.io/saki340/relayport:0.1 push ./photos
```

Folders keep their name, hidden files are skipped, `-to some/dir` puts everything under a folder on the relay.
If the push is interrupted, run the same command again: finished chunks are not sent twice.

## Configuration

`relayport serve`

| Variable | Default | Meaning |
|---|---|---|
| `RELAYPORT_LISTEN` | `:8080` | listen address |
| `RELAYPORT_DATA` | `./data` | staging directory (`inbox/` + `tmp/`) |
| `RELAYPORT_UPLOAD_PASSWORD` | required | password for the web page |
| `RELAYPORT_PULL_TOKEN` | required | bearer token for the puller |
| `RELAYPORT_QUOTA` | unlimited | max staged + in-flight bytes, e.g. `50GiB` |
| `RELAYPORT_STALE_TTL` | `48h` | drop unfinished uploads idle this long |
| `RELAYPORT_TRUST_PROXY` | off | `1` = take client IP from `X-Forwarded-For` (only behind your proxy) |
| `RELAYPORT_SECRET` | derived from password | cookie signing key |

`relayport pull`

| Variable | Default | Meaning |
|---|---|---|
| `RELAYPORT_SERVER` | required | relay base URL |
| `RELAYPORT_PULL_TOKEN` | required | same token as the relay |
| `RELAYPORT_DEST` | `./inbox` | where finished files go (partial files are dot-prefixed) |
| `RELAYPORT_PARALLEL` | `4` | concurrent segment downloads |
| `RELAYPORT_FILES` | `2` | files in flight at once |
| `RELAYPORT_SEGMENT` | `16MiB` | segment size |
| `RELAYPORT_POLL` | `15s` | how often to look for new files |

`relayport push [-to dir] path...`

| Variable | Default | Meaning |
|---|---|---|
| `RELAYPORT_SERVER` | required | relay base URL |
| `RELAYPORT_UPLOAD_PASSWORD` | required | the web page password |
| `RELAYPORT_PARALLEL` | `4` | chunks in flight per file |

`relayport version` prints the version and commit. The relay and the puller refuse to start while a secret
still starts with `change-me` (the old example placeholder).

## Notes

- Run the puller container as your own uid:gid (`user:` in `compose.puller.yaml`); as root,
  pulled files end up owned by root in the destination folder.
- Plain HTTP works but sends the password and files in clear text; use TLS on the public side.
- Uploading the same name twice while it is still staged is refused (`409`); the name frees up once the puller has confirmed it.
- Checksums in the browser use `crypto.subtle`, which needs HTTPS (or localhost); without it chunk
  checksums are skipped and the final whole-file SHA-256 on the pull side still catches corruption.
- Tests: `go test -race ./...`

MIT licensed.

---

## 中文

给慢速或高丢包链路用的小型"接力中转":文件先通过网页传到网络好的 VPS,再由家里(如 NAS)
**主动**从 VPS 拉取、校验、并让 VPS 删除副本。上传和拉取都支持断点续传、分块并行。

- 单个 Go 二进制、零依赖、一个端口、一个 Docker 镜像。
- 网页拖拽上传文件或文件夹,显示每个文件的进度和速度,以及两段进度(浏览器→中转机、中转机→家里)。
- 上传:8 MiB 分块,4 路并行,每块校验并带退避重试;关掉页面后再次拖入同一文件会接着传。
- 拉取:单个大文件也会切成多段并行 `Range` 下载;进度落盘,重启可续;整文件 SHA-256 校验通过才落盘并通知中转机删除。
- 安全:上传口令(登录失败限速)+ 独立的拉取令牌;暂存配额;自动清理废弃上传;拒绝路径穿越和隐藏文件名。
- 家里只需要出站连接,不用开放端口。
- `relayport push <文件或文件夹>` 可以从命令行或脚本上传,断了重跑同一条命令即可续传。

使用:镜像已发布为 `ghcr.io/saki340/relayport`(amd64/arm64)。中转机用 `compose.relay.yaml`,在同目录建
`.env` 写上 `RELAYPORT_UPLOAD_PASSWORD` 和 `RELAYPORT_PULL_TOKEN`(可用 `openssl rand -hex 32` 生成),前面加
Caddy/nginx 做 HTTPS;家里用 `compose.puller.yaml`,`.env` 里放同一个令牌。升级只需
`docker compose pull && docker compose up -d`。配置项见上表。
