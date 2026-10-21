<div align="center">

<img src="docs/assets/social-preview.png" alt="CottenRouter — یک درگاه عمومی برای چند تونل DNS" width="100%">

### یک درگاه عمومی، چند تونل DNS — سریع، امن و بدون تغییر Payload

**مسیریابی امن و شفاف برای تونل‌های DNS و جریان‌های TLS**

[![Release](https://img.shields.io/github/v/release/TaJirax/CottenRouter?style=flat-square&logo=github&label=release)](https://github.com/TaJirax/CottenRouter/releases/latest)
[![CI](https://img.shields.io/github/actions/workflow/status/TaJirax/CottenRouter/ci.yml?branch=main&style=flat-square&logo=githubactions&label=tests)](https://github.com/TaJirax/CottenRouter/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/TaJirax/CottenRouter?style=flat-square&logo=go)](go.mod)
[![License](https://img.shields.io/github/license/TaJirax/CottenRouter?style=flat-square)](LICENSE)
[![Container](https://img.shields.io/badge/GHCR-multi--arch-blue?style=flat-square&logo=docker)](https://github.com/TaJirax/CottenRouter/pkgs/container/cottenrouter)

[English](README.md) · [بتای CottenMax](#cottenmax-beta) · [نصب سریع](#-نصب-سریع) · [راهنمای استفاده](#-راهنمای-استفاده) · [دستورات](#-مرجع-دستورات) · [Docker](#-اجرای-docker) · [حذف](#-حذف-cottenrouter) · [قدردانی](#-پروژهها-و-قدردانی)

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

## پنل WhiteDNS و به‌روزرسانی مستقل

گزینهٔ جداگانهٔ **WhiteDNS Panel** کنار Soar و **Soar 2.2** قرار دارد. Soar 2.2 همچنان نصب‌کنندهٔ قدیمی و هستهٔ Lantern v0.2.2 را نگه می‌دارد. پنل 0.5.1 فعلاً **فقط بتا** است؛ شاخهٔ main و انتشارهای پایدار WhiteDNS برای این بتا تغییر نکرده‌اند.

برای بتای آزمایش‌شده، [آرتیفکت CI](https://github.com/WhiteDNS/Soar-Server/actions/runs/39619905951/artifacts/12354244178) را دریافت و ZIP را باز کنید. فایل `soar-panel-0.5.1-linux-amd64.tar.gz` را به سرور منتقل کنید. دریافت آرتیفکت ممکن است ورود به GitHub بخواهد. Router فقط آرشیو آزمایش‌شده با SHA-256 برابر `59f64f2de6a5fbbc652bd93e0cd9e2ab4675816b9cbf27a1324f66eceab8f90b` را می‌پذیرد. آرتیفکت موقت است؛ پس از انقضا، نصب فعلی را تا ارائهٔ بتای تأییدشده یا نسخهٔ پایدار سازگار حفظ کنید.

```bash
sudo cottenrouter install --project whitedns-panel --domain p.example.com --tcp \
  --panel-beta-bundle /root/soar-panel-0.5.1-linux-amd64.tar.gz
sudo cottenrouter update --project whitedns-panel \
  --panel-beta-bundle /root/soar-panel-0.5.1-linux-amd64.tar.gz
```

در فرم TUI مسیر فایل را در **Beta CI archive** وارد کنید. فیلد خالی یا فرمان بدون این گزینه، آخرین انتشار پایدار `panel-v*` را مستقل از نسخهٔ Router بررسی می‌کند. نسخهٔ پایدار قدیمی فاقد اصلاحات نشست است و پیش از توقف سرویس رد می‌شود. انتشار پایدار سازگار آینده با همین فرمان‌های عادی دریافت خواهد شد؛ بتا خودکار انتخاب یا به‌عنوان پایدار منتشر نمی‌شود.

برای پنل یک دامنهٔ واگذارشدهٔ کوتاه و جداگانه انتخاب کنید. زیر‌دامنه‌های حساب‌ها از همان مسیر پنل عبور می‌کنند. پنل، Soar روی UDP و Cotten معمولی روی UDP/TCP را ارائه می‌کند. گزینهٔ مستقل CottenDNS حالت معمولی و CottenMax را با دامنه و کلید مشترک ارائه می‌کند.

در نصب اول رمز مدیر در ترمینال پرسیده می‌شود. نصب خودکار می‌تواند `--panel-password-file` را با فایل خصوصی 0600 دریافت کند. به‌روزرسانی، پایگاه داده، کلید حساب‌ها، مصرف و سقف‌های انتخابی را حفظ می‌کند و در صورت خرابی به نسخهٔ قبلی بازمی‌گردد.

مدیریت فقط روی loopback، معمولاً `127.0.0.1:2054`، فعال است. آدرس را با `sudo cottenrouter keys --project whitedns-panel` ببینید و با SSH forwarding باز کنید. نگهداری این نصب با Router است. مدیریت حساب و دانلود پشتیبان در پنل فعال است؛ به‌روزرسان مستقلِ دارای دسترسی سیستمی و راه‌انداز HTTPS عمومی در نصب Router استفاده نمی‌شود.

برای CottenDNS از `sudo cottenrouter update --project cottendns` استفاده کنید. در هر عملیات آخرین انتشار سازگار مستقل از Router دریافت می‌شود و نسخهٔ قدیمی پنهانی جایگزین نمی‌شود.

CottenDNS و CottenMax مستقلِ منتشرشده سقف پنهان ۸ نشست ندارند: پیش‌فرض ۲۰۴۸ است و expanded حداقل ۴۰۹۶ را اعمال می‌کند. هستهٔ تعبیه‌شدهٔ فعلی و **بتای پنل** برای حساب جدید یا فاقد مقدار صریح، پیش‌فرض ۶۴ و سقف قابل تنظیم ۴۰۹۶ دارند. مقدار صریح مدیر حفظ می‌شود. نسخهٔ پایدار موجود پنل این تغییرات بتا را دریافت نکرده است. این اعداد سقف نشست هستند و تضمین تعداد کاربر هم‌زمان نیستند.

<a id="cottenmax-beta"></a>

## بتای CottenMax

**CottenMax به‌صورت اختیاری برای آزمایش بتا درون CottenDNS آماده است.** از همان سرویس، دامنه delegate‌شده، کلید مشترک و مسیر فعلی CottenDNS در Router استفاده می‌کند. در کلاینت سازگار، حالت **Normal / CottenMax beta** را انتخاب کنید؛ سرور به‌روز CottenDNS هر دو حالت را می‌پذیرد. نیازی به پروژه، مسیر یا کلید مشترک جدا در Router نیست.

| بخش | وضعیت بتا |
|---|---|
| سورس Router و CottenDNS | تغییرات روی شاخه `main` در [TaJirax/CottenRouter](https://github.com/TaJirax/CottenRouter)، [WhiteDNS/CottenDNS](https://github.com/WhiteDNS/CottenDNS) و [TaJirax/cottenDNS](https://github.com/TaJirax/cottenDNS) قرار دارند |
| کلاینت دسکتاپ | [بیلد debug ویندوز](https://github.com/WhiteDNS/WhiteDNS-Desktop/actions/runs/39601468697/artifacts/12346273079)، با انتخاب Normal / CottenMax |
| کلاینت اندروید | [APKهای debug](https://github.com/WhiteDNS/WhiteDNS-Android/actions/runs/39604706552/artifacts/12348436491)، با انتخاب حالت در پروفایل |
| نصب‌کننده‌های منتشرشده | [نسخه فعلی CottenDNS](https://github.com/WhiteDNS/CottenDNS/releases/tag/v2026.10.21.140901-0a7a241) با پشتیبانی هم‌زمان Normal و Max نصب می‌شود |

این artifactهای CI موقت هستند و ممکن است دریافت آن‌ها به ورود به حساب GitHub نیاز داشته باشد. **release جدیدی برای دسکتاپ یا اندروید منتشر نشده است.** هنگام آزمایش روی دستگاه واقعی، اینترنت موبایل و اتصال‌های طولانی، حالت Normal را برای بازگشت در دسترس نگه دارید.

کلاینت‌های به‌روز Max برای انتقال واقعی ترافیک تونل از DNS روی UDP و TCP پشتیبانی می‌کنند. حالت Auto در کشف resolver و اسکن MTU همراه با احراز هویت، هر دو مسیر را مستقل آزمایش می‌کند و ترافیک را از مسیرهای قابل استفاده عبور می‌دهد. پراکسی محلی دستگاه و مسیر Router همان تنظیمات قبلی را دارند؛ [راهنمای انتقال UDP/TCP و اسکن](https://github.com/WhiteDNS/CottenDNS/blob/main/docs/COTTENMAX_OPTIONS.md#udptcp-connections-and-authenticated-mtu-scans) را ببینید.

برای افزایش ظرفیت نشست، در فیلد **Capacity** فرم CottenDNS گزینه **expanded** را وارد کنید یا به دستور نصب/تنظیم آن `--expanded-capacity` اضافه کنید. این انتخاب حداقل **۴۰۹۶ نشست سرور**، **۱۰۲۴ جریان معمولی در هر نشست** و **۵۱۲ جریان Max در هر نشست** را تنظیم می‌کند و مقادیر سفارشی بالاتر را حفظ می‌کند. سقف بالاتر Max به سرور و کلاینت به‌روز نیاز دارد. گزینه **keep** مقادیر فعلی را حفظ می‌کند. محدودیت نرخ و حجم داده برقرار می‌ماند. این اعداد سقف تنظیمات هستند، نه تضمین تعداد کاربر؛ [راهنمای ظرفیت و محدودیت‌ها](https://github.com/WhiteDNS/CottenDNS/blob/main/docs/COTTENMAX_OPTIONS.md#session-and-per-client-capacity) را ببینید.

برای رابط و بررسی نصب جدید، آخرین Router را نصب کنید. CottenDNS را از TUI یا با `sudo cottenrouter update --project cottendns` به‌روز کنید؛ نسخه پروژه مستقل از Router از آخرین Release دریافت می‌شود. اسکنر کلاینت مسیرهای UDP/TCP فعال در آزمایش و موفق را نشان می‌دهد و وضعیت اتصال Max پس از تأیید انتقال معتبر نمایش داده می‌شود.

اگر بک‌اند از قبل زیر مدیریت Router است، هنگام جایگزینی باینری سرور، پوشه کاری سرویس، تنظیمات، کلید و پورت‌های خصوصی loopback آن را حفظ کنید. پورت عمومی `53` باید در اختیار CottenRouter بماند؛ بک‌اند را به این پورت منتقل نکنید. از همان مسیر CottenDNS استفاده کنید، سپس در کلاینت Max را انتخاب و دوباره متصل شوید. دامنه‌های بسیار طولانی حالت legacy ممکن است از محدودیت طول دامنه Max بیشتر باشند.

تب **Guide** و فرم نصب CottenDNS در Router، اشتراک دامنه/کلید و نیاز به سرور سازگار را توضیح می‌دهند. [راهنمای ترمینال و تغییر حالت](https://github.com/WhiteDNS/CottenDNS/blob/main/docs/COTTENMAX_BETA_TERMINAL.md) و [ترنسپورت‌ها و تنظیمات اختیاری Max](https://github.com/WhiteDNS/CottenDNS/blob/main/docs/COTTENMAX_OPTIONS.md) را ببینید. DoH روی HTTP/3 و DoQ گزینه‌های ارتباط کلاینت با resolver هستند؛ این گزینه‌ها QUIC passthrough یا listener کوئیک authoritative به CottenRouter اضافه نمی‌کنند.

**Soar** و **Soar 2.2** همچنان بک‌اندهای جدا با zone و کلید مستقل هستند. CottenMax حالت داخلی CottenDNS است؛ Soar 2.2 همان گزینه دارای موتور قدیمی‌تر Lantern v0.2.2 است که پایین توضیح داده شده است.

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

برای نصب همین checkout با تغییرات محلی و هر هفت backend، از ریشه مخزن در Linux یا WSL اجرا کنید:

```bash
sudo bash scripts/install.sh --local-source="$PWD"
sudo cottenrouter tui
```

در بخش **Install** با **Space** پروژه‌ها را انتخاب و با **i** نصب مرحله‌ای را شروع کنید. **Tab** فیلد بعدی و **Enter** روی آخرین فیلد شروع نصب است. برای هر پروژه دامنه جدا وارد کنید. thefeed حداقل یک منبع می‌خواهد و SlipGate راهنمای نصب اصلی خود را باز می‌کند.

### روش پیشنهادی: آخرین نسخه پایدار

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh | sudo bash
sudo cottenrouter tui
```

نصاب، آخرین [نسخه رسمی](https://github.com/TaJirax/CottenRouter/releases/latest) را دریافت و checksum آن را بررسی می‌کند. اجرای دوباره همین دستور، برنامه را به‌صورت امن ارتقا می‌دهد.

### نصب نسخه مشخص

```bash
curl -fsSL https://raw.githubusercontent.com/TaJirax/CottenRouter/main/scripts/install.sh \
  | sudo bash -s -- --version=v1.2.19
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
| `Tab` / `Shift+Tab` | جابه‌جایی میان تب‌های Overview، Install، Ports و Guide |
| `Space` | انتخاب یا لغو انتخاب پروژه |
| `i` | نصب هدایت‌شده پروژه‌های انتخاب‌شده |
| `Enter` / `e` | ویرایش دامنه، پورت خصوصی، TCP، DoT و DoH |
| `a` | باز کردن تنظیمات پیشرفته و native پروژه |
| `s` | restart پروژه |
| `U` | به‌روزرسانی بک‌اند نصب‌شده از کانال انتشار آن |
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
soar.example.com    → Soar
soar22.example.com  → Soar 2.2
```

دو سرویس نمی‌توانند دقیقاً یک suffix یکسان داشته باشند. suffixهای تو‌در‌تو مجاز هستند و طولانی‌ترین تطبیق برنده می‌شود.

### نصب Soar

در Project Manager پروژه **Soar** را انتخاب کنید یا دستورهای زیر را اجرا کنید:

```bash
sudo cottenrouter install --project=soar --domain=soar.example.com
sudo cottenrouter keys --project=soar
```

گزینه **Soar** آخرین commit مشخص‌شده از مخزن رسمی Lantern را از سورس build می‌کند؛ در صورت نیاز ابزار Go سازگار را نیز دریافت می‌کند. این کار به دسترسی شبکه برای دریافت Go و dependencyها و فضای دیسک برای compiler و cache نیاز دارد. سرویس `soar-server` به‌صورت پیش‌فرض فقط روی UDP با آدرس `127.0.0.1:5305` گوش می‌دهد.

گزینه جداگانه **Soar 2.2** از بسته standalone setup 0.1.3 پروژه WhiteDNS با **موتور قدیمی‌تر Lantern نسخه v0.2.2** استفاده می‌کند. باینری آماده فقط برای `amd64` با بررسی checksum دریافت می‌شود و سرویس `soar22-server` روی UDP با آدرس `127.0.0.1:5306` اجرا می‌شود:

```bash
sudo cottenrouter install --project=soar22 --domain=soar22.example.com
sudo cottenrouter keys --project=soar22
```

هر دو گزینه می‌توانند هم‌زمان با zone، پورت و کلید جدا اجرا شوند. مدیریت سرویس‌ها با CottenRouter است؛ wizard، مدیر مستقل و پنل وب WhiteDNS اجرا نمی‌شوند. هر گزینه یک zone اختصاصی دارد و extra/chat domain، TCP بک‌اند، DoT و DoH برای آن فعال نیستند.

**کلید عمومی** نمایش‌داده‌شده را در اختیار client قرار دهید؛ seed خصوصی روی سرور باقی می‌ماند و هنگام نصب مجدد حفظ می‌شود. راهنمای تنظیمات و بررسی اتصال در [راهنمای Soar](docs/soar.md) آمده است.

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
cottenrouter update            به‌روزرسانی مستقل backend نصب‌شده
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

### به‌روزرسانی مستقل backendها

```bash
sudo cottenrouter update --project cottendns
```

در TUI پروژه نصب‌شده را انتخاب کنید و `U` را بزنید. هر بار اسکریپت فعلی پروژه
دریافت می‌شود و برای انتشار نسخه‌های جدید backend نیازی به آپدیت دوباره
CottenRouter نیست. دامنه‌ها، کلیدها و تنظیمات مسیریابی حفظ می‌شوند.
برای CottenDNS آخرین انتشار رسمی `WhiteDNS/CottenDNS` دریافت و نسخه باینری
نصب‌شده بررسی می‌شود؛ خطای دریافت نسخه به نصب بی‌صدای نسخه قدیمی منجر نمی‌شود.

Release فعلی CottenDNS حالت Normal و Max را با هم دارد. تنظیمات و کلید سفارشی حفظ می‌شوند. اگر مدیر در drop-in مقدار `ExecStart` را تغییر داده است، پیش از نصب خودکار آن را بررسی کنید؛ نصب‌کننده این مورد را قبل از توقف سرویس گزارش می‌کند.

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

اطلاعات بیشتر: [امنیت](docs/security.md) · [نصاب و پنل](docs/installer.md) · [اتصال backendها](docs/backend-integration.md) · [یادداشت‌های انتشار v1.2.13](docs/releases/v1.2.13.md)

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

CI علاوه بر این موارد، جداسازی هم‌زمان backendها، packet کامل `16 KiB`، throughput، DoT/DoH، NaiveProxy/StunTLS، format و امنیت container بدون root را بررسی می‌کند.

برای گزارش مشکل یا پیشنهاد قابلیت جدید از [Issues](https://github.com/TaJirax/CottenRouter/issues) و برای مشارکت کد از [Pull Requests](https://github.com/TaJirax/CottenRouter/pulls) استفاده کنید.

---

## 🤝 پروژه‌ها و قدردانی

CottenRouter برای کار در کنار پروژه‌های زیر ساخته شده است. از توسعه‌دهندگان و مشارکت‌کنندگان همه این پروژه‌های متن‌باز صمیمانه سپاسگزاریم:

| پروژه | نقش در اکوسیستم | لینک |
|---|---|---|
| **CottenDNS** | DNS tunnel با UDP/TCP، DoT، DoH، ARQ، MTU discovery و حالت اختیاری بتای CottenMax | [مخزن](https://github.com/WhiteDNS/CottenDNS) |
| **MasterDnsVPN** | DNS tunneling backend | [مخزن](https://github.com/masterking32/MasterDnsVPN) |
| **StormDNS** | DNS tunneling backend | [مخزن](https://github.com/nullroute1970/StormDNS) |
| **thefeed** | feed، chat، media و relay روی DNS | [مخزن](https://github.com/sartoopjj/thefeed) |
| **Soar** | انتقال جریان‌های TCP رمزنگاری‌شده از طریق recursive DNS resolverها | [مخزن](https://github.com/getlantern/soar) |
| **Soar 2.2 / WhiteDNS** | بسته مستقل موتور Lantern Soar نسخه v0.2.2 | [مخزن](https://github.com/WhiteDNS/Soar-Server) |
| **SlipGate** | DNS transports، SlipNet، NaiveProxy و StunTLS | [مخزن](https://github.com/anonvector/slipgate) |

رابط ترمینال با پروژه‌های عالی [Bubble Tea](https://github.com/charmbracelet/bubbletea)، [Bubbles](https://github.com/charmbracelet/bubbles) و [Lip Gloss](https://github.com/charmbracelet/lipgloss) از مجموعه [Charm](https://charm.sh/) ساخته شده است. Image امن Docker بر پایه `scratch` است و فقط فایل اجرایی استاتیک، گواهی‌های CA و تنظیمات را دارد و با کاربر غیر root اجرا می‌شود. فهرست کامل dependencyها در [`go.mod`](go.mod) و [`go.sum`](go.sum) موجود است.

هیچ source یا binary از پروژه‌های upstream در این مخزن vendor نشده است. هر پروژه مالک copyright و license خود است و installer آن فقط با درخواست کاربر از مخزن خودش دریافت می‌شود. جزئیات: [`NOTICE.md`](NOTICE.md).

---

## 📄 مجوز

CottenRouter تحت مجوز [MIT](LICENSE) منتشر شده است. استفاده از integrationهای upstream تابع مجوز همان پروژه‌هاست.

<div align="center">

ساخته‌شده برای اجرای تمیز‌تر، امن‌تر و قابل‌مدیریت‌تر DNS Tunnelها.

[بالای صفحه](#cottenrouter) · [English](README.md)

</div>
