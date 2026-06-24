# NaiveCDN Binary Distribution via Sonatype Nexus

Централизованное автообновление бинарника `naivecdn` через GitLab CI/CD
и Sonatype Nexus Repository (Raw hosted) на `pypi.pixels-it.ru` —
для Android SDK, Tauri (Win/macOS) и NuxtJS.

---

## Архитектура

```
gitlab.pixels-it.ru/pixelservices/pixelprotocol
│
└── .gitlab-ci.yml          ← билд-матрица по тегу vX.Y.Z
        │
        │  PUT (Basic Auth)
        ▼
pypi.pixels-it.ru  (Sonatype Nexus — Raw hosted repo "naivecdn")
    repository/naivecdn/
        {version}/
            naivecdn-linux-arm64
            naivecdn-linux-arm64.sha256
            naivecdn-windows-amd64.exe
            naivecdn-windows-amd64.exe.sha256
            naivecdn-darwin-amd64
            naivecdn-darwin-amd64.sha256
            naivecdn-darwin-arm64
            naivecdn-darwin-arm64.sha256
            version.json
        latest/
            version.json     ← всегда указывает на последний релиз
```

Клиенты (Android / Tauri / NuxtJS) при старте:
1. Запрашивают `https://pypi.pixels-it.ru/repository/naivecdn/latest/version.json`
2. Сравнивают `version` с локальной → если новее, скачивают бинарник
3. Проверяют SHA-256 → заменяют → перезапускают процесс

---

## 1. Настройка Sonatype Nexus

### Создание Raw hosted репозитория

В веб-интерфейсе Nexus (`https://pypi.pixels-it.ru`):

```
Administration → Repositories → Create repository → raw (hosted)
  Name:          naivecdn
  Deployment:    Allow redeploy   ← разрешить перезапись latest/
  Blob store:    default
```

### Учётные данные для CI

В GitLab (`Settings → CI/CD → Variables`) добавить:

| Variable | Value | Protected | Masked |
|---|---|---|---|
| `NEXUS_USER` | deploy-user | ✓ | — |
| `NEXUS_PASSWORD` | ••••• | ✓ | ✓ |

Пользователь Nexus должен иметь роль `nx-repository-view-raw-naivecdn-*`
(или `nx-admin` для простоты).

---

## 2. GitLab CI/CD Pipeline

### `.gitlab-ci.yml`

```yaml
stages:
  - build
  - publish

variables:
  GO_VERSION: "1.22"
  NEXUS_RAW: "https://pypi.pixels-it.ru/repository/naivecdn"

.build_template: &build_template
  stage: build
  image: golang:${GO_VERSION}
  before_script:
    - apt-get update -qq && apt-get install -y -qq curl
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
    - tags    # только при пуше тега vX.Y.Z
  needs:
    - build:linux-arm64
    - build:windows-amd64
    - build:darwin-amd64
    - build:darwin-arm64
  script:
    - VERSION=${CI_COMMIT_TAG}
    - VERSIONED="${NEXUS_RAW}/${VERSION}"
    # Загружаем бинарники и sha256-файлы в Nexus
    - |
      for f in dist/*; do
        name=$(basename "$f")
        curl --fail --silent \
             -u "${NEXUS_USER}:${NEXUS_PASSWORD}" \
             --upload-file "$f" \
             "${VERSIONED}/${name}"
      done
    # Генерируем version.json
    - |
      cat > dist/version.json <<EOF
      {
        "version": "${VERSION}",
        "date": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
        "binaries": {
          "linux-arm64":   { "url": "${VERSIONED}/naivecdn-linux-arm64",       "sha256": "$(cat dist/naivecdn-linux-arm64.sha256)" },
          "windows-amd64": { "url": "${VERSIONED}/naivecdn-windows-amd64.exe", "sha256": "$(cat dist/naivecdn-windows-amd64.exe.sha256)" },
          "darwin-amd64":  { "url": "${VERSIONED}/naivecdn-darwin-amd64",      "sha256": "$(cat dist/naivecdn-darwin-amd64.sha256)" },
          "darwin-arm64":  { "url": "${VERSIONED}/naivecdn-darwin-arm64",      "sha256": "$(cat dist/naivecdn-darwin-arm64.sha256)" }
        }
      }
      EOF
    # Кладём version.json рядом с версионными бинарниками
    - curl --fail --silent -u "${NEXUS_USER}:${NEXUS_PASSWORD}" \
           --upload-file dist/version.json "${VERSIONED}/version.json"
    # Перезаписываем latest/ (Allow redeploy включён в репо)
    - curl --fail --silent -u "${NEXUS_USER}:${NEXUS_PASSWORD}" \
           --upload-file dist/version.json "${NEXUS_RAW}/latest/version.json"
    # Создаём GitLab Release со ссылками на Nexus
    - |
      curl --fail --silent \
           --header "JOB-TOKEN: ${CI_JOB_TOKEN}" \
           --header "Content-Type: application/json" \
           --data "{
             \"name\": \"NaiveCDN ${VERSION}\",
             \"tag_name\": \"${VERSION}\",
             \"description\": \"Auto-release ${VERSION}\",
             \"assets\": { \"links\": [
               {\"name\": \"linux-arm64\",   \"url\": \"${VERSIONED}/naivecdn-linux-arm64\"},
               {\"name\": \"windows-amd64\", \"url\": \"${VERSIONED}/naivecdn-windows-amd64.exe\"},
               {\"name\": \"darwin-amd64\",  \"url\": \"${VERSIONED}/naivecdn-darwin-amd64\"},
               {\"name\": \"darwin-arm64\",  \"url\": \"${VERSIONED}/naivecdn-darwin-arm64\"}
             ]}
           }" \
           "${CI_API_V4_URL}/projects/${CI_PROJECT_ID}/releases"
```

> **Триггер релиза:** `git tag v1.0.0 && git push gitlab v1.0.0`

---

## 3. Android SDK

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
        "https://pypi.pixels-it.ru/repository/naivecdn/latest/version.json"
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

### `Application.onCreate`

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

### `AndroidManifest.xml`

```xml
<uses-permission android:name="android.permission.INTERNET" />
<uses-permission android:name="android.permission.WRITE_EXTERNAL_STORAGE" />
```

### VPN bypass

```kotlin
vpnService.protect(socket.fd)
// или маршрут bypass для IP сервера:
vpnBuilder.addRoute("0.0.0.0", 0)
vpnBuilder.excludeRoute("151.236.112.29", 32)
```

---

## 4. Tauri (Windows / macOS)

### `src-tauri/src/updater.rs`

```rust
use reqwest::Client;
use serde::Deserialize;
use sha2::{Digest, Sha256};
use std::{fs, path::PathBuf};

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

const VERSION_URL: &str =
    "https://pypi.pixels-it.ru/repository/naivecdn/latest/version.json";

pub async fn check_and_update(sidecar_path: PathBuf, current_version: &str) -> anyhow::Result<bool> {
    let client = Client::new();
    let manifest: Manifest = client.get(VERSION_URL).send().await?.json().await?;

    if manifest.version == current_version {
        return Ok(false);
    }

    #[cfg(target_os = "macos")]
    let platform = if cfg!(target_arch = "aarch64") { "darwin-arm64" } else { "darwin-amd64" };
    #[cfg(target_os = "windows")]
    let platform = "windows-amd64";

    let info = manifest.binaries.get(platform)
        .ok_or_else(|| anyhow::anyhow!("no binary for platform"))?;
    let bytes = client.get(&info.url).send().await?.bytes().await?;

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

### `tauri.conf.json`

```json
{
  "tauri": {
    "bundle": {
      "externalBin": ["binaries/naivecdn"]
    }
  }
}
```

> Tauri ожидает файлы вида `binaries/naivecdn-x86_64-apple-darwin`.
> Именование по Triple-target: `ARCH-VENDOR-OS`.

### Вызов из команды

```rust
#[tauri::command]
async fn start_proxy(app: tauri::AppHandle) -> Result<(), String> {
    let binary = app.path_resolver()
        .resolve_resource("binaries/naivecdn")
        .expect("binary not found");

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

## 5. NuxtJS (SSR-сервер)

### `server/plugins/naivecdn.ts`

```typescript
import { spawn, ChildProcess } from "child_process";
import { createWriteStream, chmodSync, existsSync } from "fs";
import { join } from "path";
import https from "https";

const VERSION_URL =
  "https://pypi.pixels-it.ru/repository/naivecdn/latest/version.json";

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

## 6. Сводная таблица

| Платформа        | Бинарник                     | Механизм обновления       | Запуск           |
|------------------|------------------------------|---------------------------|------------------|
| Android ARM64    | `naivecdn-linux-arm64`       | Kotlin coroutine + SHA-256 | `ProcessBuilder` |
| macOS Intel      | `naivecdn-darwin-amd64`      | Rust `reqwest` + SHA-256  | Tauri sidecar    |
| macOS Apple      | `naivecdn-darwin-arm64`      | Rust `reqwest` + SHA-256  | Tauri sidecar    |
| Windows x64      | `naivecdn-windows-amd64.exe` | Rust `reqwest` + SHA-256  | Tauri sidecar    |
| Linux/Server SSR | `naivecdn-linux-arm64`       | Node.js `https` + chmod   | `child_process`  |

---

## 7. Выпуск новой версии

```bash
git tag v1.1.0
git push gitlab v1.1.0
```

GitLab CI автоматически:
1. Кросс-компилирует все 4 платформы.
2. Загружает бинарники и `.sha256` в Nexus по пути `/{version}/`.
3. Перезаписывает `latest/version.json` в Nexus.
4. Создаёт GitLab Release со ссылками на Nexus.

Клиенты при следующем старте увидят новую версию в `version.json` и
скачают обновлённый бинарник с `pypi.pixels-it.ru`.

---

## 8. Безопасность

- **Публичный доступ на чтение**: в Nexus выдать анонимному пользователю
  роль `nx-repository-view-raw-naivecdn-read` — клиентам не нужны учётные
  данные для скачивания.
- **Запись только из CI**: переменные `NEXUS_USER` / `NEXUS_PASSWORD`
  помечены `Protected` в GitLab — недоступны в ветках, только в тегах.
- **SHA-256**: клиенты проверяют контрольную сумму перед заменой бинарника.
- **Pinning**: хранить минимальную допустимую версию в коде клиента —
  не откатываться ниже неё.
- **GPG-подпись** (опционально): подписывать бинарники в CI и
  проверять подпись до запуска.
