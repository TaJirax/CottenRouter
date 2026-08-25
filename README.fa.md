<div align="center">

# CottenRouter

### یک درگاه عمومی، چند تونل DNS — سریع، امن و بدون تغییر Payload

**مسیریابی امن و شفاف برای تونل‌های DNS و جریان‌های TLS**

[![Release](https://img.shields.io/github/v/release/TaJirax/CottenRouter?style=flat-square&logo=github&label=release)](https://github.com/TaJirax/CottenRouter/releases/latest)
[![CI](https://img.shields.io/github/actions/workflow/status/TaJirax/CottenRouter/ci.yml?branch=main&style=flat-square&logo=githubactions&label=tests)](https://github.com/TaJirax/CottenRouter/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/TaJirax/CottenRouter?style=flat-square&logo=go)](go.mod)
[![License](https://img.shields.io/github/license/TaJirax/CottenRouter?style=flat-square)](LICENSE)
[![Container](https://img.shields.io/badge/GHCR-multi--arch-blue?style=flat-square&logo=docker)](https://github.com/TaJirax/CottenRouter/pkgs/container/cottenrouter)

[English](README.md) · [نصب سریع](#-نصب-سریع) · [راهنمای استفاده](#-راهنمای-استفاده) · [دستورات](#-مرجع-دستورات) · [Docker](#-اجرای-docker) · [حذف](#-حذف-cottenrouter) · [قدردانی](#-پروژهها-و-قدردانی)

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
  | sudo bash -s -- --version=v1.2.10
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

[بالای صفحه](#cottenrouter) · [English](README.md)

</div>
