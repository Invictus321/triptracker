package com.invictus321.triptracker;

import android.content.Context;
import android.content.SharedPreferences;
import android.net.Uri;

/** Shared preference keys used by the activity and the tracking service. */
final class Prefs {
    static final String URL = "server_url";
    static final String TOKEN = "token";
    static final String STATE = "state";
    static final String JOURNEY = "journey_id";
    static final String OUTBOX = "outbox";
    static final String QUEUED = "queued";
    static final String STATUS = "status";
    static final String LAST_SENT = "last_sent";

    static final String STATE_IDLE = "idle";
    static final String STATE_TRACKING = "tracking";
    static final String STATE_STOPPING = "stopping";

    private Prefs() {}

    static SharedPreferences get(Context context) {
        return context.getSharedPreferences("triptracker", Context.MODE_PRIVATE);
    }

    /**
     * Returns the server base URL without a trailing slash, or null if it is unusable. The scheme
     * must be explicit: guessing https breaks plain-http servers, and guessing http leaks location.
     */
    static String normalizeBaseUrl(String raw) {
        if (raw == null) {
            return null;
        }
        String s = raw.trim();
        while (s.endsWith("/")) {
            s = s.substring(0, s.length() - 1);
        }
        if (s.isEmpty()) {
            return null;
        }
        if (!s.startsWith("http://") && !s.startsWith("https://")) {
            return null;
        }
        String host = Uri.parse(s).getHost();
        return host == null || host.isEmpty() ? null : s;
    }
}
