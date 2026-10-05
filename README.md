# 🌐 local-dyn-dns

One-shot Linux CLI that reads eligible hosts from PostgreSQL and synchronizes `A`/`AAAA` records in an
existing [Technitium DNS Server](https://technitium.com/dns/) zone through its HTTP API. PostgreSQL is
only read. Ships as a single static binary.

## ✨ Features

- 🔄 **Sync**: creates, updates, or leaves each record unchanged, then confirms the result via the API.
- 🧭 **Record type**: IPv4 selects `A`, IPv6 selects `AAAA`; the other family is preserved.
- 🥇 **Priority**: repeated exact addresses compete by numeric priority; the highest wins.
- ⚖️ **Ambiguity detection**: ties with different IPs and conflicting case or trailing-dot variants are
  skipped and fail the run.
- 🛑 **Never deletes**: inactive or missing rows leave DNS untouched; CNAME conflicts and names outside
  the zone are skipped as errors.
- ⚡ **Concurrent**: bounded parallelism with a hard deadline per name; failures are isolated.
- 🔁 **Failover**: up to three backup databases, tried in order when the primary is unreachable.
- 🔒 **Secrets**: token sent as bearer header only; passwords and token are redacted from output.
- 📝 **First run**: creates a user-only config template.
- 🌈 **Output**: colored with emojis; plain when redirected or `NO_COLOR` is set.

## 📦 Install

Building needs Go 1.26+. Running needs PostgreSQL access and Technitium DNS Server 15+.

```bash
git clone https://github.com/tf4482/local-dyn-dns.git
cd local-dyn-dns
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/local-dyn-dns .
sudo install -m 755 dist/local-dyn-dns /usr/local/bin/local-dyn-dns
```

## 🗄️ Database

`public.hosts` needs these columns; the role needs only `CONNECT`, schema `USAGE`, and table `SELECT`.

| Column | Meaning |
| --- | --- |
| `status` | Boolean or text `true`/`false`; `NULL` and false rows are inactive |
| `ip` | IPv4 or IPv6 target |
| `address` | Concrete DNS host name; URLs, paths, and wildcards are rejected |
| `priority` | Numeric precedence for repeated exact addresses; ignored for unique ones |

Invalid rows are reported and skipped without failing the run.

## ⚙️ Configuration

The first existing file is used, without merging:

1. `config.yml` beside the executable
2. `~/.config/local-dyn-dns/config.yml`

If neither exists, the second one is created with dummy values and the run exits `1`. Copy
[`config.yml.example`](config.yml.example) to `config.yml` beside the executable, or use the
automatically created template. The same example is also available as
[`config.example.yml`](config.example.yml).

| Setting | Description |
| --- | --- |
| `database.host`, `port`, `name`, `user`, `password` | Primary PostgreSQL target |
| `database.backups` | Optional list of up to three failover targets with the same keys |
| `technitium.base_url` | HTTP(S) base URL; HTTPS certificates are verified |
| `technitium.api_token` | Token with **View** and **Modify** permission on the zone |
| `technitium.zone` | Existing zone; it is never created or deleted |
| `technitium.ttl_seconds` | TTL of the synchronized records |
| `settings.dns_concurrency` | Names synchronized in parallel |
| `settings.dns_timeout_seconds` | Hard deadline for all requests of one name |

## 🚀 Usage

```bash
local-dyn-dns
local-dyn-dns --help   # also: -h, help
```

| Exit | Meaning |
| --- | --- |
| `0` | Every eligible name matched or synchronized, including empty or inactive-only tables |
| `1` | Configuration, ambiguity, database, API, conflict, or timeout error |
| `130` | Interrupted by the user |

A timeout after a write began reports that the final DNS state is uncertain.

## ⏱️ Deploy

Install the binary, place the configuration, then schedule it with the example systemd units:

```bash
sudo install -m 644 local-dyn-dns.example.service /etc/systemd/system/local-dyn-dns.service
sudo install -m 644 local-dyn-dns.example.timer /etc/systemd/system/local-dyn-dns.timer
sudo systemctl daemon-reload
sudo systemctl enable --now local-dyn-dns.timer
journalctl -u local-dyn-dns.service
```

Adjust `User=` in the service; that account needs `~/.config/local-dyn-dns/config.yml`.

## 🧪 Develop

Tests need no live services and never change DNS:

```bash
go vet ./...
go test ./...
```

## 📜 License

MIT, see [LICENSE](LICENSE).
