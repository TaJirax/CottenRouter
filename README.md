<div align="center">

# CottenRouter

### یک درگاه عمومی، چند تونل DNS — سریع، امن و بدون تغییر Payload

**One public gateway for multiple DNS tunnels — fast, safe, and payload-transparent.**

[![Release](https://img.shields.io/github/v/release/TaJirax/CottenRouter?style=flat-square&logo=github&label=release)](https://github.com/TaJirax/CottenRouter/releases/latest)
[![CI](https://img.shields.io/github/actions/workflow/status/TaJirax/CottenRouter/ci.yml?branch=main&style=flat-square&logo=githubactions&label=tests)](https://github.com/TaJirax/CottenRouter/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/TaJirax/CottenRouter?style=flat-square&logo=go)](go.mod)
[![License](https://img.shields.io/github/license/TaJirax/CottenRouter?style=flat-square)](LICENSE)
[![Container](https://img.shields.io/badge/GHCR-multi--arch-blue?style=flat-square&logo=docker)](https://github.com/TaJirax/CottenRouter/pkgs/container/cottenrouter)

[نصب سریع](#-نصب-سریع) · [راهنمای استفاده](#-راهنمای-استفاده) · [دستورات](#-مرجع-دستورات) · [Docker](#-اجرای-docker) · [حذف](#-حذف-cottenrouter) · [English](#english-guide) · [پروژه‌ها و قدردانی](#-پروژهها-و-قدردانی)

</div>

---

## CottenRouter چیست؟

**CottenRouter** یک مسیریاب سبک در جلوی سرویس‌های DNS Tunnel است. تمام سرویس‌ها می‌توانند روی یک سرور و یک IP اجرا شوند؛ CottenRouter پورت عمومی `53` را دریافت می‌کند، نام دامنه اولین DNS Question را می‌خواند و درخواست را به پورت خصوصی بک‌اند درست می‌فرستد.

برای ترافیک TLS نیز می‌تواند با استفاده از **SNI**، پورت‌های عمومی DoT و HTTPS را میان سرویس‌های مختلف به اشتراک بگذارد. جریان رمزنگاری‌شده بدون TLS termination و بدون دست‌کاری به بک‌اند منتقل می‌شود.

> CottenRouter هیچ DNS label یا بایت پروتکلی اضافه نمی‌کند؛ بنابراین MTU تونل را کاهش نمی‌دهد. فقط شناسه ۱۶ بیتی DNS هنگام پردازش موقتاً نگاشت و در پاسخ بازیابی می‌شود.

### قابلیت‌های اصلی

- اجرای هم‌زمان چند DNS Tunnel روی یک IP و پورت عمومی `53`
- مسیریابی بر اساس طولانی‌ترین suffix دامنه؛ مناسب برای دامنه‌های تو‌در‌تو
- پشتیبانی از UDP، DNS-over-TCP، DoT، DoH، NaiveProxy و StunTLS passthrough
- پنل TUI برای نصب، تنظیم، مانیتورینگ و مدیریت پروژه‌ها
- نصب transactional با snapshot، health check و rollback خودکار
- محدودیت نرخ، صف ثابت، timeout، سقف اتصال و کنترل مصرف منابع
- API مدیریتی فقط روی loopback و داشبورد با refresh دو ثانیه‌ای
- Docker بدون root، با filesystem فقط‌خواندنی و بدون shell داخلی
- باینری‌های رسمی `linux/amd64` و `linux/arm64` با checksum

---

## پیش‌نیازها

### نصب مستقیم روی سرور

- Linux دارای `systemd 245+`
- دسترسی `root` یا `sudo`
- معماری `amd64` یا `arm64` برای نصب باینری آماده
- یک IP عمومی و حداقل یک دامنه/زیردامنه اختصاصی برای هر بک‌اند
- توزیع‌های پیشنهادی: Ubuntu 20.04+، Debian 11+، RHEL/Rocky/AlmaLinux 9+

> WSL فقط برای توسعه و آزمایش مناسب است. برای سرویس واقعی از یک سرور Linux بومی استفاده کنید.

### انتخاب روش اجرا

| ویژگی | نصب روی سرور | Docker |
|---|---:|---:|
| هسته مسیریابی | ✅ | ✅ |
| نصب و مدیریت خودکار بک‌اندها | ✅ | — |
| پنل TUI | ✅ | — |
| سرویس systemd و firewall integration | ✅ | — |
| اجرای بدون root | — | ✅ |
| مناسب برای | سرور تازه و مدیریت کامل | زیرساخت کانتینری موجود |

---

## 🚀 نصب سریع

### روش پیشنهادی: آخرین نسخه پایدار

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh | sudo bash
sudo cottenrouter tui
```

نصاب، آخرین [نسخه رسمی](https://github.com/TaJirax/CottenRouter/releases/latest) را دریافت و checksum آن را بررسی می‌کند. اجرای دوباره همین دستور، برنامه را به‌صورت امن ارتقا می‌دهد.

### نصب نسخه مشخص

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh \
  | sudo bash -s -- --version=v1.2.8
```

### نصب نسخه توسعه

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh \
  | sudo bash -s -- --channel=edge
```

### build از سورس هنگام نصب

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh \
  | sudo bash -s -- --build-from-source
```

### گزینه‌های نصب

| گزینه | کاربرد |
|---|---|
| `--version=vX.Y.Z` | نصب یک نسخه رسمی مشخص |
| `--channel=stable` | نصب آخرین release؛ حالت پیش‌فرض |
| `--channel=edge` | build آخرین commit شاخه اصلی؛ مناسب تست |
| `--build-from-source` | کامپایل محلی به‌جای باینری release |
| `--no-swap` | غیرفعال کردن ساخت swap محافظتی |

> نصاب قبل از اعمال تغییرات، ابزارها، نسخه systemd، فضای دیسک و پورت‌ها را بررسی می‌کند. در صورت شکست، تنظیمات قبلی بازیابی می‌شوند.

---

## 🧭 راهنمای استفاده

### ۱. اجرای پنل مدیریت

```bash
sudo cottenrouter tui
```

کلیدهای مهم Project Manager:

| کلید | عملیات |
|---|---|
| `Space` | انتخاب یا لغو انتخاب پروژه |
| `i` | نصب هدایت‌شده پروژه‌های انتخاب‌شده |
| `Enter` / `e` | ویرایش دامنه، پورت خصوصی، TCP، DoT و DoH |
| `a` | باز کردن تنظیمات پیشرفته و native پروژه |
| `s` | restart پروژه |
| `u` | جدا کردن پروژه بدون حذف فایل‌ها |
| `x` | حذف کامل پروژه با تأیید صریح |
| `v` | نمایش مسیر credentialها و اطلاعات عمومی |
| `V` | نمایش secretها پس از تأیید |

### ۲. اتصال دامنه‌ها

برای هر سرویس یک دامنه یا زیردامنه یکتا تعریف کنید و همه آن‌ها را به IP سرور delegate کنید:

```text
cotten.example.com  → CottenDNS
master.example.com  → MasterDnsVPN
storm.example.com   → StormDNS
feed.example.com    → thefeed
```

دو سرویس نمی‌توانند دقیقاً یک suffix یکسان داشته باشند. suffixهای تو‌در‌تو مجاز هستند و طولانی‌ترین تطبیق برنده می‌شود.

### ۳. بررسی وضعیت

```bash
sudo systemctl status cottenrouter
sudo journalctl -u cottenrouter -f
sudo cottenrouter healthz -config /etc/cottenrouter/config.json
curl -fsS http://127.0.0.1:9088/v1/status
```

### ۴. اعتبارسنجی و اجرای دستی config

```bash
cottenrouter check -config cottenrouter.json
cottenrouter serve -config cottenrouter.json
```

نمونه کامل در [`cottenrouter.example.json`](cottenrouter.example.json) و نمونه SlipGate در [`cottenrouter.slipgate.example.json`](cottenrouter.slipgate.example.json) قرار دارد. فیلدهای ناشناخته در config رد می‌شوند.

### ۵. وارد کردن تنظیمات SlipGate

```bash
cottenrouter slipgate-import \
  --input /etc/slipgate/config.json \
  --output cottenrouter.json
```

### ۶. مشاهده catalog پروژه‌ها

```bash
cottenrouter catalog
cottenrouter catalog --offline
```

حالت عادی، شاخه پیش‌فرض و installer فعلی هر پروژه را از GitHub بررسی می‌کند. حالت `--offline` فقط metadata جایگزین داخلی را نشان می‌دهد و برای نصب توصیه نمی‌شود.

---

## 📚 مرجع دستورات

```text
cottenrouter tui               پنل نصب، مدیریت و مانیتورینگ
cottenrouter serve             اجرای router با config مشخص
cottenrouter check             اعتبارسنجی config بدون اجرا
cottenrouter healthz           بررسی سلامت سرویس
cottenrouter install           نصب یک backend از CLI
cottenrouter configure         ویرایش تنظیمات پایدار backend
cottenrouter advanced          تنظیمات پیشرفته backend
cottenrouter service           start / stop / restart یک پروژه
cottenrouter remove            جدا یا purge کردن پروژه
cottenrouter uninstall         alias عملیات حذف پروژه
cottenrouter keys              نمایش credentialهای پروژه
cottenrouter catalog           نمایش پروژه‌ها و installerهای فعلی
cottenrouter slipgate-import   تبدیل config موجود SlipGate
cottenrouter version           نمایش نسخه
```

برای مشاهده optionهای هر دستور:

```bash
cottenrouter <command> --help
```

### مدیریت مستقیم سرویس

```bash
sudo cottenrouter service --project=cottendns --action=restart
sudo cottenrouter service --project=stormdns --action=stop
sudo cottenrouter service --project=stormdns --action=start
```

### حذف یک backend از Router

حفظ فایل‌ها و داده‌های پروژه:

```bash
sudo cottenrouter remove --project=cottendns
```

حذف دائمی داده مدیریت‌شده همان پروژه:

```bash
sudo cottenrouter remove --project=cottendns --purge --confirm=cottendns
```

---

## 🐳 اجرای Docker

کانتینر فقط هسته routing را اجرا می‌کند؛ نصب و مدیریت backendها در Docker بر عهده شماست.

```bash
curl -fsSLO https://raw.githubusercontent.com/TaJirax/CottenRouter/main/docker-compose.yml
curl -fsSLO https://raw.githubusercontent.com/TaJirax/CottenRouter/main/cottenrouter.docker.json

# دامنه و backend نمونه را در cottenrouter.docker.json تغییر دهید
docker compose up -d
```

بررسی سلامت:

```bash
docker compose exec cottenrouter \
  /usr/local/bin/cottenrouter healthz \
  -config /etc/cottenrouter/config.json
```

به‌روزرسانی:

```bash
docker compose pull
docker compose up -d
```

حذف کانتینر، با حفظ فایل config محلی:

```bash
docker compose down
```

Image رسمی: [`ghcr.io/tajirax/cottenrouter`](https://github.com/TaJirax/CottenRouter/pkgs/container/cottenrouter) برای `linux/amd64` و `linux/arm64`.

کانتینر داخل خود روی پورت غیر privileged یعنی `5353` گوش می‌دهد و Docker پورت `53` میزبان را publish می‌کند. Image با UID `65532`، قابلیت‌های حذف‌شده، root filesystem فقط‌خواندنی و `no-new-privileges` اجرا می‌شود. جزئیات بیشتر: [راهنمای Docker](docs/docker.md).

---

## 🔄 به‌روزرسانی

### نصب روی سرور

همان دستور نصب پایدار را دوباره اجرا کنید:

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh | sudo bash
```

نصاب release جدید را دریافت می‌کند، config را نگه می‌دارد و در صورت خطا rollback می‌کند.

### بررسی نسخه

```bash
cottenrouter version
```

---

## 🗑️ حذف CottenRouter

### حذف امن و حفظ داده‌ها

```bash
sudo cottenrouter-uninstall
```

این دستور binary، سرویس، resource slice و drop-inهای متعلق به CottenRouter را حذف می‌کند؛ اما config، backendها، پنل‌ها، ruleهای firewall قبلی و swap را نگه می‌دارد.

### حذف config برنامه

```bash
sudo cottenrouter-uninstall --purge --confirm CottenRouter
```

### حذف برنامه، تمام backendهای مدیریت‌شده و داده‌های آن‌ها

```bash
sudo cottenrouter-uninstall \
  --purge \
  --purge-backends \
  --confirm CottenRouter
```

### حذف swap ساخته‌شده توسط نصاب

```bash
sudo cottenrouter-uninstall --remove-swap --confirm CottenRouter
```

> گزینه‌های purge برگشت‌پذیر نیستند. فقط firewall rule و swap دارای ownership marker متعلق به CottenRouter حذف می‌شوند؛ تنظیمات از پیش موجود دست‌کاری نمی‌شوند.

---

## ⚙️ نحوه مسیریابی

1. اولین DNS Question از packet خوانده می‌شود؛ packet خراب drop می‌شود.
2. طولانی‌ترین suffix تنظیم‌شده انتخاب می‌شود.
3. transaction ID با یک شناسه یکتای متصل به socket بک‌اند جایگزین می‌شود.
4. packet بدون تغییر دیگر به پورت خصوصی backend ارسال می‌شود.
5. پاسخ فقط در صورت تطبیق دقیق نام، type و class سؤال پذیرفته می‌شود.
6. ID اصلی client بازیابی و پاسخ ارسال می‌شود.

شناسه‌ها در یک socket generation دوباره استفاده نمی‌شوند. پس از مصرف ۶۵٬۵۳۶ شناسه، source port چرخش می‌کند تا پاسخ دیررس وارد نسل جدید نشود.

### امنیت و محدودیت‌ها

- backend راه‌دور به‌صورت پیش‌فرض رد می‌شود؛ در Docker این رفتار با شبکه خصوصی compose کنترل می‌شود.
- `max_packet_size` و `max_tcp_message_size` حداکثر `16 KiB` هستند.
- API مدیریتی باید روی loopback باقی بماند.
- installerهای backend به‌صورت root اجرا می‌شوند؛ قبل از اجرای release ناشناخته از سرور snapshot بگیرید.
- تطبیق ID و Question، احراز هویت cryptographic نیست؛ backend مورد اعتماد فرض می‌شود.
- تست کامل lifecycle روی تمام توزیع‌ها و پنل‌های سرور هنوز در CI شبیه‌سازی نمی‌شود.

اطلاعات بیشتر: [امنیت](docs/security.md) · [نصاب و پنل](docs/installer.md) · [اتصال backendها](docs/backend-integration.md)

---

## 🛠️ توسعه و مشارکت

### دریافت سورس و build

```bash
git clone https://github.com/TaJirax/CottenRouter.git
cd CottenRouter
go build -o cottenrouter ./cmd/cottenrouter
./cottenrouter version
```

### تست‌ها

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -w cmd internal
```

CI علاوه بر این موارد، جداسازی هم‌زمان پنج backend، packet کامل `16 KiB`، throughput، DoT/DoH، NaiveProxy/StunTLS، format و امنیت container بدون root را بررسی می‌کند.

برای گزارش مشکل یا پیشنهاد قابلیت جدید از [Issues](https://github.com/TaJirax/CottenRouter/issues) و برای مشارکت کد از [Pull Requests](https://github.com/TaJirax/CottenRouter/pulls) استفاده کنید.

---

## English guide

### What is CottenRouter?

**CottenRouter** is a lightweight front router that lets multiple DNS-tunnel backends share one server, one IP address, and public port `53`. It reads the first DNS question, selects the longest matching configured suffix, and forwards the packet to the correct private backend.

It can also share public DoT and HTTPS ports by reading TLS SNI and passing each encrypted stream through unchanged. CottenRouter does not terminate TLS, add DNS labels, or add protocol bytes, so it does not reduce tunnel MTU.

Key features:

- Concurrent routing for several DNS-tunnel projects on one public endpoint
- UDP DNS, DNS-over-TCP, DoT, DoH, NaiveProxy, and StunTLS passthrough
- Guided terminal UI for installation, configuration, monitoring, and removal
- Transactional host installation with snapshots, health checks, and rollback
- Fixed queues, rate limits, timeouts, connection caps, and resource controls
- Loopback-only administration API and a dashboard refreshed every two seconds
- Rootless, read-only, shell-free Docker image
- Checksum-verified `linux/amd64` and `linux/arm64` release binaries

### Requirements

For a host installation you need:

- Linux with `systemd 245+`
- `root` or `sudo` access
- A public IP and at least one unique domain or subdomain for each backend
- Ubuntu 20.04+, Debian 11+, or RHEL/Rocky/AlmaLinux 9+ is recommended
- `amd64` or `arm64` for a prebuilt binary; other architectures build from source

WSL is supported for development and testing only. Use a native Linux server for production.

### Install the latest stable release

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh | sudo bash
sudo cottenrouter tui
```

The installer downloads the latest [official release](https://github.com/TaJirax/CottenRouter/releases/latest), verifies its checksum, checks required tools, systemd, disk space, and listeners, and waits for a successful health check. A failed installation restores the previous state. Running the command again performs an in-place upgrade.

Install a specific release:

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh \
  | sudo bash -s -- --version=v1.2.8
```

Install the current development branch:

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh \
  | sudo bash -s -- --channel=edge
```

Build from source during installation:

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh \
  | sudo bash -s -- --build-from-source
```

| Installer option | Purpose |
|---|---|
| `--version=vX.Y.Z` | Install an exact tagged release |
| `--channel=stable` | Install the latest release; this is the default |
| `--channel=edge` | Build the latest default-branch commit for testing |
| `--build-from-source` | Compile locally instead of using a release binary |
| `--no-swap` | Skip the installer-owned emergency swap safeguard |

### Use the control deck

```bash
sudo cottenrouter tui
```

| Key | Action |
|---|---|
| `Space` | Select or deselect a project |
| `i` | Run guided installation for selected projects |
| `Enter` / `e` | Edit domains, private ports, TCP, DoT, and DoH |
| `a` | Open the complete native project configuration |
| `s` | Restart a project |
| `u` | Detach a project while preserving its files |
| `x` | Permanently remove a project after confirmation |
| `v` | Show credential paths and public values |
| `V` | Reveal secrets after explicit confirmation |

Give every backend a unique delegated domain, for example `cotten.example.com`, `master.example.com`, `storm.example.com`, and `feed.example.com`. Exact duplicate suffixes are rejected. Nested suffixes are supported and the longest match wins.

### Status and configuration

```bash
sudo systemctl status cottenrouter
sudo journalctl -u cottenrouter -f
sudo cottenrouter healthz -config /etc/cottenrouter/config.json
curl -fsS http://127.0.0.1:9088/v1/status
```

Validate or run a configuration manually:

```bash
cottenrouter check -config cottenrouter.json
cottenrouter serve -config cottenrouter.json
```

Start with [`cottenrouter.example.json`](cottenrouter.example.json), or use [`cottenrouter.docker.json`](cottenrouter.docker.json) for containers. Unknown configuration fields are rejected.

Import an existing SlipGate configuration:

```bash
cottenrouter slipgate-import \
  --input /etc/slipgate/config.json \
  --output cottenrouter.json
```

Inspect current upstream projects and installers:

```bash
cottenrouter catalog
cottenrouter catalog --offline
```

The online catalog resolves and verifies each project's current default branch. The offline output is bundled fallback metadata and is not recommended for installation.

### Command reference

```text
cottenrouter tui               Install, manage, and monitor projects
cottenrouter serve             Run the router with a configuration
cottenrouter check             Validate a configuration without serving
cottenrouter healthz           Probe router health
cottenrouter install           Install a backend from the CLI
cottenrouter configure         Edit stable backend settings
cottenrouter advanced          Edit advanced native backend settings
cottenrouter service           Start, stop, or restart a project
cottenrouter remove            Detach or purge a project
cottenrouter uninstall         Alias for the project removal operation
cottenrouter keys              Display project credentials
cottenrouter catalog           List current projects and installers
cottenrouter slipgate-import   Convert an existing SlipGate configuration
cottenrouter version           Print the installed version
```

Use `cottenrouter <command> --help` for command-specific options.

Manage a service directly:

```bash
sudo cottenrouter service --project=cottendns --action=restart
sudo cottenrouter service --project=stormdns --action=stop
sudo cottenrouter service --project=stormdns --action=start
```

Detach one backend while preserving its files:

```bash
sudo cottenrouter remove --project=cottendns
```

Permanently remove that backend's managed data:

```bash
sudo cottenrouter remove --project=cottendns --purge --confirm=cottendns
```

### Docker

The container runs the routing core only. You remain responsible for the lifecycle of backend containers.

```bash
curl -fsSLO https://raw.githubusercontent.com/TaJirax/CottenRouter/main/docker-compose.yml
curl -fsSLO https://raw.githubusercontent.com/TaJirax/CottenRouter/main/cottenrouter.docker.json

# Replace the example domain and backend in cottenrouter.docker.json
docker compose up -d
```

Check health:

```bash
docker compose exec cottenrouter \
  /usr/local/bin/cottenrouter healthz \
  -config /etc/cottenrouter/config.json
```

Update or remove the container:

```bash
docker compose pull
docker compose up -d
docker compose down
```

The official [`ghcr.io/tajirax/cottenrouter`](https://github.com/TaJirax/CottenRouter/pkgs/container/cottenrouter) image supports `linux/amd64` and `linux/arm64`. It listens on unprivileged port `5353` inside the container while Docker publishes host port `53`. It runs as UID `65532`, drops all capabilities, uses a read-only root filesystem, and includes no shell. See the [Docker guide](docs/docker.md).

### Upgrade

Rerun the stable installer. It preserves configuration and rolls back if the new service fails its health check:

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh | sudo bash
cottenrouter version
```

### Uninstall

Safely remove CottenRouter while preserving configuration, upstream projects, panels, pre-existing firewall rules, and swap:

```bash
sudo cottenrouter-uninstall
```

Also delete the router configuration:

```bash
sudo cottenrouter-uninstall --purge --confirm CottenRouter
```

Delete CottenRouter, every managed backend, and their managed data:

```bash
sudo cottenrouter-uninstall \
  --purge \
  --purge-backends \
  --confirm CottenRouter
```

Remove only swap created and marked as owned by the installer:

```bash
sudo cottenrouter-uninstall --remove-swap --confirm CottenRouter
```

Purge operations cannot be undone. CottenRouter removes only firewall rules, accounts, and swap carrying its ownership markers; it does not remove pre-existing state.

### Routing and security model

1. Parse the first DNS question and drop malformed packets.
2. Select the longest configured domain suffix.
3. Allocate a transaction ID on a connected backend socket.
4. Forward the packet unchanged apart from that temporary ID.
5. Accept a reply only when its question name, type, and class match exactly.
6. Restore the client's original ID and return the reply.

IDs are never reused in one socket generation. After all 65,536 IDs have been consumed, CottenRouter rotates to a new source port so a late response cannot enter the new generation.

Remote backends are rejected by default, both UDP and TCP messages are capped at `16 KiB`, and the admin API must remain on loopback. Transaction-ID and question matching is not cryptographic authentication; configured backends are trusted. Host backend installers run as root, so snapshot the server before running an upstream release you have not reviewed.

Read more in [Security](docs/security.md), [Installer and control deck](docs/installer.md), and [Backend integration](docs/backend-integration.md).

### Build, test, and contribute

```bash
git clone https://github.com/TaJirax/CottenRouter.git
cd CottenRouter
go build -o cottenrouter ./cmd/cottenrouter

go test ./...
go test -race ./...
go vet ./...
gofmt -w cmd internal
```

CI also checks five-backend isolation under load, full `16 KiB` packets, throughput, concurrent DoT/DoH and NaiveProxy/StunTLS routing, formatting, and the rootless Docker image.

Use [Issues](https://github.com/TaJirax/CottenRouter/issues) for bugs and feature requests, and [Pull Requests](https://github.com/TaJirax/CottenRouter/pulls) for code contributions.

### Documentation and credits

- [Docker guide](docs/docker.md)
- [Installer and control deck](docs/installer.md)
- [Backend integration](docs/backend-integration.md)
- [Security model](docs/security.md)
- [Configuration example](cottenrouter.example.json)
- [Latest release](https://github.com/TaJirax/CottenRouter/releases/latest)

CottenRouter integrates with [CottenDNS](https://github.com/TaJirax/CottenDns), [MasterDnsVPN](https://github.com/masterking32/MasterDnsVPN), [StormDNS](https://github.com/nullroute1970/StormDNS), [thefeed](https://github.com/sartoopjj/thefeed), and [SlipGate](https://github.com/anonvector/slipgate). Thank you to every author and contributor behind those projects.

The terminal UI uses [Bubble Tea](https://github.com/charmbracelet/bubbletea), [Bubbles](https://github.com/charmbracelet/bubbles), and [Lip Gloss](https://github.com/charmbracelet/lipgloss) from [Charm](https://charm.sh/). The secure container is based on [Distroless](https://github.com/GoogleContainerTools/distroless). See [`NOTICE.md`](NOTICE.md) and the complete dependency lists in [`go.mod`](go.mod) and [`go.sum`](go.sum).

---

## 🤝 پروژه‌ها و قدردانی

CottenRouter برای کار در کنار پروژه‌های زیر ساخته شده است. از توسعه‌دهندگان و مشارکت‌کنندگان همه این پروژه‌های متن‌باز صمیمانه سپاسگزاریم:

| پروژه | نقش در اکوسیستم | لینک |
|---|---|---|
| **CottenDNS** | DNS tunnel با UDP/TCP، DoT، DoH، ARQ و MTU discovery | [مخزن](https://github.com/TaJirax/CottenDns) |
| **MasterDnsVPN** | DNS tunneling backend | [مخزن](https://github.com/masterking32/MasterDnsVPN) |
| **StormDNS** | DNS tunneling backend | [مخزن](https://github.com/nullroute1970/StormDNS) |
| **thefeed** | feed، chat، media و relay روی DNS | [مخزن](https://github.com/sartoopjj/thefeed) |
| **SlipGate** | DNS transports، SlipNet، NaiveProxy و StunTLS | [مخزن](https://github.com/anonvector/slipgate) |

رابط ترمینال با پروژه‌های عالی [Bubble Tea](https://github.com/charmbracelet/bubbletea)، [Bubbles](https://github.com/charmbracelet/bubbles) و [Lip Gloss](https://github.com/charmbracelet/lipgloss) از مجموعه [Charm](https://charm.sh/) ساخته شده است. Image امن Docker بر پایه [Distroless](https://github.com/GoogleContainerTools/distroless) است. فهرست کامل dependencyها در [`go.mod`](go.mod) و [`go.sum`](go.sum) موجود است.

هیچ source یا binary از پروژه‌های upstream در این مخزن vendor نشده است. هر پروژه مالک copyright و license خود است و installer آن فقط با درخواست کاربر از مخزن خودش دریافت می‌شود. جزئیات: [`NOTICE.md`](NOTICE.md).

---

## 📄 مجوز

CottenRouter تحت مجوز [MIT](LICENSE) منتشر شده است. استفاده از integrationهای upstream تابع مجوز همان پروژه‌هاست.

<div align="center">

ساخته‌شده برای اجرای تمیز‌تر، امن‌تر و قابل‌مدیریت‌تر DNS Tunnelها.

[بالای صفحه](#cottenrouter)

</div>
