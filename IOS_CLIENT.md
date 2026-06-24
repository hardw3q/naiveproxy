# NaiveCDN iOS Client — План реализации

Нативный iOS-клиент на Swift с туннелем через `NEPacketTunnelProvider`
и Go-библиотекой (gomobile) для TLS-фингерпринта. Распространение через
AltStore как `.ipa`.

---

## 1. Архитектура

```
┌─────────────────────────────────────────────────────────────┐
│  iOS App (SwiftUI)                                          │
│  ┌──────────────┐   NETunnelProviderManager                 │
│  │  UI / Config │──────────────────────────┐                │
│  └──────────────┘                          │                │
│                                            ▼                │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  Network Extension (отдельный процесс)              │   │
│  │  NaiveCdnTunnelProvider : NEPacketTunnelProvider    │   │
│  │                                                     │   │
│  │  Swift ──► NaiveCdnGo.xcframework (gomobile)       │   │
│  │             │                                       │   │
│  │             ├── uTLS (Chrome fingerprint)           │   │
│  │             ├── HTTP/2 Transport                    │   │
│  │             └── POST /tunnel poll loop              │   │
│  │                          │                          │   │
│  │  TUN (utun0) ◄──────────┘ ◄──── NEPacketTunnelFlow │   │
│  └─────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
              │
              ▼ HTTPS/HTTP2 (uTLS Chrome)
    CDN (selcdn.net / CloudFront / etc.)
              │
              ▼
         Backend → Internet
```

### Компоненты

| Компонент | Технология | Назначение |
|---|---|---|
| UI-приложение | SwiftUI | Конфиг, кнопка вкл/выкл, статус |
| Network Extension | `NEPacketTunnelProvider` | TUN-уровень, обрабатывает пакеты |
| Туннельная библиотека | Go + gomobile | uTLS, HTTP/2, poll loop |
| Пакетный роутинг | `tun2socks` или нативный IP-стек | IP-пакеты → TCP/UDP-потоки |

---

## 2. Требования

### Apple Developer Account

`NEPacketTunnelProvider` требует entitlement
`com.apple.developer.networking.networkextension`, который выдаётся только
на **платном аккаунте** ($99/год). Без него расширение не запустится.

| Способ | Аккаунт | Ограничения |
|---|---|---|
| AltStore (бесплатный Apple ID) | Free | 3 приложения, сертификат 7 дней, **NetworkExtension недоступен** |
| AltStore (платный Developer) | Paid $99 | Нет ограничений по сертификату, entitlement работает |
| AltStore+ источник | Paid $99 | Авто-переподпись через AltStore CDN |
| TestFlight | Paid $99 | До 10 000 тестеров |

> **Для AltStore-дистрибуции с VPN**: нужен платный аккаунт. Entitlement
> прописывается в `*.entitlements` и `provisioning profile` — AltStore
> сохраняет их при переподписи.

### Зависимости

```
Xcode 15+
Go 1.22+
gomobile (go install golang.org/x/mobile/cmd/gomobile@latest)
```

---

## 3. Go → iOS Framework (gomobile)

### Интерфейс для экспорта

Создать `go/mobile/ios.go` — тонкая обёртка над `tunnel.Stream`:

```go
// go/mobile/ios.go
package mobile

import (
    "context"
    "net"

    "github.com/hardw3q/naiveproxy/go/tunnel"
)

// TunnelConfig передаётся из Swift через gomobile.
type TunnelConfig struct {
    ProxyURL           string
    TunnelPath         string
    TLSFingerprint     string
    InsecureSkipVerify bool
}

// SocketProtector — реализуется в Swift, вызывается для protect(fd).
type SocketProtector interface {
    Protect(fd int32) bool
}

var globalProtector SocketProtector

// SetProtector регистрирует VpnService-совместимый протектор сокетов.
func SetProtector(p SocketProtector) {
    globalProtector = p
}

// StartTunnel запускает туннель; conn — объект, реализующий net.Conn через gomobile.
// Блокирует до закрытия соединения или отмены ctx.
func StartTunnel(cfg *TunnelConfig, target string, conn ClientConn) error {
    tcfg := tunnel.Config{
        ProxyURL:           cfg.ProxyURL,
        TunnelPath:         cfg.TunnelPath,
        TLSFingerprint:     cfg.TLSFingerprint,
        InsecureSkipVerify: cfg.InsecureSkipVerify,
    }
    return tunnel.Stream(context.Background(), tcfg, target, &connAdapter{conn})
}

// ClientConn — интерфейс, реализуемый в Swift (gomobile генерирует биндинг).
type ClientConn interface {
    Read(b []byte) (int, error)
    Write(b []byte) (int, error)
    Close() error
}

type connAdapter struct{ c ClientConn }

func (a *connAdapter) Read(b []byte) (int, error)         { return a.c.Read(b) }
func (a *connAdapter) Write(b []byte) (int, error)        { return a.c.Write(b) }
func (a *connAdapter) Close() error                        { return a.c.Close() }
func (a *connAdapter) LocalAddr() net.Addr                 { return dummyAddr{} }
func (a *connAdapter) RemoteAddr() net.Addr                { return dummyAddr{} }
func (a *connAdapter) SetDeadline(t interface{}) error     { return nil }
func (a *connAdapter) SetReadDeadline(t interface{}) error { return nil }
func (a *connAdapter) SetWriteDeadline(t interface{}) error { return nil }

type dummyAddr struct{}
func (dummyAddr) Network() string { return "tcp" }
func (dummyAddr) String() string  { return "0.0.0.0:0" }
```

### Сборка xcframework

```bash
# Инициализация gomobile (один раз)
gomobile init

cd go

# Сборка для симулятора + реального устройства
gomobile bind \
    -target ios,iossimulator \
    -o ../ios/NaiveCdnGo.xcframework \
    -ldflags "-w -s" \
    ./mobile

# Результат: NaiveCdnGo.xcframework — добавить в Xcode как зависимость
```

---

## 4. Структура Xcode-проекта

```
NaiveCdnApp/
├── NaiveCdnApp/                    ← основное приложение (SwiftUI)
│   ├── NaiveCdnApp.swift
│   ├── ContentView.swift
│   ├── VpnManager.swift
│   ├── Assets.xcassets
│   └── NaiveCdnApp.entitlements
│
├── TunnelExtension/                ← Network Extension
│   ├── PacketTunnelProvider.swift
│   ├── Info.plist
│   └── TunnelExtension.entitlements
│
├── NaiveCdnGo.xcframework/         ← собранный gomobile фреймворк
│
└── NaiveCdnApp.xcodeproj
```

---

## 5. Network Extension

### `TunnelExtension.entitlements`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>com.apple.developer.networking.networkextension</key>
    <array>
        <string>packet-tunnel-provider</string>
    </array>
</dict>
</plist>
```

### `PacketTunnelProvider.swift`

```swift
import NetworkExtension
import NaiveCdnGo   // gomobile xcframework

class PacketTunnelProvider: NEPacketTunnelProvider {

    private var tunnelTask: Task<Void, Never>?

    override func startTunnel(options: [String: NSObject]?, completionHandler: @escaping (Error?) -> Void) {
        guard let proto = protocolConfiguration as? NETunnelProviderProtocol,
              let settings = proto.providerConfiguration else {
            completionHandler(NSError(domain: "NaiveCDN", code: 1))
            return
        }

        let proxyURL   = settings["proxyURL"]   as? String ?? ""
        let tunnelPath = settings["tunnelPath"] as? String ?? "/tunnel"
        let cdnIP      = settings["cdnIP"]      as? String ?? ""

        // TUN-настройки
        let networkSettings = NEPacketTunnelNetworkSettings(tunnelRemoteAddress: "10.0.0.1")
        networkSettings.ipv4Settings = NEIPv4Settings(addresses: ["10.0.0.2"], subnetMasks: ["255.255.255.0"])
        networkSettings.ipv4Settings?.includedRoutes = [NEIPv4Route.default()]
        // Исключить CDN-сервер из TUN — обязательно во избежание петли
        if !cdnIP.isEmpty {
            networkSettings.ipv4Settings?.excludedRoutes = [
                NEIPv4Route(destinationAddress: cdnIP, subnetMask: "255.255.255.255")
            ]
        }
        networkSettings.dnsSettings = NEDNSSettings(servers: ["77.88.8.8", "77.88.8.1"])
        networkSettings.mtu = 1500

        setTunnelNetworkSettings(networkSettings) { [weak self] error in
            if let error = error {
                completionHandler(error)
                return
            }
            self?.startForwarding(proxyURL: proxyURL, tunnelPath: tunnelPath)
            completionHandler(nil)
        }
    }

    private func startForwarding(proxyURL: String, tunnelPath: String) {
        tunnelTask = Task {
            // Читаем IP-пакеты из TUN и проксируем каждый TCP-поток
            // через Go-туннель (tun2socks-логика упрощена для примера)
            while !Task.isCancelled {
                guard let packets = try? await packetFlow.readPacketObjects() else { break }
                for packet in packets {
                    handlePacket(packet.data, proxyURL: proxyURL, tunnelPath: tunnelPath)
                }
            }
        }
    }

    private func handlePacket(_ data: Data, proxyURL: String, tunnelPath: String) {
        // Парсим IP/TCP заголовок → получаем target host:port
        // Создаём пару pipe-сокетов, запускаем MobileStartTunnel в Go
        // Полная реализация: использовать tun2socks (lwIP) или swift-nio
        let cfg = MobileTunnelConfig()
        cfg.proxyURL = proxyURL
        cfg.tunnelPath = tunnelPath
        cfg.tlsFingerprint = "chrome_auto"

        let (localSock, goSock) = createSocketPair()
        MobileStartTunnel(cfg, extractTarget(data), goSock)   // Go goroutine
        forwardTunToSocket(data, localSock)
    }

    override func stopTunnel(with reason: NEProviderStopReason, completionHandler: @escaping () -> Void) {
        tunnelTask?.cancel()
        completionHandler()
    }
}
```

> **Полная реализация пакетного роутинга**: использовать
> [tun2socks](https://github.com/xjasonlyu/tun2socks) в виде Go-библиотеки
> или [swift-nio](https://github.com/apple/swift-nio) для разбора
> TCP/IP стека. Это значительный объём кода, выходящий за рамки плана.

---

## 6. UI — SwiftUI

### `VpnManager.swift`

```swift
import NetworkExtension
import Foundation

@MainActor
class VpnManager: ObservableObject {

    @Published var status: NEVPNStatus = .disconnected
    @Published var errorMessage: String?

    private var manager: NETunnelProviderManager?

    func load() async {
        let managers = try? await NETunnelProviderManager.loadAllFromPreferences()
        manager = managers?.first ?? NETunnelProviderManager()
        status = manager?.connection.status ?? .disconnected
    }

    func connect(proxyURL: String, tunnelPath: String) async {
        guard let manager = manager else { return }

        // Резолвим CDN-домен ДО поднятия TUN
        let cdnHost = extractHost(from: proxyURL)
        let cdnIP = await resolveCDNHost(cdnHost) ?? ""

        let proto = NETunnelProviderProtocol()
        proto.providerBundleIdentifier = "com.pixels-it.naivecdnapp.tunnel"
        proto.serverAddress = cdnHost
        proto.providerConfiguration = [
            "proxyURL":   proxyURL,
            "tunnelPath": tunnelPath,
            "cdnIP":      cdnIP,       // передаём для excludedRoutes
        ]

        manager.protocolConfiguration = proto
        manager.localizedDescription = "NaiveCDN"
        manager.isEnabled = true

        do {
            try await manager.saveToPreferences()
            try await manager.loadFromPreferences()
            try manager.connection.startVPNTunnel()
        } catch {
            errorMessage = error.localizedDescription
        }
    }

    func disconnect() {
        manager?.connection.stopVPNTunnel()
    }

    private func resolveCDNHost(_ host: String) async -> String? {
        await withCheckedContinuation { continuation in
            DispatchQueue.global().async {
                let result = host.withCString { ptr -> String? in
                    var hints = addrinfo()
                    hints.ai_family = AF_INET
                    var res: UnsafeMutablePointer<addrinfo>?
                    guard getaddrinfo(ptr, nil, &hints, &res) == 0, let info = res else {
                        return nil
                    }
                    defer { freeaddrinfo(res) }
                    var addr = sockaddr_in()
                    memcpy(&addr, info.pointee.ai_addr, MemoryLayout<sockaddr_in>.size)
                    return String(cString: inet_ntoa(addr.sin_addr))
                }
                continuation.resume(returning: result)
            }
        }
    }

    private func extractHost(from urlString: String) -> String {
        URL(string: urlString)?.host ?? urlString
    }
}
```

### `ContentView.swift`

```swift
import SwiftUI
import NetworkExtension

struct ContentView: View {

    @StateObject private var vpn = VpnManager()
    @State private var proxyURL  = "https://user:pass@cdn.example.com"
    @State private var tunnelPath = "/tunnel"

    var body: some View {
        NavigationStack {
            Form {
                Section("Сервер") {
                    TextField("Proxy URL", text: $proxyURL)
                        .autocapitalization(.none)
                    TextField("Tunnel path", text: $tunnelPath)
                        .autocapitalization(.none)
                }
                Section {
                    HStack {
                        Circle()
                            .fill(statusColor)
                            .frame(width: 10, height: 10)
                        Text(statusText)
                    }
                    Button(vpn.status == .connected ? "Отключить" : "Подключить") {
                        Task {
                            if vpn.status == .connected {
                                vpn.disconnect()
                            } else {
                                await vpn.connect(proxyURL: proxyURL, tunnelPath: tunnelPath)
                            }
                        }
                    }
                    .buttonStyle(.borderedProminent)
                }
                if let err = vpn.errorMessage {
                    Section { Text(err).foregroundColor(.red).font(.caption) }
                }
            }
            .navigationTitle("NaiveCDN")
        }
        .task { await vpn.load() }
    }

    private var statusColor: Color {
        switch vpn.status {
        case .connected:    return .green
        case .connecting:   return .yellow
        default:            return .gray
        }
    }

    private var statusText: String {
        switch vpn.status {
        case .connected:     return "Подключено"
        case .connecting:    return "Подключение..."
        case .disconnecting: return "Отключение..."
        default:             return "Отключено"
        }
    }
}
```

---

## 7. Сборка IPA

### Ручная сборка (Xcode)

```bash
# 1. Собрать gomobile xcframework
cd go
gomobile bind -target ios,iossimulator \
    -o ../ios/NaiveCdnGo.xcframework ./mobile

# 2. Архивировать приложение
xcodebuild archive \
    -project ios/NaiveCdnApp.xcodeproj \
    -scheme NaiveCdnApp \
    -destination "generic/platform=iOS" \
    -archivePath build/NaiveCdnApp.xcarchive \
    CODE_SIGN_IDENTITY="iPhone Distribution" \
    PROVISIONING_PROFILE_SPECIFIER="NaiveCDN Distribution"

# 3. Экспортировать IPA
xcodebuild -exportArchive \
    -archivePath build/NaiveCdnApp.xcarchive \
    -exportPath build/ipa \
    -exportOptionsPlist ExportOptions.plist
```

### `ExportOptions.plist`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>method</key>
    <string>ad-hoc</string>        <!-- или developer-id для AltStore -->
    <key>teamID</key>
    <string>XXXXXXXXXX</string>
    <key>uploadBitcode</key>
    <false/>
    <key>compileBitcode</key>
    <false/>
</dict>
</plist>
```

### GitLab CI (требует macOS-раннер)

```yaml
build:ios:
  stage: build
  tags:
    - macos       # self-hosted macOS runner с Xcode
  only:
    - tags
  before_script:
    - go install golang.org/x/mobile/cmd/gomobile@latest
    - gomobile init
  script:
    - cd go && gomobile bind -target ios,iossimulator
          -o ../ios/NaiveCdnGo.xcframework ./mobile
    - xcodebuild archive
          -project ios/NaiveCdnApp.xcodeproj
          -scheme NaiveCdnApp
          -destination "generic/platform=iOS"
          -archivePath build/NaiveCdnApp.xcarchive
    - xcodebuild -exportArchive
          -archivePath build/NaiveCdnApp.xcarchive
          -exportPath build/ipa
          -exportOptionsPlist ios/ExportOptions.plist
    - |
      VERSION=${CI_COMMIT_TAG}
      curl --fail -u "${NEXUS_USER}:${NEXUS_PASSWORD}" \
           --upload-file build/ipa/NaiveCdnApp.ipa \
           "https://pypi.pixels-it.ru/repository/naivecdn/${VERSION}/NaiveCdnApp.ipa"
      # Обновить AltStore source
      curl --fail -u "${NEXUS_USER}:${NEXUS_PASSWORD}" \
           --upload-file ios/altstore-source.json \
           "https://pypi.pixels-it.ru/repository/naivecdn/altstore-source.json"
  artifacts:
    paths:
      - build/ipa/NaiveCdnApp.ipa
```

---

## 8. Распространение через AltStore

### AltStore Source JSON

Создать файл `ios/altstore-source.json` и хостить на Nexus:

```json
{
  "name": "PixelProtocol",
  "identifier": "com.pixels-it.altstore-source",
  "sourceURL": "https://pypi.pixels-it.ru/repository/naivecdn/altstore-source.json",
  "apps": [
    {
      "name": "NaiveCDN",
      "bundleIdentifier": "com.pixels-it.naivecdnapp",
      "developerName": "Pixels IT",
      "subtitle": "CDN-совместимый прокси-клиент",
      "localizedDescription": "Клиент протокола NaiveCDN. Туннелирует трафик через CDN с помощью уTLS Chrome-fingerprint и HTTP/2.",
      "iconURL": "https://pypi.pixels-it.ru/repository/naivecdn/icon.png",
      "tintColor": "4A90D9",
      "category": "utilities",
      "versions": [
        {
          "version": "1.0.0",
          "date": "2026-06-24",
          "downloadURL": "https://pypi.pixels-it.ru/repository/naivecdn/v1.0.0/NaiveCdnApp.ipa",
          "localizedDescription": "Первый релиз",
          "size": 15000000,
          "minOSVersion": "16.0"
        }
      ]
    }
  ]
}
```

### Подключение источника в AltStore

На устройстве пользователя:
1. Открыть **AltStore** → вкладка **Browse**
2. Нажать **+** → вставить URL источника:
   ```
   https://pypi.pixels-it.ru/repository/naivecdn/altstore-source.json
   ```
3. Найти **NaiveCDN** → Install

---

## 9. Сводная таблица

| Этап | Инструмент | Артефакт |
|---|---|---|
| Go → iOS library | `gomobile bind` | `NaiveCdnGo.xcframework` |
| iOS App + Extension | Xcode 15 | `.xcarchive` |
| IPA экспорт | `xcodebuild -exportArchive` | `NaiveCdnApp.ipa` |
| Дистрибуция | AltStore Source на Nexus | `altstore-source.json` |
| CI/CD | GitLab (macOS runner) | auto на тег `vX.Y.Z` |

---

## 10. Замечания

- **TUN-исключение для CDN**: резолвить IP домена CDN до поднятия TUN
  и прописывать в `excludedRoutes` — CDN использует GeoDNS, IP меняется
  в зависимости от провайдера.
- **Entitlement `packet-tunnel-provider`**: требует платного Apple
  Developer аккаунта и одобрения в provisioning profile. Без него
  Network Extension не запустится даже через AltStore.
- **macOS runner для CI**: сборка iOS возможна только на macOS.
  Self-hosted runner на Mac Mini или GitHub Actions macOS runner
  (платный минуты).
- **tun2socks**: для полноценного IP → TCP/UDP роутинга рекомендуется
  встроить [tun2socks](https://github.com/xjasonlyu/tun2socks) как
  Go-зависимость в gomobile-модуль — он уже написан на Go и легко
  добавляется в тот же xcframework.
