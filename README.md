# relayport

[中文](#中文)

A small store-and-forward file relay for links that are slow, lossy, or both.

You drop files on a web page hosted on a well-connected VPS. A puller running at home
(a NAS, say) fetches them from the VPS over its own outbound connection, verifies them,
and tells the VPS to delete its copy. Uploads and pulls are resumable, split into chunks
and run in parallel, so a flaky cross-border link still makes steady progress.

```
browser ──HTTP(S)──▶ relay (VPS) ◀──HTTP(S)── puller (home / NAS)
 chunked, resumable    staging area          parallel Range requests, sha256 check,
 upload with progress                        then asks the relay to delete the file
```

- One static Go binary, no dependencies, one port, one Docker image (`scratch`-based).
- Web page: drag files or folders, per-file progress and speed, both hops visible
  (browser → relay, relay → home).
- Uploads: 8 MiB chunks, 4 in flight, each chunk checksummed and retried with backoff; closing the tab
  and dropping the same file again resumes where it stopped.
- Pulls: one file is split into parallel `Range` segments (helps a single huge file, unlike
  per-file parallelism), progress is saved so a restart resumes, and the whole file is verified
  against a SHA-256 before it is moved into place and the relay copy is deleted. An existing local
  file with the same name is never overwritten.
- Safety: one upload password and a separate pull token (failures rate limited per address),
  a staging quota, automatic cleanup of abandoned uploads, no path traversal, hidden names refused.
- The pull side only makes outbound connections, so the destination needs no port forwarding.
- `relayport push` uploads files or folders from a script or terminal, with the same resume as the page.

## Install

You need Docker (with Compose) on two machines:

- a **relay host** that both sides can reach, typically a VPS with a public IP. A domain is optional;
- the **destination**, e.g. a NAS. It only needs outbound internet access.

Images for amd64 and arm64: `ghcr.io/saki340/relayport` (`:0.2`, `:latest`, `:edge` for the main branch).

### 1. Relay host

```sh
mkdir -p relayport/data && cd relayport
curl -fsSLO https://raw.githubusercontent.com/Saki340/relayport/main/compose.relay.yaml
printf 'RELAYPORT_UPLOAD_PASSWORD=%s\nRELAYPORT_PULL_TOKEN=%s\nRELAYPORT_UID=%s\nRELAYPORT_GID=%s\n' \
  "$(openssl rand -base64 18)" "$(openssl rand -hex 32)" "$(id -u)" "$(id -g)" > .env
chmod 600 .env
```

Then choose how browsers reach it.

**A. IP address and port** (no domain needed):

```sh
echo 'RELAYPORT_PUBLISH=8080' >> .env
docker compose -f compose.relay.yaml up -d
```

Allow TCP 8080 in the VPS firewall and in the provider's security group / security list. The address is
`http://<server-ip>:8080`. This is plain HTTP: the password and the files cross the network unencrypted, and
browsers skip the per-chunk checksums (the final SHA-256 check on the pull side still catches corruption).

**B. Domain with HTTPS**: start it as is (it listens on `127.0.0.1:8080` only) and point a reverse proxy at
it. With Caddy, which gets the certificate by itself:

```sh
docker compose -f compose.relay.yaml up -d
```

```
relay.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

The address is `https://relay.example.com`.

Either way, `cat .env` shows the upload password (for the web page) and the pull token (for step 2).

### 2. Destination

```sh
mkdir relayport && cd relayport
curl -fsSLO https://raw.githubusercontent.com/Saki340/relayport/main/compose.puller.yaml
cat > .env <<EOF
RELAYPORT_SERVER=http://203.0.113.5:8080
RELAYPORT_PULL_TOKEN=change-me
RELAYPORT_INBOX=$HOME/inbox
RELAYPORT_UID=$(id -u)
RELAYPORT_GID=$(id -g)
EOF
chmod 600 .env
```

Edit `.env`: `RELAYPORT_SERVER` is the address from step 1, `RELAYPORT_PULL_TOKEN` the token from the relay's
`.env` (the puller refuses to start while it still says `change-me`), `RELAYPORT_INBOX` the folder to fill
(it must exist). Then:

```sh
docker compose -f compose.puller.yaml up -d
```

### 3. Check that it works

```sh
docker compose -f compose.relay.yaml logs     # on the relay
docker compose -f compose.puller.yaml logs    # on the destination
```

- The relay log starts with `relayport 0.2.x (...)` and `serving on :8080`.
- The puller log says `pulling from <your address> into /inbox`. If it says the relay rejected the token, the
  two `RELAYPORT_PULL_TOKEN` values differ. A connection error or timeout usually means the port is not open in
  a firewall, or `RELAYPORT_SERVER` is wrong.
- Open the address in a browser, sign in with the upload password and drop a small file. It shows
  `waiting for NAS`, then `to NAS: ...`, then disappears from the list; the puller logs `<name>: done in ...`
  and the file is in your `RELAYPORT_INBOX` folder.

### Updating

Run `docker compose -f compose.<relay|puller>.yaml pull` and then `up -d` with the same file. The first log line
shows the running version.

Older compose files ran the relay as root, so its `data/` belongs to root. When moving to the current
`compose.relay.yaml`, add `RELAYPORT_UID` / `RELAYPORT_GID` to `.env` and hand the folder over once:
`sudo chown -R "$(id -u):$(id -g)" data`. Until then the relay stops at startup with
`the data folder /data must be writable by uid ...`. To build the image yourself instead: `docker build -t relayport .` (add
`--build-arg VERSION=... --build-arg COMMIT=...` to stamp it) and change `image:` in the compose file.
Building needs BuildKit (`docker buildx`); Ubuntu's `docker.io` package does not include it, so install
`docker-buildx` as well, otherwise the build stops at `failed to parse platform`.

## Uploading from the command line

Download `relayport_<version>_<os>_<arch>` from the [releases](https://github.com/Saki340/relayport/releases)
page (Linux, macOS and Windows, amd64 and arm64; `SHA256SUMS` alongside), or use the Docker image.

```sh
export RELAYPORT_SERVER=http://203.0.113.5:8080 RELAYPORT_UPLOAD_PASSWORD=...
./relayport push ./photos ./notes.txt
docker run --rm -e RELAYPORT_SERVER -e RELAYPORT_UPLOAD_PASSWORD -v "$PWD:/w" -w /w \
  ghcr.io/saki340/relayport:0.2 push ./photos
```

```powershell
$env:RELAYPORT_SERVER = 'http://203.0.113.5:8080'; $env:RELAYPORT_UPLOAD_PASSWORD = '...'
.\relayport_0.2.0_windows_amd64.exe push .\photos
```

Folders keep their name, hidden files are skipped, `-to some/dir` puts everything under a folder on the relay.
If the push is interrupted, run the same command again: finished chunks are not sent twice.

## Configuration

Compose `.env` variables (used by the example compose files):

| Variable | File | Meaning |
|---|---|---|
| `RELAYPORT_PUBLISH` | relay | published port, default `127.0.0.1:8080`; `8080` = every interface |
| `RELAYPORT_SERVER` | puller | relay address, `http://<ip>:<port>` or `https://<domain>` |
| `RELAYPORT_INBOX` | puller | host folder that receives the files |
| `RELAYPORT_UID` / `RELAYPORT_GID` | both | user the container runs as (owner of `data/` and of pulled files), default `1000` |

`relayport serve`

| Variable | Default | Meaning |
|---|---|---|
| `RELAYPORT_LISTEN` | `:8080` | listen address |
| `RELAYPORT_DATA` | `./data` | staging directory (`inbox/` + `tmp/`) |
| `RELAYPORT_UPLOAD_PASSWORD` | required | password for the web page |
| `RELAYPORT_PULL_TOKEN` | required | bearer token for the puller |
| `RELAYPORT_QUOTA` | unlimited | max staged + in-flight bytes, e.g. `50GiB` |
| `RELAYPORT_STALE_TTL` | `48h` | drop unfinished uploads idle this long |
| `RELAYPORT_TRUST_PROXY` | off | `1` = take the client IP from `X-Forwarded-For`, only for connections from loopback or private addresses (your proxy) |
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

`relayport version` prints the version and commit. Every command refuses a secret that still starts with
`change-me`.

## Notes

- Five wrong passwords (or five wrong pull tokens) from one address within 10 minutes block that address for
  the rest of the window. Passwords and tokens are counted separately, so a misconfigured puller does not lock
  you out of the web page.
- Uploading the same name twice while it is still staged is refused (`409`); the name frees up once the puller
  has confirmed it.
- Checksums in the browser use `crypto.subtle`, which needs HTTPS (or localhost).
- Tests: `go test -race ./...`

MIT licensed.

---

## 中文

给慢速或高丢包链路用的小型"接力中转":文件先通过网页传到网络好的 VPS,再由家里(如 NAS)
**主动**从 VPS 拉取、校验、并让 VPS 删除副本。上传和拉取都支持断点续传、分块并行。

- 单个 Go 二进制、零依赖、一个端口、一个 Docker 镜像。
- 网页拖拽上传文件或文件夹,显示每个文件的进度和速度,以及两段进度(浏览器→中转机、中转机→家里)。
- 上传:8 MiB 分块,4 路并行,每块校验并带退避重试;关掉页面后再次拖入同一文件会接着传。
- 拉取:单个大文件也会切成多段并行 `Range` 下载;进度落盘,重启可续;整文件 SHA-256 校验通过才落盘并通知中转机删除;
  不会覆盖本地已有的同名文件。
- 安全:上传口令 + 独立的拉取令牌(按地址限制失败次数);暂存配额;自动清理废弃上传;拒绝路径穿越和隐藏文件名。
- 家里只需要出站连接,不用开放端口。
- `relayport push <文件或文件夹>` 可以从命令行或脚本上传,断了重跑同一条命令即可续传。

### 安装

两台机器都要有 Docker(含 Compose):一台双方都能访问的**中转机**(通常是有公网 IP 的 VPS,域名可有可无),
以及**目的端**(如 NAS,只需要能上网)。

**1. 中转机**

```sh
mkdir -p relayport/data && cd relayport
curl -fsSLO https://raw.githubusercontent.com/Saki340/relayport/main/compose.relay.yaml
printf 'RELAYPORT_UPLOAD_PASSWORD=%s\nRELAYPORT_PULL_TOKEN=%s\nRELAYPORT_UID=%s\nRELAYPORT_GID=%s\n' \
  "$(openssl rand -base64 18)" "$(openssl rand -hex 32)" "$(id -u)" "$(id -g)" > .env
chmod 600 .env
```

- **A. 用 IP + 端口**(不需要域名):`echo 'RELAYPORT_PUBLISH=8080' >> .env`,再
  `docker compose -f compose.relay.yaml up -d`。记得在 VPS 防火墙和云厂商的安全组里放行 TCP 8080。
  访问地址是 `http://<服务器IP>:8080`。这是明文 HTTP,口令和文件都不加密,浏览器端也不做分块校验
  (拉取端最终的 SHA-256 仍能发现损坏)。
- **B. 用域名 + HTTPS**:直接 `docker compose -f compose.relay.yaml up -d`(只监听 `127.0.0.1:8080`),
  再用反向代理指过去,Caddy 写 `relay.example.com { reverse_proxy 127.0.0.1:8080 }` 即可自动签证书。

`cat .env` 可以看到上传口令(网页登录用)和拉取令牌(第 2 步用)。

**2. 目的端**

```sh
mkdir relayport && cd relayport
curl -fsSLO https://raw.githubusercontent.com/Saki340/relayport/main/compose.puller.yaml
cat > .env <<EOF
RELAYPORT_SERVER=http://203.0.113.5:8080
RELAYPORT_PULL_TOKEN=change-me
RELAYPORT_INBOX=$HOME/inbox
RELAYPORT_UID=$(id -u)
RELAYPORT_GID=$(id -g)
EOF
chmod 600 .env
```

把 `.env` 里的 `RELAYPORT_SERVER` 改成第 1 步的地址,`RELAYPORT_PULL_TOKEN` 换成中转机 `.env` 里的令牌
(还是 `change-me` 时拒绝启动),`RELAYPORT_INBOX` 改成接收文件的目录(需已存在),然后
`docker compose -f compose.puller.yaml up -d`。

**3. 确认能用**:两边 `docker compose -f ... logs`。中转机第一行是版本号和 `serving on :8080`;拉取端显示
`pulling from ...`。提示令牌被拒绝就是两边令牌不一致;连接超时多半是防火墙没放行端口或地址写错。
然后在浏览器打开地址、登录、拖一个小文件,看到"waiting for NAS"→"to NAS"→消失,文件出现在接收目录即成功。

升级:`docker compose -f ... pull`,再 `up -d`。旧版 compose 里中转端以 root 运行,`data/` 归 root;换成现在的
`compose.relay.yaml` 时,在 `.env` 里加上 `RELAYPORT_UID` / `RELAYPORT_GID`,并执行一次
`sudo chown -R "$(id -u):$(id -g)" data`,否则中转端启动时会报 `the data folder /data must be writable by uid ...`。自己从源码构建镜像需要 BuildKit(`docker buildx`):Ubuntu 的
`docker.io` 包不带它,要另装 `docker-buildx`,否则会报 `failed to parse platform`。命令行上传可以从 [Releases](https://github.com/Saki340/relayport/releases)
下载对应系统的 `relayport` 可执行文件。配置项见上面的表格。
