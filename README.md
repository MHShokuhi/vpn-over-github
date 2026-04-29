# VPN Over GitHub

> **For educational / authorized research use only.**
> Likely violates [GitHub's ToS](https://docs.github.com/en/site-policy/github-terms/github-terms-of-service). GitHub can read all gist/repo content. The default XOR cipher is not cryptographically secure.

A SOCKS5 tunnel that ships TCP packets through a private GitHub repo (`git`
transport) or private gists (`gist` transport). Useful where GitHub is
reachable but the rest of the internet isn't.

```
your app → SOCKS5 (client) → GitHub → server → internet
```

Two transports, picked per-token:

| Transport | Storage | PAT scope | Rate limit | Notes |
|---|---|---|---|---|
| **contents** (recommended) | files in a private repo via REST Contents API | `repo` (or fine-grained `Contents: write`) | 5000 REST/hr/token | One PUT per batch, ~250 ms RTT |
| **git** | files in a private repo via git push/pull | `repo` | no REST quota | ~800–1500 ms RTT (pack file overhead) |
| **gist** | content in a private gist via REST API | `gist` | 500 writes/hr/account | for very-low-traffic only |

## Does it actually work?

**Slow. Mostly a proof-of-concept.** Tunnel round-trip is one push + one
fetch:

- `contents` (default): ~**800 ms** — usable for SSH, light browsing, text
  chat. Telegram handshake completes inside its 10 s timeout.
- `git`: ~**1.5–2 s** — same workloads but visibly laggier.
- `gist`: rate-capped, only useful for very-low-rate traffic.

Every TCP stream your app opens shares that single pipe, so a browser with
6–8 parallel conns to one site gets a fraction each. Don't expect smooth
1080p video or video calls — your link to GitHub is the ceiling.

## Install

### Server (Linux, systemd)

```bash
sudo bash -c "$(curl -Ls https://raw.githubusercontent.com/sartoopjj/vpn-over-github/main/install.sh)"
```

The installer drops a binary at `/opt/gh-tunnel/gh-tunnel-server`, writes
`/opt/gh-tunnel/server-config.yaml`, and starts a systemd service.

### Client

Download the latest release binary from the GitHub releases page and run:

```bash
./gh-tunnel-client -config client_config.yaml          # TUI dashboard
./gh-tunnel-client --no-tui -config client_config.yaml # plain logs
```

Then point any SOCKS5-aware app at `127.0.0.1:1080`:

```bash
curl -x socks5h://127.0.0.1:1080 https://api.ipify.org
```

### Build from source

```bash
make build-all    # both binaries → ./build/
make test         # unit tests
make test-race    # with race detector
```

Requires Go 1.21+.

## Configure

Both client and server take a YAML file. Tokens are an array; each entry can
override `transport`, `repo`, `batch_interval`, `fetch_interval`, and
`upstream_connections`. Example:

```yaml
github:
  tokens:
    # Recommended: REST Contents API. ~250 ms per call.
    - token: "ghp_yourtoken"
      transport: "contents"
      repo: "yourname/your-private-tunnel-repo"
      batch_interval: 800ms
      fetch_interval: 800ms

    # git Smart HTTP (slower, no REST quota):
    # - token: "ghp_yourtoken"
    #   transport: "git"
    #   repo: "yourname/your-private-tunnel-repo"
    #   batch_interval: 1500ms
    #   fetch_interval: 1500ms

    # gist (capped at 500 writes/hr/account):
    # - token: "ghp_yourtoken"
    #   transport: "gist"
    #   batch_interval: 1500ms
    #   fetch_interval: 1500ms

  batch_interval: 800ms          # global defaults
  fetch_interval: 800ms
  api_timeout: 10s

socks:
  listen_addr: "127.0.0.1:1080"
  timeout: 30s
  buffer_size: 1048576           # 1 MiB — bigger Reads from the SOCKS app

encryption:
  algorithm: "xor"               # or "aes" (AES-256-GCM)
```

Server-side, `proxy.buffer_size` (default 2 MiB) controls per-Read size from
upstream destinations — bigger means larger frames, useful for video.

For the `git` transport, create a private repo with at least one commit
(e.g. an initial `README`). Use the same `repo` and the same encryption
algorithm on both sides.

`example_client_config.yaml` and `example_server_config.yaml` are starting
points.

## Uninstall

```bash
sudo bash uninstall.sh
```

## License

MIT — no warranty.
