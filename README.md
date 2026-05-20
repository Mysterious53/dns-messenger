# DNS Messenger

یک پیام‌رسان گروهی که تمام ترافیکش روی **DNS TXT record** سوار می‌شه.  
بر پایه پروتکل [thefeed](https://github.com/sartoopjj/thefeed) — همان رمزنگاری، همان مکانیزم scatter/cache، بدون هیچ سرویس خارجی.

---

## چطور کار می‌کنه؟

```
Client ──DNS TXT query──► Server (port 53)
       ◄──encrypted TXT──
```

- پیام‌ها به صورت **AES-256-GCM encrypted TXT record** منتقل می‌شن
- از چند resolver همزمان query می‌زنه (scatter) و سریع‌ترین جواب رو استفاده می‌کنه
- کلاینت یه وب UI محلی روی `localhost:7742` می‌زنه — مرورگر کافیه
- هیچ نیازی به Telegram، توکن، یا اکانت خارجی نیست

---

## نیازمندی‌ها

### سرور
- یک VPS لینوکسی با **دسترسی به پورت UDP 53**
- Go 1.21+ (اسکریپت setup خودش نصب می‌کنه)
- یک دامنه که NS record اون به IP سرور اشاره کنه  
  *(یا می‌شه IP سرور رو مستقیم به عنوان resolver در کلاینت وارد کرد)*

### کلاینت
- Go 1.21+ برای build
- هر مرورگر مدرن

---

## راه‌اندازی سرور

### ⚡ روش سریع — یک دستور

روی سرور لینوکسی (Ubuntu / Debian / CentOS) این دستور رو اجرا کنید:

```bash
curl -fsSL https://raw.githubusercontent.com/Mysterious53/dns-messenger/main/setup-server.sh | sudo bash
```

> یا اگه ترجیح می‌دید فایل رو اول ببینید:
> ```bash
> curl -fsSL https://raw.githubusercontent.com/Mysterious53/dns-messenger/main/setup-server.sh -o setup.sh
> cat setup.sh          # بررسی محتوا
> sudo bash setup.sh
> ```

اسکریپت به‌ترتیب:

| مرحله | کار |
|-------|-----|
| ۱ | Go 1.22 را نصب می‌کند (اگه نباشه) |
| ۲ | پروژه را clone و build می‌کند |
| ۳ | از شما **domain**، **passphrase** و پورت می‌پرسد |
| ۴ | فایل‌ها را در `/opt/dnsmessenger` نصب می‌کند |
| ۵ | یک systemd service می‌سازد و enable می‌کند |
| ۶ | پورت‌های `53/udp` و HTTP را در فایروال باز می‌کند |
| ۷ | سرویس را start می‌کند و خلاصه اتصال را نشان می‌دهد |

**نمونه خروجی پایان اسکریپت:**
```
╔══════════════════════════════════════════════════════╗
║               Setup Complete!                       ║
╠══════════════════════════════════════════════════════╣
║  Domain    : chat.example.com
║  DNS       : 1.2.3.4:53 (UDP)
║  Web UI    : http://1.2.3.4:8080
║  Rooms     : /var/lib/dnsmessenger/rooms.txt
╚══════════════════════════════════════════════════════╝
```

### روش دستی (clone + build)

```bash
# Build
go build -o dnsmsg-server ./cmd/server

# اجرا
sudo ./dnsmsg-server \
  --domain chat.example.com \
  --passphrase "رمز_مشترک" \
  --dns-addr :53 \
  --http-addr :8080 \
  --rooms rooms.txt
```

**Flag ها:**

| Flag | پیش‌فرض | توضیح |
|------|---------|-------|
| `--domain` | — | **اجباری** — دامنه DNS |
| `--passphrase` | — | **اجباری** — رمز مشترک |
| `--dns-addr` | `:53` | آدرس listen سرور DNS (UDP) |
| `--http-addr` | `:8080` | آدرس web UI سرور |
| `--rooms` | `rooms.txt` | فایل لیست اتاق‌ها |
| `--max-messages` | `50` | حداکثر پیام در حافظه هر اتاق |
| `--allow-manage` | `true` | اجازه ساخت/حذف اتاق از طریق DNS |
| `--debug` | `false` | لاگ verbose DNS |

### مدیریت اتاق‌ها

اتاق‌ها در `rooms.txt` تعریف می‌شن — یک اسم در هر خط:

```
# اتاق‌های عمومی
general
tech
random
```

بعد از ویرایش فایل، سرویس را restart کنید:
```bash
systemctl restart dnsmessenger
```

---

## راه‌اندازی کلاینت

```bash
go build -o dnsmsg-client ./cmd/client
./dnsmsg-client
```

مرورگر به‌صورت خودکار روی `http://127.0.0.1:7742` باز می‌شه.

**بار اول** — فرم اتصال نشون داده می‌شه:
- **Domain** — دامنه سرور
- **Passphrase** — همان رمز سرور
- **Username** — اسم نمایشی شما
- **Advanced** — می‌شه DNS resolver سفارشی وارد کرد

بعد از اتصال، config در `~/.dnsmessenger/config.json` ذخیره می‌شه و دفعه بعد مستقیم وارد چت می‌شی.

**Flag های کلاینت:**

| Flag | پیش‌فرض | توضیح |
|------|---------|-------|
| `--domain` | — | override دامنه ذخیره‌شده |
| `--passphrase` | — | override رمز ذخیره‌شده |
| `--username` | — | override یوزرنیم |
| `--resolvers` | — | لیست resolver‌ها با کاما |
| `--port` | `7742` | پورت web UI محلی |
| `--data-dir` | `~/.dnsmessenger` | محل ذخیره config |
| `--no-browser` | `false` | مرورگر را خودکار باز نکن |
| `--debug` | `false` | لاگ verbose DNS |

---

## تست محلی (بدون دامنه)

می‌شه سرور و کلاینت را روی یک ماشین تست کرد:

```bash
# ترمینال ۱ — سرور
sudo go run ./cmd/server \
  --domain test.local \
  --passphrase testpass \
  --dns-addr 127.0.0.1:5353

# ترمینال ۲ — کلاینت
go run ./cmd/client \
  --domain test.local \
  --passphrase testpass \
  --resolvers 127.0.0.1:5353
```

یا در کلاینت بعد از باز شدن مرورگر:
- Domain: `test.local`
- Passphrase: `testpass`
- Resolvers (Advanced): `127.0.0.1:5353`

---

## پیکربندی DNS

برای استفاده از دامنه واقعی، یکی از دو روش:

**روش ۱ — NS record (توصیه‌شده):**
```
chat.example.com.  NS  ns1.example.com.
ns1.example.com.   A   <IP سرور>
```
کلاینت‌ها از هر resolver عمومی می‌تونن وصل بشن.

**روش ۲ — IP مستقیم:**  
کلاینت‌ها IP سرور را در بخش Advanced → Resolvers وارد می‌کنن. نیازی به NS record نیست.

---

## ساختار پروژه

```
dnsmessenger/
├── cmd/
│   ├── server/main.go       سرور DNS + HTTP
│   └── client/main.go       کلاینت (web proxy محلی)
├── internal/
│   ├── protocol/            پروتکل DNS (کپی از thefeed)
│   ├── client/              fetcher، resolver، scanner، cache
│   ├── server/              ChatServer، DNSServer، Feed
│   └── web/static/          Web UI (index.html)
├── rooms.txt                لیست اتاق‌های پیش‌فرض
└── setup-server.sh          اسکریپت نصب سرور
```

---

## دستورات مفید سرور

```bash
# وضعیت سرویس
systemctl status dnsmessenger

# لاگ زنده
journalctl -u dnsmessenger -f

# ویرایش اتاق‌ها و restart
nano /var/lib/dnsmessenger/rooms.txt
systemctl restart dnsmessenger

# تست DNS دستی
dig TXT @<server-ip> <query>.<domain>
```

---

## امنیت

- تمام محتوا با **AES-256-GCM** رمز می‌شه — بدون passphrase خواندنی نیست
- هر query یک پیشوند تصادفی داره — pattern analysis سخت‌تر می‌شه
- کلاینت noise query به دامنه‌های معروف می‌زنه تا ترافیک blend بشه
- passphrase را قوی انتخاب کنید و فقط به کسانی بدید که باید دسترسی داشته باشن
