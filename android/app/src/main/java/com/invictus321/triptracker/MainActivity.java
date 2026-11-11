package com.invictus321.triptracker;

import android.Manifest;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.ActivityNotFoundException;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.provider.Settings;
import android.text.Editable;
import android.text.TextWatcher;
import android.text.format.DateUtils;
import android.widget.Button;
import android.widget.EditText;
import android.widget.TextView;
import android.widget.Toast;

import java.text.DateFormat;
import java.util.ArrayList;
import java.util.Date;
import java.util.List;

public class MainActivity extends Activity {
    private static final int REQ_START = 1;
    private static final int REQ_BACKGROUND = 2;

    private final Handler ui = new Handler(Looper.getMainLooper());
    private final Runnable refresher = new Runnable() {
        @Override
        public void run() {
            refresh();
            ui.postDelayed(this, 1000);
        }
    };

    private SharedPreferences prefs;
    private EditText serverUrl;
    private EditText token;
    private Button start;
    private Button stop;
    private TextView status;
    private boolean askedBackground;
    private boolean askedBattery;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);
        prefs = Prefs.get(this);

        serverUrl = findViewById(R.id.server_url);
        token = findViewById(R.id.token);
        start = findViewById(R.id.start);
        stop = findViewById(R.id.stop);
        status = findViewById(R.id.status);

        serverUrl.setText(prefs.getString(Prefs.URL, ""));
        token.setText(prefs.getString(Prefs.TOKEN, ""));
        TextWatcher save = new TextWatcher() {
            @Override
            public void beforeTextChanged(CharSequence s, int start, int count, int after) {}

            @Override
            public void onTextChanged(CharSequence s, int start, int before, int count) {}

            @Override
            public void afterTextChanged(Editable s) {
                saveSettings();
            }
        };
        serverUrl.addTextChangedListener(save);
        token.addTextChangedListener(save);

        start.setOnClickListener(v -> onStartClicked());
        stop.setOnClickListener(v -> confirmStop());
        findViewById(R.id.reliability).setOnClickListener(v -> {
            askedBackground = false;
            askedBattery = false;
            ensureReliability(true);
        });
    }

    @Override
    protected void onResume() {
        super.onResume();
        ui.post(refresher);
    }

    @Override
    protected void onPause() {
        ui.removeCallbacks(refresher);
        super.onPause();
    }

    private void saveSettings() {
        prefs.edit()
                .putString(Prefs.URL, serverUrl.getText().toString().trim())
                .putString(Prefs.TOKEN, token.getText().toString().trim())
                .apply();
    }

    private void onStartClicked() {
        saveSettings();
        if (Prefs.normalizeBaseUrl(serverUrl.getText().toString()) == null) {
            serverUrl.setError("Enter the full map URL including http:// or https://, e.g. http://1.2.3.4:8080");
            serverUrl.requestFocus();
            return;
        }
        List<String> needed = new ArrayList<>();
        if (!granted(Manifest.permission.ACCESS_FINE_LOCATION)) {
            needed.add(Manifest.permission.ACCESS_FINE_LOCATION);
            needed.add(Manifest.permission.ACCESS_COARSE_LOCATION);
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU
                && !granted(Manifest.permission.POST_NOTIFICATIONS)) {
            needed.add(Manifest.permission.POST_NOTIFICATIONS);
        }
        if (needed.isEmpty()) {
            startTracking();
        } else {
            requestPermissions(needed.toArray(new String[0]), REQ_START);
        }
    }

    @Override
    public void onRequestPermissionsResult(int requestCode, String[] permissions, int[] grantResults) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults);
        if (requestCode == REQ_START) {
            if (granted(Manifest.permission.ACCESS_FINE_LOCATION)) {
                startTracking();
            } else if (granted(Manifest.permission.ACCESS_COARSE_LOCATION)) {
                Toast.makeText(this, "Only approximate location allowed – pins may be inaccurate",
                        Toast.LENGTH_LONG).show();
                startTracking();
            } else {
                new AlertDialog.Builder(this)
                        .setTitle("Location permission needed")
                        .setMessage("TripTracker can't share your journey without access to your location.")
                        .setPositiveButton("Open settings", (d, w) -> openAppSettings())
                        .setNegativeButton("Cancel", null)
                        .show();
            }
        } else if (requestCode == REQ_BACKGROUND) {
            ensureReliability(false);
        }
    }

    private void startTracking() {
        askedBackground = false;
        askedBattery = false;
        startForegroundService(new Intent(this, TrackingService.class).setAction(TrackingService.ACTION_START));
        ensureReliability(false);
    }

    private void confirmStop() {
        new AlertDialog.Builder(this)
                .setTitle("End journey?")
                .setMessage("The map will show that your journey has ended safely.")
                .setPositiveButton("End journey", (d, w) -> startForegroundService(
                        new Intent(this, TrackingService.class).setAction(TrackingService.ACTION_STOP)))
                .setNegativeButton("Keep tracking", null)
                .show();
    }

    /**
     * Asks for "Allow all the time" location and for exemption from battery optimisation, so that
     * Android does not pause tracking while the phone is locked.
     */
    private void ensureReliability(boolean explicit) {
        boolean hasForegroundLocation = granted(Manifest.permission.ACCESS_FINE_LOCATION)
                || granted(Manifest.permission.ACCESS_COARSE_LOCATION);
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q && hasForegroundLocation && !askedBackground
                && !granted(Manifest.permission.ACCESS_BACKGROUND_LOCATION)) {
            askedBackground = true;
            new AlertDialog.Builder(this)
                    .setTitle("Allow location all the time")
                    .setMessage("TripTracker keeps sending your location while the phone is locked. "
                            + "To stop Android from ever pausing it, choose \"Allow all the time\" on the next screen.")
                    .setPositiveButton("Continue", (d, w) -> requestPermissions(
                            new String[] {Manifest.permission.ACCESS_BACKGROUND_LOCATION}, REQ_BACKGROUND))
                    .setNegativeButton("Not now", (d, w) -> ensureReliability(explicit))
                    .show();
            return;
        }
        PowerManager pm = getSystemService(PowerManager.class);
        if (!askedBattery && !pm.isIgnoringBatteryOptimizations(getPackageName())) {
            askedBattery = true;
            new AlertDialog.Builder(this)
                    .setTitle("Turn off battery optimisation")
                    .setMessage("This lets TripTracker send your location every minute even when the phone is asleep.")
                    .setPositiveButton("Continue", (d, w) -> requestBatteryExemption())
                    .setNegativeButton("Not now", null)
                    .show();
            return;
        }
        if (explicit) {
            Toast.makeText(this, "Background location and battery settings look good", Toast.LENGTH_SHORT).show();
        }
    }

    @android.annotation.SuppressLint("BatteryLife")
    private void requestBatteryExemption() {
        try {
            startActivity(new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                    Uri.parse("package:" + getPackageName())));
        } catch (ActivityNotFoundException e) {
            startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
        }
    }

    private void openAppSettings() {
        startActivity(new Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                Uri.parse("package:" + getPackageName())));
    }

    private boolean granted(String permission) {
        return checkSelfPermission(permission) == PackageManager.PERMISSION_GRANTED;
    }

    private void refresh() {
        String state = prefs.getString(Prefs.STATE, Prefs.STATE_IDLE);
        boolean tracking = Prefs.STATE_TRACKING.equals(state);
        start.setEnabled(!tracking);
        stop.setEnabled(tracking);

        StringBuilder sb = new StringBuilder();
        if (tracking) {
            sb.append("● Tracking – journey in progress");
        } else if (Prefs.STATE_STOPPING.equals(state)) {
            sb.append("Ending journey – sending stop signal (retrying every minute)");
        } else {
            sb.append("Not tracking");
        }
        String detail = prefs.getString(Prefs.STATUS, "");
        if (!detail.isEmpty()) {
            sb.append("\n").append(detail);
        }
        long lastSent = prefs.getLong(Prefs.LAST_SENT, 0);
        if (lastSent > 0 && !Prefs.STATE_IDLE.equals(state)) {
            sb.append("\nLast delivered: ")
                    .append(DateFormat.getTimeInstance().format(new Date(lastSent)))
                    .append(" (")
                    .append(DateUtils.getRelativeTimeSpanString(lastSent, System.currentTimeMillis(),
                            DateUtils.SECOND_IN_MILLIS))
                    .append(")");
        }
        int queued = prefs.getInt(Prefs.QUEUED, 0);
        if (queued > 0) {
            sb.append("\nWaiting to send: ").append(queued);
        }
        status.setText(sb);
    }
}
