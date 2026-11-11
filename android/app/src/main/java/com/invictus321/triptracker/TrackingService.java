package com.invictus321.triptracker;

import android.annotation.SuppressLint;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ServiceInfo;
import android.location.Location;
import android.location.LocationListener;
import android.location.LocationManager;
import android.os.BatteryManager;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.HandlerThread;
import android.os.IBinder;
import android.os.PowerManager;
import android.os.SystemClock;
import android.util.Log;

import org.json.JSONException;
import org.json.JSONObject;

import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.text.DateFormat;
import java.util.ArrayList;
import java.util.Date;
import java.util.List;
import java.util.UUID;

/**
 * Foreground service that sends the phone's location to the map server every minute until
 * the journey is stopped. It holds a partial wake lock so the minute timer keeps firing while
 * the screen is off, and keeps unsent requests in a persistent {@link Outbox} that is retried
 * every minute until the server accepts them.
 */
public class TrackingService extends Service implements LocationListener {
    static final String ACTION_START = "com.invictus321.triptracker.START";
    static final String ACTION_STOP = "com.invictus321.triptracker.STOP";

    private static final String TAG = "TrackingService";
    private static final long INTERVAL_MS = 60_000L;
    private static final long FIX_INTERVAL_MS = 20_000L;
    private static final long MAX_LAST_KNOWN_AGE_MS = 10 * 60_000L;
    private static final int HTTP_TIMEOUT_MS = 15_000;
    private static final String CHANNEL_ID = "tracking";
    private static final int NOTIFICATION_ID = 1;

    private final Runnable tick = this::tick;

    private SharedPreferences prefs;
    private Outbox outbox;
    private LocationManager locationManager;
    private PowerManager.WakeLock wakeLock;
    private HandlerThread thread;
    private Handler handler;
    private volatile int lastStartId;

    // Only touched on the handler thread.
    private Location best;
    private boolean listening;
    private boolean sentFirstFix;

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    @Override
    public void onCreate() {
        super.onCreate();
        prefs = Prefs.get(this);
        outbox = new Outbox(prefs);
        locationManager = getSystemService(LocationManager.class);
        wakeLock = getSystemService(PowerManager.class)
                .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "TripTracker:tracking");
        wakeLock.setReferenceCounted(false);
        getSystemService(NotificationManager.class).createNotificationChannel(
                new NotificationChannel(CHANNEL_ID, "Journey tracking", NotificationManager.IMPORTANCE_LOW));
        thread = new HandlerThread("TripTracker");
        thread.start();
        handler = new Handler(thread.getLooper());
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        lastStartId = startId;
        String action = intent == null ? null : intent.getAction();
        if (!enterForeground()) {
            stopSelf(startId);
            return START_NOT_STICKY;
        }
        wakeLock.acquire(3 * INTERVAL_MS);
        handler.post(() -> {
            if (ACTION_START.equals(action)) {
                beginJourney();
            } else if (ACTION_STOP.equals(action)) {
                endJourney();
            } else {
                resume(); // restarted by the system after being killed
            }
        });
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        handler.removeCallbacksAndMessages(null);
        locationManager.removeUpdates(this);
        if (wakeLock.isHeld()) {
            wakeLock.release();
        }
        thread.quitSafely();
        super.onDestroy();
    }

    private boolean enterForeground() {
        try {
            Notification n = buildNotification(prefs.getString(Prefs.STATUS, "Starting…"));
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
                startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_LOCATION);
            } else {
                startForeground(NOTIFICATION_ID, n);
            }
            return true;
        } catch (RuntimeException e) {
            Log.w(TAG, "could not enter foreground", e);
            prefs.edit().putString(Prefs.STATUS, "Could not start tracking: " + e.getMessage()).apply();
            return false;
        }
    }

    private void beginJourney() {
        if (Prefs.STATE_TRACKING.equals(state())) {
            resume();
            return;
        }
        enterForeground(); // in case a previous journey's shutdown raced with this start
        String id = UUID.randomUUID().toString().replace("-", "").substring(0, 16);
        prefs.edit()
                .putString(Prefs.JOURNEY, id)
                .putString(Prefs.STATE, Prefs.STATE_TRACKING)
                .putLong(Prefs.LAST_SENT, 0)
                .apply();
        best = freshLastKnown();
        sentFirstFix = false;
        outbox.add("/api/start", json("journeyId", id));
        setStatus("Journey started – waiting for GPS fix…");
        startLocationUpdates();
        handler.removeCallbacks(tick);
        tick();
    }

    private void endJourney() {
        if (Prefs.STATE_TRACKING.equals(state())) {
            enqueueLocation(); // final position
            stopLocationUpdates();
            outbox.add("/api/stop", json("journeyId", prefs.getString(Prefs.JOURNEY, "")));
            prefs.edit().putString(Prefs.STATE, Prefs.STATE_STOPPING).apply();
            setStatus("Sending stop signal…");
        }
        handler.removeCallbacks(tick);
        tick();
    }

    private void resume() {
        if (Prefs.STATE_TRACKING.equals(state())) {
            startLocationUpdates();
        }
        handler.removeCallbacks(tick);
        tick();
    }

    private void tick() {
        long startedAt = SystemClock.uptimeMillis();
        wakeLock.acquire(3 * INTERVAL_MS);
        boolean tracking = Prefs.STATE_TRACKING.equals(state());
        if (tracking && !enqueueLocation()) {
            setStatus("Waiting for GPS fix…");
        }
        flush();
        if (tracking || !outbox.isEmpty()) {
            // The wake lock keeps the CPU awake, so uptime-based scheduling stays on a one-minute cadence.
            handler.postAtTime(tick, startedAt + INTERVAL_MS);
        } else {
            finish();
        }
    }

    /** Queues the best available location. Returns false if there is no fix yet. */
    private boolean enqueueLocation() {
        Location loc = best != null ? best : freshLastKnown();
        if (loc == null) {
            return false;
        }
        JSONObject body = json("journeyId", prefs.getString(Prefs.JOURNEY, ""));
        try {
            body.put("lat", loc.getLatitude());
            body.put("lon", loc.getLongitude());
            body.put("time", loc.getTime());
            if (loc.hasAccuracy()) {
                body.put("accuracy", loc.getAccuracy());
            }
            int battery = getSystemService(BatteryManager.class)
                    .getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY);
            if (battery > 0 && battery <= 100) {
                body.put("battery", battery);
            }
        } catch (JSONException e) {
            throw new IllegalStateException(e);
        }
        outbox.add("/api/location", body);
        sentFirstFix = true;
        return true;
    }

    /** Sends queued requests in order, stopping at the first failure so it is retried next tick. */
    private void flush() {
        if (outbox.isEmpty()) {
            return;
        }
        String base = Prefs.normalizeBaseUrl(prefs.getString(Prefs.URL, ""));
        if (base == null) {
            setStatus("Set the server URL (starting http:// or https://) in the app – "
                    + outbox.size() + " update(s) waiting");
            return;
        }
        String token = prefs.getString(Prefs.TOKEN, "").trim();
        while (!outbox.isEmpty()) {
            JSONObject item = outbox.peek();
            int code;
            try {
                code = post(base + item.optString("path"), item.optJSONObject("body"), token);
            } catch (IOException e) {
                Log.i(TAG, "send failed: " + e);
                setStatus("Can't reach server (" + e.getClass().getSimpleName() + ": " + e.getMessage()
                        + ") – retrying every minute, " + outbox.size() + " waiting");
                return;
            }
            if (code >= 200 && code < 300) {
                outbox.removeFirst();
                prefs.edit().putLong(Prefs.LAST_SENT, System.currentTimeMillis()).apply();
            } else if (code == 401 || code == 403) {
                setStatus("Server rejected the access token – check it in the app");
                return;
            } else if (code >= 400 && code < 500 && code != 408 && code != 429) {
                Log.w(TAG, "server rejected request with " + code + ": " + item);
                outbox.removeFirst(); // malformed; retrying will never succeed
            } else {
                setStatus("Server error " + code + " – retrying every minute");
                return;
            }
        }
        if (Prefs.STATE_TRACKING.equals(state())) {
            setStatus("Location sent at " + DateFormat.getTimeInstance().format(new Date()));
        }
    }

    private static int post(String url, JSONObject body, String token) throws IOException {
        HttpURLConnection conn = (HttpURLConnection) new URL(url).openConnection();
        try {
            conn.setConnectTimeout(HTTP_TIMEOUT_MS);
            conn.setReadTimeout(HTTP_TIMEOUT_MS);
            conn.setRequestMethod("POST");
            conn.setDoOutput(true);
            conn.setRequestProperty("Content-Type", "application/json");
            if (!token.isEmpty()) {
                conn.setRequestProperty("Authorization", "Bearer " + token);
            }
            byte[] data = (body == null ? "{}" : body.toString()).getBytes(StandardCharsets.UTF_8);
            conn.setFixedLengthStreamingMode(data.length);
            try (OutputStream out = conn.getOutputStream()) {
                out.write(data);
            }
            int code = conn.getResponseCode();
            InputStream in = code >= 400 ? conn.getErrorStream() : conn.getInputStream();
            if (in != null) {
                try (InputStream drained = in) {
                    byte[] buf = new byte[1024];
                    while (drained.read(buf) != -1) {
                        // discard
                    }
                }
            }
            return code;
        } finally {
            conn.disconnect();
        }
    }

    private void finish() {
        stopLocationUpdates();
        handler.removeCallbacks(tick);
        SharedPreferences.Editor editor = prefs.edit().putString(Prefs.STATE, Prefs.STATE_IDLE);
        if (Prefs.STATE_STOPPING.equals(state())) {
            editor.putString(Prefs.STATUS, "Journey ended – the map shows you arrived safely.");
        }
        editor.apply();
        if (wakeLock.isHeld()) {
            wakeLock.release();
        }
        stopForeground(STOP_FOREGROUND_REMOVE);
        stopSelf(lastStartId);
    }

    @SuppressLint("MissingPermission")
    private void startLocationUpdates() {
        if (listening) {
            return;
        }
        List<String> available = locationManager.getAllProviders();
        for (String provider : providers()) {
            if (!available.contains(provider)) {
                continue;
            }
            try {
                locationManager.requestLocationUpdates(provider, FIX_INTERVAL_MS, 0f, this, handler.getLooper());
                listening = true;
            } catch (SecurityException | IllegalArgumentException e) {
                Log.w(TAG, "cannot use provider " + provider, e);
            }
        }
        if (!listening) {
            setStatus("Location permission missing – open TripTracker");
        }
    }

    private void stopLocationUpdates() {
        locationManager.removeUpdates(this);
        listening = false;
    }

    @SuppressLint("MissingPermission")
    private Location freshLastKnown() {
        Location newest = null;
        for (String provider : providers()) {
            try {
                Location loc = locationManager.getLastKnownLocation(provider);
                if (loc != null && (newest == null || loc.getTime() > newest.getTime())) {
                    newest = loc;
                }
            } catch (SecurityException | IllegalArgumentException e) {
                // provider missing or permission revoked
            }
        }
        if (newest != null && System.currentTimeMillis() - newest.getTime() <= MAX_LAST_KNOWN_AGE_MS) {
            return newest;
        }
        return null;
    }

    private static List<String> providers() {
        List<String> providers = new ArrayList<>();
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            providers.add(LocationManager.FUSED_PROVIDER);
        }
        providers.add(LocationManager.GPS_PROVIDER);
        providers.add(LocationManager.NETWORK_PROVIDER);
        return providers;
    }

    private static boolean isBetter(Location candidate, Location current) {
        if (current == null) {
            return true;
        }
        long delta = candidate.getTime() - current.getTime();
        if (delta > 30_000L) {
            return true;
        }
        if (delta < -30_000L) {
            return false;
        }
        float cand = candidate.hasAccuracy() ? candidate.getAccuracy() : Float.MAX_VALUE;
        float cur = current.hasAccuracy() ? current.getAccuracy() : Float.MAX_VALUE;
        return cand <= cur || (delta > 0 && cand <= cur + 25f);
    }

    @Override
    public void onLocationChanged(Location location) {
        if (isBetter(location, best)) {
            best = location;
        }
        // Send the first fix straight away rather than waiting for the next tick.
        if (!sentFirstFix && Prefs.STATE_TRACKING.equals(state())) {
            enqueueLocation();
            flush();
        }
    }

    // Implemented explicitly: these are abstract on Android 10 and below.
    @Override
    public void onProviderEnabled(String provider) {}

    @Override
    public void onProviderDisabled(String provider) {}

    @Override
    @SuppressWarnings("deprecation")
    public void onStatusChanged(String provider, int status, Bundle extras) {}

    private String state() {
        return prefs.getString(Prefs.STATE, Prefs.STATE_IDLE);
    }

    private void setStatus(String status) {
        prefs.edit().putString(Prefs.STATUS, status).apply();
        getSystemService(NotificationManager.class).notify(NOTIFICATION_ID, buildNotification(status));
    }

    private Notification buildNotification(String text) {
        String title = Prefs.STATE_STOPPING.equals(state())
                ? "Ending journey – sending stop signal"
                : "Journey in progress – sharing location";
        PendingIntent open = PendingIntent.getActivity(this, 0,
                new Intent(this, MainActivity.class).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP),
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        Notification.Builder builder = new Notification.Builder(this, CHANNEL_ID)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(title)
                .setContentText(text)
                .setContentIntent(open)
                .setOngoing(true)
                .setOnlyAlertOnce(true)
                .setCategory(Notification.CATEGORY_SERVICE);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            builder.setForegroundServiceBehavior(Notification.FOREGROUND_SERVICE_IMMEDIATE);
        }
        return builder.build();
    }

    private static JSONObject json(String key, String value) {
        JSONObject o = new JSONObject();
        try {
            o.put(key, value);
        } catch (JSONException e) {
            throw new IllegalStateException(e);
        }
        return o;
    }
}
