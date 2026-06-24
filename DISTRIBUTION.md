# NaiveCDN Binary Distribution via GitLab

Централизованное автообновление бинарника `naivecdn` через GitLab CI/CD,
Generic Package Registry и Release API — для Android SDK, Tauri (Win/macOS)
и NuxtJS.

---

## Архитектура

```
gitlab.pixels-it.ru/pixelservices/pixelprotocol
│
├── .gitlab-ci.yml          ← билд-матрица по тегу vX.Y.Z
│
├── GitLab Package Registry ← артефакты всех платформ + version.json
│   packages/generic/naivecdn/{version}/
│       naivecdn-linux-arm64
│       naivecdn-windows-amd64.exe
│       naivecdn-darwin-amd64
│       naivecdn-darwin-arm64
│       version.json
│
└── GitLab Releases         ← human-readable changelog + те же ссылки
```

Клиенты (Android / Tauri / NuxtJS) при старте:
1. Запрашивают `version.json` → сравнивают с локальной версией.
2. Если есть обновление → скачивают платформенный бинарник.
3. Проверяют SHA-256 → заменяют → перезапускают процесс.

---

## 1. GitLab CI/CD Pipeline

### `.gitlab-ci.yml`

```yaml
stages:
  - build
  - publish

variables:
  GO_VERSION: "1.22"
  PACKAGE_NAME: "naivecdn"
  REGISTRY_URL: "${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/packages/generic/${PACKAGE_NAME}"

# Матрица платформ
.build_template: &build_template
  stage: build
  image: golang:${GO_VERSION}
  before_script:
    - apt-get update -qq && apt-get install -y -qq curl
    # Зеркало модулей недоступно — берём из кеша или вендора
    - cd go && go mod download 2>/dev/null || true
  artifacts:
    paths:
      - dist/
    expire_in: 1 hour

build:linux-arm64:
  <<: *build_template
  variables:
    GOOS: linux
    GOARCH: arm64
    OUT: naivecdn-linux-arm64
  script:
    - mkdir -p dist
    - cd go && GOOS=$GOOS GOARCH=$GOARCH go build -ldflags "-X main.Version=${CI_COMMIT_TAG}" -o ../dist/$OUT ./cmd/naivecdn
    - sha256sum dist/$OUT | awk '{print $1}' > dist/$OUT.sha256

build:windows-amd64:
  <<: *build_template
  variables:
    GOOS: windows
    GOARCH: amd64
    OUT: naivecdn-windows-amd64.exe
  script:
    - mkdir -p dist
    - cd go && GOOS=$GOOS GOARCH=$GOARCH go build -ldflags "-X main.Version=${CI_COMMIT_TAG}" -o ../dist/$OUT ./cmd/naivecdn
    - sha256sum dist/$OUT | awk '{print $1}' > dist/$OUT.sha256

build:darwin-amd64:
  <<: *build_template
  variables:
    GOOS: darwin
    GOARCH: amd64
    OUT: naivecdn-darwin-amd64
  script:
    - mkdir -p dist
    - cd go && GOOS=$GOOS GOARCH=$GOARCH go build -ldflags "-X main.Version=${CI_COMMIT_TAG}" -o ../dist/$OUT ./cmd/naivecdn
    - sha256sum dist/$OUT | awk '{print $1}' > dist/$OUT.sha256

build:darwin-arm64:
  <<: *build_template
  variables:
    GOOS: darwin
    GOARCH: arm64
    OUT: naivecdn-darwin-arm64
  script:
    - mkdir -p dist
    - cd go && GOOS=$GOOS GOARCH=$GOARCH go build -ldflags "-X main.Version=${CI_COMMIT_TAG}" -o ../dist/$OUT ./cmd/naivecdn
    - sha256sum dist/$OUT | awk '{print $1}' > dist/$OUT.sha256

publish:
  stage: publish
  image: curlimages/curl:latest
  only:
    - tags   # только при пуше тега vX.Y.Z
  needs:
    - build:linux-arm64
    - build:windows-amd64
    - build:darwin-amd64
    - build:darwin-arm64
  script:
    - VERSION=${CI_COMMIT_TAG}
    - BASE="${REGISTRY_URL}/${VERSION}"
    # Загружаем бинарники
    - |
      for f in dist/*; do
        name=$(basename $f)
        curl --fail --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
             --upload-file "$f" "${BASE}/${name}"
      done
    # Генерируем version.json
    - |
      cat > dist/version.json <<EOF
      {
        "version": "${VERSION}",
        "date": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
        "binaries": {
          "linux-arm64":    { "url": "${BASE}/naivecdn-linux-arm64",       "sha256": "$(cat dist/naivecdn-linux-arm64.sha256)" },
          "windows-amd64":  { "url": "${BASE}/naivecdn-windows-amd64.exe", "sha256": "$(cat dist/naivecdn-windows-amd64.exe.sha256)" },
          "darwin-amd64":   { "url": "${BASE}/naivecdn-darwin-amd64",      "sha256": "$(cat dist/naivecdn-darwin-amd64.sha256)" },
          "darwin-arm64":   { "url": "${BASE}/naivecdn-darwin-arm64",      "sha256": "$(cat dist/naivecdn-darwin-arm64.sha256)" }
        }
      }
      EOF
    - curl --fail --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
           --upload-file dist/version.json "${BASE}/version.json"
    # Также кладём как "latest"
    - curl --fail --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
           --upload-file dist/version.json "${REGISTRY_URL}/latest/version.json"
    # Создаём GitLab Release
    - |
      curl --fail --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
           --header "Content-Type: application/json" \
           --data "{
             \"name\": \"NaiveCDN ${VERSION}\",
             \"tag_name\": \"${VERSION}\",
             \"description\": \"Auto-release ${VERSION}\",
             \"assets\": { \"links\": [
               {\"name\": \"linux-arm64\",   \"url\": \"${BASE}/naivecdn-linux-arm64\"},
               {\"name\": \"windows-amd64\", \"url\": \"${BASE}/naivecdn-windows-amd64.exe\"},
               {\"name\": \"darwin-amd64\",  \"url\": \"${BASE}/naivecdn-darwin-amd64\"},
               {\"name\": \"darwin-arm64\",  \"url\": \"${BASE}/naivecdn-darwin-arm64\"}
             ]}
           }" \
           "${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/releases"
```

> **Триггер релиза:** `git tag v1.0.0 && git push gitlab v1.0.0`

---

## 2. Android SDK

### Подход

Бинарник запускается как дочерний процесс (как в Wireguard/Shadowsocks Go
сборках). Внешний процесс изолирует VPN-логику от основного потока JVM.

### `NaiveCdnUpdater.kt`

```kotlin
import android.content.Context
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.File
import java.net.URL
import java.security.MessageDigest

object NaiveCdnUpdater {

    private const val VERSION_URL =
        "https://gitlab.pixels-it.ru/api/v4/projects/<PROJECT_ID>/packages/generic/naivecdn/latest/version.json"
    private const val PLATFORM = "linux-arm64"   // Android ARM64

    suspend fun checkAndUpdate(ctx: Context): Boolean = withContext(Dispatchers.IO) {
        val manifest = fetchJson(VERSION_URL) ?: return@withContext false
        val remote = manifest.getString("version")
        val localVersion = ctx.getSharedPreferences("cdn", Context.MODE_PRIVATE)
            .getString("version", "") ?: ""

        if (remote == localVersion) return@withContext false

        val binaryInfo = manifest.getJSONObject("binaries").getJSONObject(PLATFORM)
        val url = binaryInfo.getString("url")
        val expectedSha = binaryInfo.getString("sha256")

        val dest = File(ctx.filesDir, "naivecdn")
        download(url, dest)

        if (!verifySha256(dest, expectedSha)) {
            dest.delete()
            return@withContext false
        }

        dest.setExecutable(true, true)
        ctx.getSharedPreferences("cdn", Context.MODE_PRIVATE)
            .edit().putString("version", remote).apply()

        true
    }

    private fun fetchJson(url: String): JSONObject? = runCatching {
        JSONObject(URL(url).readText())
    }.getOrNull()

    private fun download(url: String, dest: File) {
        URL(url).openStream().use { input ->
            dest.outputStream().use { output -> input.copyTo(output) }
        }
    }

    private fun verifySha256(file: File, expected: String): Boolean {
        val digest = MessageDigest.getInstance("SHA-256")
        file.inputStream().use { digest.update(it.readBytes()) }
        val actual = digest.digest().joinToString("") { "%02x".format(it) }
        return actual == expected
    }
}
```

### `NaiveCdnProcess.kt` — запуск бинарника

```kotlin
import android.content.Context
import java.io.File

class NaiveCdnProcess(private val ctx: Context) {

    private var process: Process? = null

    fun start(configJson: String) {
        val binary = File(ctx.filesDir, "naivecdn")
        val config = File(ctx.filesDir, "naivecdn.json").also { it.writeText(configJson) }

        process = ProcessBuilder(binary.absolutePath, "--config", config.absolutePath)
            .redirectErrorStream(true)
            .start()

        // Логи в logcat
        Thread {
            process?.inputStream?.bufferedReader()?.forEachLine { android.util.Log.d("NaiveCDN", it) }
        }.start()
    }

    fun stop() {
        process?.destroy()
        process = null
    }

    val isRunning get() = process?.isAlive == true
}
```

### `Application.onCreate` (точка входа)

```kotlin
class App : Application() {
    override fun onCreate() {
        super.onCreate()
        CoroutineScope(Dispatchers.IO).launch {
            NaiveCdnUpdater.checkAndUpdate(this@App)
        }
    }
}
```

### `AndroidManifest.xml` — разрешения

```xml
<uses-permission android:name="android.permission.INTERNET" />
<uses-permission android:name="android.permission.WRITE_EXTERNAL_STORAGE" />
```

### VPN bypass (если используется VpnService)

```kotlin
// Перед установкой туннеля защитить сокет бинарника:
// (передаётся через Unix socket или ioctl из бинарника)
vpnService.protect(socket.fd)
// или добавить bypass-маршрут через Builder:
vpnBuilder.addRoute("0.0.0.0", 0)
vpnBuilder.excludeRoute("151.236.112.29", 32) // IP CDN-сервера
```

---

## 3. Tauri (Windows / macOS)

### Подход

Tauri поддерживает sidecar-бинарники (команда `tauri.conf.json` →
`bundle.externalBin`). Автообновление sidecar — через кастомный updater
(встроенный updater обновляет только сам `.app`, не sidecar).

### `src-tauri/src/updater.rs`

```rust
use reqwest::Client;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{fs, io::Write, path::PathBuf};

#[derive(Deserialize)]
struct Manifest {
    version: String,
    binaries: std::collections::HashMap<String, BinaryInfo>,
}

#[derive(Deserialize)]
struct BinaryInfo {
    url: String,
    sha256: String,
}

pub async fn check_and_update(sidecar_path: PathBuf, current_version: &str) -> anyhow::Result<bool> {
    let client = Client::new();
    let manifest: Manifest = client
        .get("https://gitlab.pixels-it.ru/api/v4/projects/<PROJECT_ID>/packages/generic/naivecdn/latest/version.json")
        .send().await?
        .json().await?;

    if manifest.version == current_version {
        return Ok(false);
    }

    #[cfg(target_os = "macos")]
    let platform = if cfg!(target_arch = "aarch64") { "darwin-arm64" } else { "darwin-amd64" };
    #[cfg(target_os = "windows")]
    let platform = "windows-amd64";

    let info = manifest.binaries.get(platform).ok_or(anyhow::anyhow!("no binary for platform"))?;
    let bytes = client.get(&info.url).send().await?.bytes().await?;

    // SHA-256 проверка
    let mut hasher = Sha256::new();
    hasher.update(&bytes);
    let actual = format!("{:x}", hasher.finalize());
    anyhow::ensure!(actual == info.sha256, "SHA-256 mismatch");

    let tmp = sidecar_path.with_extension("tmp");
    fs::write(&tmp, &bytes)?;

    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&tmp, fs::Permissions::from_mode(0o755))?;
    }

    fs::rename(tmp, &sidecar_path)?;
    Ok(true)
}
```

### `tauri.conf.json` — регистрация sidecar

```json
{
  "tauri": {
    "bundle": {
      "externalBin": ["binaries/naivecdn"]
    }
  }
}
```

> Tauri ожидает файлы вида `binaries/naivecdn-x86_64-apple-darwin` и т.д.
> Именование по Triple-target: `ARCH-VENDOR-OS`.

### Вызов из Tauri команды

```rust
#[tauri::command]
async fn start_proxy(app: tauri::AppHandle) -> Result<(), String> {
    let binary = app.path_resolver()
        .resolve_resource("binaries/naivecdn")
        .expect("binary not found");

    // Обновить перед стартом
    let version = std::env::var("NAIVECDN_VERSION").unwrap_or_default();
    let _ = check_and_update(binary.clone(), &version).await;

    tauri::api::process::Command::new_sidecar("naivecdn")
        .map_err(|e| e.to_string())?
        .args(["--config", "config.json"])
        .spawn()
        .map_err(|e| e.to_string())?;
    Ok(())
}
```

---

## 4. NuxtJS (десктоп/сервер)

### Подход А — Nuxt как UI, бинарник на том же хосте (Electron-like / Tauri)

Если NuxtJS работает внутри Tauri — используй раздел выше.

### Подход Б — Nuxt SSR-сервер + naivecdn как sidecar процесс

```
┌──────────────────────────────┐
│  Nuxt SSR (Node.js)          │
│  server/plugins/naivecdn.ts  │──spawn──► naivecdn --config ...
│  server/api/proxy-status.ts  │           SOCKS5 :1080
└──────────────────────────────┘
```

### `server/plugins/naivecdn.ts`

```typescript
import { spawn, ChildProcess } from "child_process";
import { createWriteStream, chmodSync, existsSync } from "fs";
import { join } from "path";
import https from "https";

const VERSION_URL =
  "https://gitlab.pixels-it.ru/api/v4/projects/<PROJECT_ID>/packages/generic/naivecdn/latest/version.json";

let cdnProcess: ChildProcess | null = null;
let currentVersion = "";

async function fetchJson(url: string): Promise<any> {
  return new Promise((resolve, reject) => {
    https.get(url, (res) => {
      let data = "";
      res.on("data", (chunk) => (data += chunk));
      res.on("end", () => resolve(JSON.parse(data)));
    }).on("error", reject);
  });
}

async function downloadFile(url: string, dest: string): Promise<void> {
  return new Promise((resolve, reject) => {
    const file = createWriteStream(dest);
    https.get(url, (res) => {
      res.pipe(file);
      file.on("finish", () => { file.close(); resolve(); });
    }).on("error", reject);
  });
}

function platform(): string {
  if (process.platform === "darwin")
    return process.arch === "arm64" ? "darwin-arm64" : "darwin-amd64";
  if (process.platform === "win32") return "windows-amd64";
  return "linux-arm64";
}

async function updateBinary(binPath: string): Promise<boolean> {
  const manifest = await fetchJson(VERSION_URL);
  if (manifest.version === currentVersion) return false;

  const info = manifest.binaries[platform()];
  await downloadFile(info.url, binPath);
  chmodSync(binPath, 0o755);
  currentVersion = manifest.version;
  return true;
}

export default defineNitroPlugin(async () => {
  const binPath = join(process.cwd(), "bin", "naivecdn");

  if (!existsSync(binPath) || await updateBinary(binPath)) {
    console.log(`[naivecdn] binary updated to ${currentVersion}`);
  }

  cdnProcess = spawn(binPath, ["--config", "naivecdn.json"], {
    stdio: ["ignore", "pipe", "pipe"],
  });
  cdnProcess.stdout?.on("data", (d) => console.log("[naivecdn]", d.toString()));
  cdnProcess.stderr?.on("data", (d) => console.error("[naivecdn]", d.toString()));
  cdnProcess.on("exit", (code) => console.warn(`[naivecdn] exited ${code}`));
});
```

### `server/api/proxy-status.get.ts`

```typescript
export default defineEventHandler(() => ({
  running: cdnProcess?.exitCode === null,
  version: currentVersion,
  socks5: "127.0.0.1:1080",
}));
```

### `nuxt.config.ts`

```typescript
export default defineNuxtConfig({
  nitro: {
    plugins: ["~/server/plugins/naivecdn.ts"],
  },
});
```

---

## 5. Сводная таблица

| Платформа        | Бинарник             | Механизм обновления         | Запуск           |
|------------------|----------------------|-----------------------------|------------------|
| Android ARM64    | `naivecdn-linux-arm64` | Kotlin coroutine + SHA-256  | `ProcessBuilder` |
| macOS Intel      | `naivecdn-darwin-amd64` | Rust `reqwest` + SHA-256   | Tauri sidecar    |
| macOS Apple      | `naivecdn-darwin-arm64` | Rust `reqwest` + SHA-256   | Tauri sidecar    |
| Windows x64      | `naivecdn-windows-amd64.exe` | Rust `reqwest` + SHA-256 | Tauri sidecar  |
| Linux/Server SSR | `naivecdn-linux-arm64` | Node.js `https` + chmod     | `child_process`  |

---

## 6. Выпуск новой версии

```bash
# В репозитории pixelprotocol
git tag v1.1.0
git push gitlab v1.1.0
```

GitLab CI автоматически:
1. Кросс-компилирует все 4 платформы.
2. Загружает бинарники в Package Registry.
3. Обновляет `latest/version.json`.
4. Создаёт GitLab Release.

Клиенты при следующем старте увидят новую версию в `version.json` и
скачают обновлённый бинарник.

---

## 7. Безопасность

- **Токен скачивания**: Package Registry можно сделать публичным (без
  токена) или выдать `Deploy Token` с правами `read_package_registry`.
- **Подпись**: дополнительно можно подписывать бинарники GPG-ключом и
  проверять подпись перед запуском.
- **Pinning**: хранить минимальную версию в коде клиента — не
  откатываться ниже неё даже если сервер отдаёт старый манифест.
