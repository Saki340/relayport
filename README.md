# inbox-relay

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

## Quick start

Build the image once (or `go build` for a plain binary):

```sh
docker build -t inbox-relay .
```

**On the relay host** (`compose.relay.yaml`): set a password and a pull token, then
`docker compose -f compose.relay.yaml up -d`. Put a TLS reverse proxy in front, e.g. Caddy:

```
relay.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

**At the destination** (`compose.puller.yaml`): set `IR_SERVER` and the same pull token, mount the
folder to fill, then `docker compose -f compose.puller.yaml up -d`.

Open `https://relay.example.com`, sign in, and drop files.

## Configuration

`inbox-relay serve`

| Variable | Default | Meaning |
|---|---|---|
| `IR_LISTEN` | `:8080` | listen address |
| `IR_DATA` | `./data` | staging directory (`inbox/` + `tmp/`) |
| `IR_UPLOAD_PASSWORD` | required | password for the web page |
| `IR_PULL_TOKEN` | required | bearer token for the puller |
| `IR_QUOTA` | unlimited | max staged + in-flight bytes, e.g. `50GiB` |
| `IR_STALE_TTL` | `48h` | drop unfinished uploads idle this long |
| `IR_TRUST_PROXY` | off | `1` = take client IP from `X-Forwarded-For` (only behind your proxy) |
| `IR_SECRET` | derived from password | cookie signing key |

`inbox-relay pull`

| Variable | Default | Meaning |
|---|---|---|
| `IR_SERVER` | required | relay base URL |
| `IR_PULL_TOKEN` | required | same token as the relay |
| `IR_DEST` | `./inbox` | where finished files go (partial files are dot-prefixed) |
| `IR_PARALLEL` | `4` | concurrent segment downloads |
| `IR_FILES` | `2` | files in flight at once |
| `IR_SEGMENT` | `16MiB` | segment size |
| `IR_POLL` | `15s` | how often to look for new files |

## Notes

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

使用:`docker build -t inbox-relay .`,中转机用 `compose.relay.yaml`(前面加 Caddy/nginx 做 HTTPS),
家里用 `compose.puller.yaml`,配置项见上表。
