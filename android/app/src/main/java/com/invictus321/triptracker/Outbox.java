package com.invictus321.triptracker;

import android.content.SharedPreferences;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.util.ArrayList;
import java.util.List;

/**
 * Ordered queue of API requests that survives app restarts. Requests are only removed
 * once the server has accepted them, so nothing is lost while the phone has no signal.
 */
final class Outbox {
    private static final String TAG = "Outbox";
    private static final int MAX_ITEMS = 5000;

    private final SharedPreferences prefs;
    private final List<JSONObject> items = new ArrayList<>();

    Outbox(SharedPreferences prefs) {
        this.prefs = prefs;
        try {
            JSONArray saved = new JSONArray(prefs.getString(Prefs.OUTBOX, "[]"));
            for (int i = 0; i < saved.length(); i++) {
                items.add(saved.getJSONObject(i));
            }
        } catch (JSONException e) {
            Log.w(TAG, "discarding unreadable outbox", e);
        }
    }

    void add(String path, JSONObject body) {
        JSONObject item = new JSONObject();
        try {
            item.put("path", path);
            item.put("body", body);
        } catch (JSONException e) {
            throw new IllegalStateException(e);
        }
        items.add(item);
        // Never drop start/stop signals; shed the oldest locations if offline for days.
        while (items.size() > MAX_ITEMS) {
            int victim = 0;
            for (int i = 0; i < items.size(); i++) {
                if ("/api/location".equals(items.get(i).optString("path"))) {
                    victim = i;
                    break;
                }
            }
            items.remove(victim);
        }
        save();
    }

    JSONObject peek() {
        return items.get(0);
    }

    void removeFirst() {
        items.remove(0);
        save();
    }

    boolean isEmpty() {
        return items.isEmpty();
    }

    int size() {
        return items.size();
    }

    private void save() {
        prefs.edit()
                .putString(Prefs.OUTBOX, new JSONArray(items).toString())
                .putInt(Prefs.QUEUED, items.size())
                .apply();
    }
}
