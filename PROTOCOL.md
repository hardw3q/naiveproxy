# NaiveCDN Protocol — Спецификация и интеграция в Android

## Обзор

NaiveCDN — это CDN-совместимый туннельный транспорт на основе Meek-подхода. Вместо HTTP CONNECT (который CDN-провайдеры часто блокируют) используются обычные HTTP POST-запросы к `/tunnel`, что позволяет работать через любой CDN, принимающий стандартные HTTPS-запросы.

```
Приложение → SOCKS5 (127.0.0.1:1080) → NaiveCDN client → CDN (HTTPS/HTTP2) → Backend → Интернет
```

---

## 1. Транспортный уровень (TLS)

### 1.1 TLS Fingerprint

Сервер проверяет TLS ClientHello на соответствие браузерному fingerprint. Стандартный Go `crypto/tls` **отклоняется**. Обязательно использование [uTLS](https://github.com/refraction-networking/utls) с Chrome-пресетом.

```go
import utls "github.com/refraction-networking/utls"

tlsConn := utls.UClient(rawConn, &utls.Config{
    ServerName: host,
}, utls.HelloChrome_Auto)

tlsConn.HandshakeContext(ctx)
```

Доступные пресеты:
| Значение конфига | uTLS ClientHelloID |
|---|---|
| `chrome_auto` (default) | `utls.HelloChrome_Auto` |
| `firefox_auto` | `utls.HelloFirefox_Auto` |
| `ios` / `safari` | `utls.HelloIOS_Auto` |

### 1.2 ALPN и HTTP-версия

CDN согласовывает `h2` (HTTP/2) через ALPN. Клиент должен поддерживать HTTP/2.  
Использовать `golang.org/x/net/http2.Transport` с кастомным `DialTLSContext`:

```go
import "golang.org/x/net/http2"

transport := &http2.Transport{
    DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
        rawConn, _ := net.Dial("tcp", serverAddr)
        tlsConn := utls.UClient(rawConn, &utls.Config{ServerName: host}, utls.HelloChrome_Auto)
        tlsConn.HandshakeContext(ctx)
        return tlsConn, nil
    },
}
client := &http.Client{Transport: transport}
```

---

## 2. Протокол туннеля

### 2.1 Установка сессии

Каждый туннель идентифицируется **Session ID** — случайным 16-байтным hex-значением, генерируемым клиентом:

```
sessionID = hex(random_bytes(16))
// Пример: "a3f2c1d4e5b6a7f8c9d0e1f2a3b4c5d6"
```

### 2.2 HTTP-запросы

Каждый обмен данными — это отдельный HTTP POST-запрос:

```
POST /tunnel HTTP/2
Host: <proxy-host>
X-Naive-Session: <sessionID>
X-Naive-Target: <host:port>        ← только в первом запросе
X-Naive-Auth: Basic <base64>
Proxy-Authorization: Basic <base64>
Content-Type: application/octet-stream
Content-Length: <n>

<бинарные данные от клиента (upstream), может быть 0 байт>
```

**Заголовки:**

| Заголовок | Описание |
|---|---|
| `X-Naive-Session` | ID сессии, неизменен на протяжении всего туннеля |
| `X-Naive-Target` | `host:port` назначения, только в **первом** POST |
| `X-Naive-Auth` | Аутентификация: `Basic base64(user:pass)` |
| `Proxy-Authorization` | То же значение что и `X-Naive-Auth` |
| `Content-Type` | Всегда `application/octet-stream` |
| `Content-Length` | Размер тела (0 если нет upstream-данных) |

### 2.3 HTTP-ответы

| Статус | Тело | Смысл |
|---|---|---|
| `200 OK` | Бинарные данные | Downstream-данные от сервера назначения |
| `204 No Content` | — | Нет downstream-данных в этом цикле, повторить |
| `410 Gone` | — | Сервер назначения закрыл соединение |
| `407` / `401` | — | Неверные credentials |

**Специальный заголовок ответа:**
```
X-Naive-Closed: 1    ← сессия закрыта, не повторять
```

### 2.4 Poll-цикл

```
                    ┌─────────────────────────────────┐
                    │         Poll loop                │
                    │                                  │
 App data ──────────►  Collect upstream chunks         │
                    │  (non-blocking, drain channel)   │
                    │                                  │
                    │  POST /tunnel                    │
                    │    body = upstream data          │
                    │    (может быть пустым)           │
                    │             │                    │
                    │    ┌────────▼────────┐           │
                    │    │  200 OK         │──────────►│ Write to app
                    │    │  204 No Content │           │ (loop again)
                    │    │  410 Gone       │──────────►│ Close tunnel
                    │    └─────────────────┘           │
                    └─────────────────────────────────┘
```

**Тайминги:**
- Ожидание первых данных перед первым POST: `100 мс`
- Back-off при отсутствии данных в обоих направлениях: `20 мс`
- Back-off после ошибки: `200 мс`

---

## 3. Аутентификация

Credentials из proxy URL (`https://user:pass@host`):

```go
auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
```

Передаётся в каждом POST в двух заголовках:
```
Proxy-Authorization: Basic dXNlcjpwYXNz
X-Naive-Auth: Basic dXNlcjpwYXNz
```

---

## 4. Конфигурация клиента

```json
{
  "listen":      "socks://127.0.0.1:1080",
  "proxy":       "https://user:pass@cdn.example.com",
  "tunnel_path": "/tunnel",
  "fingerprint": "chrome_auto"
}
```

---

## 5. Интеграция в Android-приложение

### 5.1 Архитектура

```
┌─────────────────────────────────────────────────────────┐
│                   Android App                           │
│                                                         │
│  ┌──────────────┐    ┌───────────────────────────────┐  │
│  │ UI / Config  │    │    VpnService (foreground)    │  │
│  └──────┬───────┘    │                               │  │
│         │            │  ┌──────────┐  ┌───────────┐  │  │
│         │ start      │  │ tun2socks│  │  naivecdn │  │  │
│         └───────────►│  │  (TUN)   │─►│  client   │  │  │
│                      │  └──────────┘  └─────┬─────┘  │  │
│                      └────────────────────── │ ───────┘  │
└─────────────────────────────────────────────│───────────┘
                                              │ HTTPS/h2
                                        CDN (selcdn.net)
```

### 5.2 Варианты реализации

#### Вариант A — Go via gomobile (рекомендуется)

Скомпилировать tunnel-пакет в `.aar` через `gomobile bind`:

```bash
# Установить gomobile
go install golang.org/x/mobile/cmd/gomobile@latest
gomobile init

# Собрать AAR для Android
gomobile bind -target=android/arm64 -o naivecdn.aar ./tunnel ./socks5
```

Использовать в Kotlin:
```kotlin
import naivecdn.Naivecdn

class ProxyService : VpnService() {
    private var proxy: Naivecdn.Server? = null

    fun startProxy(config: String) {
        proxy = Naivecdn.newServer(config)
        proxy?.start()
    }
}
```

#### Вариант B — Нативная реализация на Kotlin/Java

Реализовать протокол напрямую с OkHttp + Conscrypt для TLS fingerprinting.

**Зависимости `build.gradle`:**
```groovy
dependencies {
    implementation 'com.squareup.okhttp3:okhttp:4.12.0'
    implementation 'org.conscrypt:conscrypt-android:2.5.2'
    // Для TLS fingerprinting (uTLS аналог на JVM пока ограничен)
    implementation 'com.github.MagicAndre1l:tlsfingerprintingutil:1.0'
}
```

**TunnelClient.kt:**
```kotlin
class TunnelClient(
    private val proxyUrl: String,
    private val tunnelPath: String = "/tunnel",
    private val credentials: String  // "user:pass"
) {
    private val sessionId = generateSessionId()
    private val authHeader = "Basic " + Base64.encodeToString(
        credentials.toByteArray(), Base64.NO_WRAP
    )

    private val client = OkHttpClient.Builder()
        .protocols(listOf(Protocol.H2_PRIOR_KNOWLEDGE, Protocol.HTTP_2, Protocol.HTTP_1_1))
        .build()

    private fun generateSessionId(): String {
        val bytes = ByteArray(16)
        SecureRandom().nextBytes(bytes)
        return bytes.joinToString("") { "%02x".format(it) }
    }

    fun post(target: String?, upData: ByteArray, isFirst: Boolean): TunnelResponse {
        val url = "$proxyUrl$tunnelPath"
        val body = upData.toRequestBody("application/octet-stream".toMediaType())

        val request = Request.Builder()
            .url(url)
            .post(body)
            .header("X-Naive-Session", sessionId)
            .header("Proxy-Authorization", authHeader)
            .header("X-Naive-Auth", authHeader)
            .apply {
                if (isFirst && target != null) {
                    header("X-Naive-Target", target)
                }
            }
            .build()

        val response = client.newCall(request).execute()
        return when (response.code) {
            200 -> TunnelResponse.Data(response.body!!.bytes(),
                response.header("X-Naive-Closed") == "1")
            204 -> TunnelResponse.Empty(response.header("X-Naive-Closed") == "1")
            410 -> TunnelResponse.Closed
            else -> throw IOException("Unexpected status: ${response.code}")
        }
    }

    sealed class TunnelResponse {
        data class Data(val bytes: ByteArray, val closed: Boolean) : TunnelResponse()
        data class Empty(val closed: Boolean) : TunnelResponse()
        object Closed : TunnelResponse()
    }
}
```

**Stream корутина:**
```kotlin
suspend fun streamTunnel(
    target: String,
    appSocket: Socket,
    client: TunnelClient
) = withContext(Dispatchers.IO) {
    val input = appSocket.getInputStream()
    val output = appSocket.getOutputStream()
    var isFirst = true
    val upChannel = Channel<ByteArray>(32)

    // Reader
    launch {
        val buf = ByteArray(16 * 1024)
        while (isActive) {
            val n = input.read(buf)
            if (n < 0) break
            upChannel.send(buf.copyOf(n))
        }
        upChannel.close()
    }

    // Poll loop
    while (isActive) {
        val upData = mutableListOf<ByteArray>()

        // Первое чтение с таймаутом 100ms
        if (isFirst) {
            withTimeoutOrNull(100) {
                upChannel.receive()
            }?.let { upData.add(it) }
        }

        // Drain остальных
        while (true) {
            upData.add(upChannel.tryReceive().getOrNull() ?: break)
        }

        val payload = if (upData.isEmpty()) ByteArray(0)
                      else upData.reduce { a, b -> a + b }

        when (val resp = client.post(
            if (isFirst) target else null,
            payload, isFirst
        )) {
            is TunnelClient.TunnelResponse.Data -> {
                output.write(resp.bytes)
                if (resp.closed) break
            }
            is TunnelClient.TunnelResponse.Empty -> {
                if (resp.closed) break
                if (payload.isEmpty()) delay(20)
            }
            TunnelClient.TunnelResponse.Closed -> break
        }
        isFirst = false
    }
}
```

### 5.3 VpnService + tun2socks

Для маршрутизации **всего трафика** устройства:

```kotlin
class NaiveCdnVpnService : VpnService() {

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val tun = Builder()
            .addAddress("198.18.0.1", 15)
            .addRoute("0.0.0.0", 0)           // весь IPv4
            .addRoute("::", 0)                // весь IPv6
            .addDnsServer("8.8.8.8")
            // Исключить сам прокси-сервер из VPN (иначе петля!)
            .addDisallowedApplication(packageName)
            .setSession("NaiveCDN")
            .establish()

        // Запустить tun2socks: TUN fd → SOCKS5 127.0.0.1:1080
        startTun2socks(tun!!.detachFd(), "127.0.0.1", 1080)

        // Запустить naivecdn клиент
        startNaiveCdnProxy()

        return START_STICKY
    }

    private fun startTun2socks(tunFd: Int, socksHost: String, socksPort: Int) {
        // Использовать hev-socks5-tunnel или xjasonlyu/tun2socks как нативную библиотеку
        // или через gomobile
        Tun2Socks.start(tunFd, "$socksHost:$socksPort")
    }

    private fun startNaiveCdnProxy() {
        val config = loadConfig()
        NaivecdnClient.start(config)  // gomobile или нативная реализация
    }
}
```

**AndroidManifest.xml:**
```xml
<uses-permission android:name="android.permission.INTERNET" />
<uses-permission android:name="android.permission.FOREGROUND_SERVICE" />
<uses-permission android:name="android.permission.FOREGROUND_SERVICE_SPECIAL_USE" />

<service
    android:name=".NaiveCdnVpnService"
    android:permission="android.permission.BIND_VPN_SERVICE"
    android:exported="false"
    android:foregroundServiceType="specialUse">
    <intent-filter>
        <action android:name="android.net.VpnService" />
    </intent-filter>
</service>
```

**Запрос VPN-разрешения:**
```kotlin
val intent = VpnService.prepare(context)
if (intent != null) {
    startActivityForResult(intent, REQUEST_VPN_PERMISSION)
} else {
    // Уже есть разрешение
    startService(Intent(this, NaiveCdnVpnService::class.java))
}
```

### 5.4 Обход петли маршрутизации

> **Важно:** TUN-исключение для CDN-домена обязательно по умолчанию.
>
> Когда TUN-интерфейс поднят, весь трафик устройства, включая соединения
> самого naivecdn к CDN-серверу, перенаправляется в него. Это создаёт
> петлю: naivecdn пытается подключиться к CDN через TUN → TUN отправляет
> трафик в naivecdn → и так по кругу. Соединение зависает.
>
> **Правило:** при любой конфигурации TUN/VPN всегда резолвить IP домена
> CDN (через DNS вне VPN) и добавлять его в исключения **до** того, как
> поднимается TUN-интерфейс. CDN использует GeoDNS — IP может отличаться
> у разных провайдеров (пример: `151.236.112.29` на одном провайдере,
> `151.236.103.221` на мобильном интернете другого оператора). Резолвить
> нужно на старте, не хардкодить.

Когда VPN активен, трафик самого naivecdn к CDN-серверу тоже попадает в VPN — это создаёт петлю. Способы решения:

**Способ 1 — `protect()` сокет (рекомендуется):**
```kotlin
// В VpnService, перед connect():
val socket = Socket()
protect(socket)  // Исключить этот сокет из VPN-маршрутизации
socket.connect(InetSocketAddress(cdnHost, 443))
```

При использовании gomobile передать `VpnService` в Go-код:
```kotlin
NaivecdnClient.setProtector { fd -> protect(fd) }
```

```go
// В Go-коде, после создания TCP-соединения:
type Protector interface {
    Protect(fd int32) bool
}

var globalProtector Protector

func dialWithProtect(ctx context.Context, addr string) (net.Conn, error) {
    d := &net.Dialer{
        Control: func(network, address string, c syscall.RawConn) error {
            return c.Control(func(fd uintptr) {
                globalProtector.Protect(int32(fd))
            })
        },
    }
    return d.DialContext(ctx, "tcp", addr)
}
```

**Способ 2 — bypass IP в маршрутизации (резолвить динамически):**
```kotlin
// Резолвить IP CDN-домена ДО поднятия TUN (через системный DNS или Яндекс 77.88.8.8)
val cdnHost = "2e42f2cd-4ab6-4d30-8635-c4aa05fc44b1.selcdn.net"
val cdnIp = withContext(Dispatchers.IO) {
    InetAddress.getByName(cdnHost).hostAddress   // GeoDNS вернёт актуальный IP
}

// Добавить маршрут к CDN-серверу напрямую, минуя TUN
Builder()
    .addRoute("0.0.0.0", 0)
    .excludeRoute(cdnIp, 32)   // никогда не хардкодить — IP зависит от провайдера
    ...
```

---

## 6. Структура проекта Android

```
app/
├── src/main/
│   ├── java/com/example/naivecdnapp/
│   │   ├── MainActivity.kt          # UI, запрос VPN-разрешения
│   │   ├── VpnService.kt            # NaiveCdnVpnService
│   │   ├── tunnel/
│   │   │   ├── TunnelClient.kt      # HTTP POST /tunnel протокол
│   │   │   └── Socks5Server.kt      # SOCKS5 сервер (127.0.0.1:1080)
│   │   └── config/
│   │       └── Config.kt            # Парсинг конфига
│   └── jniLibs/arm64-v8a/
│       ├── libtun2socks.so          # tun2socks нативная библиотека
│       └── libnaivecdn.aar          # gomobile (опционально)
├── build.gradle
└── AndroidManifest.xml
```

---

## 7. Ключевые зависимости

| Компонент | Go | Android/Kotlin |
|---|---|---|
| TLS fingerprint | `github.com/refraction-networking/utls` | Conscrypt + кастомный SSLSocketFactory |
| HTTP/2 | `golang.org/x/net/http2` | OkHttp (Protocol.HTTP_2) |
| SOCKS5 сервер | Собственная реализация | — |
| TUN интерфейс | — | `VpnService.Builder` + tun2socks |
| Нативный код | `gomobile bind` | JNI / `.aar` |

---

## 8. Отличия от стандартного NaiveProxy

| | NaiveProxy | NaiveCDN |
|---|---|---|
| Метод туннеля | HTTP CONNECT | HTTP POST /tunnel |
| CDN-совместимость | Нет (CONNECT блокируется) | Да |
| Клиент | Chromium-based | Go + utls |
| HTTP-версия | HTTP/2 | HTTP/2 (с fallback на HTTP/1.1) |
| Session state | Stateless | Session ID в заголовке |
