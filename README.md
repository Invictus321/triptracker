# triptracker

Live journey tracking: a web map with your current location (blue pin) and journey history (orange pins), fed by an Android app that reports your position every minute.

## How it works

| Part | What it does |
| --- | --- |
| `cmd/triptracker` | Go web server. It serves the live map and the location API, and saves the current journey to `data/journey.json`. |
| `web/static` | Leaflet/OpenStreetMap page. It gets updates instantly over Server-Sent Events, with a 30-second polling fallback. |
| `android/` | Lightweight Android app (plain Java, no AndroidX) that sends your location every minute. |

On the map:
- **Blue pin**: your latest location. Click it to get coordinates you can copy and paste (decimal and degrees/minutes/seconds) for emergency services.
- **Semi-transparent orange pins joined by a line**: the journey so far.
- **Status banner**:
  - Until a stop signal arrives, it shows *"Last update Xm Ys ago"*. It turns amber after 2½ minutes and red after 10 minutes.
  - After a stop signal, it shows *"✅ Journey ended safely"*.

## Running the server

```sh
TRIPTRACKER_TOKEN='choose-a-long-random-secret' go run ./cmd/triptracker -addr :8080 -data data/journey.json
```

Then open `http://localhost:8080`. The phone must be able to reach the server, so either:
- deploy it somewhere public behind HTTPS (recommended), or
- use your LAN IP (for example `http://192.168.1.10:8080`).

> `TRIPTRACKER_TOKEN` protects the endpoints that **write** data. The map itself is readable by anyone who has the URL.

### API

Write endpoints require `Authorization: Bearer <token>` (or `?token=`) when `TRIPTRACKER_TOKEN` is set.

| Method & path | Body | Purpose |
| --- | --- | --- |
| `POST /api/location` | `{"lat":-33.86,"lon":151.21,"accuracy":8,"battery":80,"time":1767261540000,"journeyId":"abc"}` | Store a location and make it the new blue pin. Only `lat`/`lon` are required. `time` is unix milliseconds or RFC 3339. Form/query parameters also work. |
| `POST /api/start` | `{"journeyId":"abc"}` | Start a new, empty journey. |
| `POST /api/stop` | `{"journeyId":"abc"}` | Mark the journey as deliberately ended. |
| `GET /api/state` | | Current journey as JSON. |
| `GET /api/events` | | Server-Sent Events stream of state changes. |

A location with a new `journeyId` (or with no ID after a stop) starts a new journey. Points that arrive late are sorted by when the fix was taken, and repeated sends of the same point are ignored.

```sh
curl -X POST -H "Authorization: Bearer $TRIPTRACKER_TOKEN" -d lat=-33.8688 -d lon=151.2093 http://localhost:8080/api/location
```

## Android app

The app has **Start** and **Stop** buttons.

**Start** runs a foreground location service:
- It keeps working while the app is in the background or the phone is locked. A persistent notification shows while it runs, and a partial wake lock keeps the timer going.
- Every minute it queues your location in a persistent outbox and sends it.
- If there's no signal, it retries every minute and sends everything it missed, in order, once it reconnects.

**Stop** asks for confirmation, then queues a stop signal. It keeps retrying every minute until the server confirms it.

Permissions requested:
- precise location, plus *Allow all the time* (optional but recommended)
- notifications
- foreground location service
- wake lock
- internet
- an exemption from battery optimisation

### Building the APK

**GitHub Actions (easiest).** Push this repo to GitHub. The **Android APK** workflow builds the app and uploads `triptracker.apk` as a run artifact. You can also start the workflow manually from the Actions tab.

**Locally.** You need JDK 17 and the Android SDK (platform 35). Point `ANDROID_HOME`, or `android/local.properties` (`sdk.dir=...`), at the SDK, then run:

```sh
cd android
./gradlew assembleRelease
# → android/app/build/outputs/apk/release/app-release.apk
```

The release APK is signed with the build machine's debug key so it can be side-loaded easily. APKs built on different machines have different signatures, so uninstall the old version before installing one built elsewhere.

### Installing on your phone

1. Copy the APK to the phone and open it. Allow *Install unknown apps* for your browser or file manager when asked.
2. Open **TripTracker** and enter the server URL and token.
3. Tap **Start journey**, and grant location (*Precise*) and notifications.
4. When prompted, choose **Allow all the time** and turn off battery optimisation.
5. On Samsung, Xiaomi, Huawei and similar phones, also set TripTracker's battery usage to *Unrestricted* or add it to *Never sleeping apps*, because they kill background apps aggressively.
