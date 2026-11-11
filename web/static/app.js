(() => {
  "use strict";

  const BLUE = "#1e6fe8";
  const ORANGE = "#f28c28";
  const PIN_PATH = "M12 0C5.4 0 0 5.4 0 12c0 9 12 24 12 24s12-15 12-24C24 5.4 18.6 0 12 0z";

  // Live after this long without a location, the status turns amber, then red.
  const WARN_AFTER_MS = 150 * 1000;
  const ALERT_AFTER_MS = 10 * 60 * 1000;

  const map = L.map("map").setView([0, 0], 2);
  L.tileLayer("https://tile.openstreetmap.org/{z}/{x}/{y}.png", {
    maxZoom: 19,
    attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors',
  }).addTo(map);

  const pinIcon = (color, w) => {
    const h = w * 1.5;
    return L.divIcon({
      className: "pin",
      html: `<svg width="${w}" height="${h}" viewBox="0 0 24 36"><path d="${PIN_PATH}" fill="${color}" stroke="#fff" stroke-width="1.5"/><circle cx="12" cy="12" r="4.5" fill="#fff"/></svg>`,
      iconSize: [w, h],
      iconAnchor: [w / 2, h],
      popupAnchor: [0, -h + 4],
    });
  };
  const blueIcon = pinIcon(BLUE, 32);
  const orangeIcon = pinIcon(ORANGE, 20);

  const historyLayer = L.layerGroup().addTo(map);
  const route = L.polyline([], { color: ORANGE, weight: 3, opacity: 0.6 }).addTo(map);
  let blueMarker = null;
  let state = null;
  let clockOffset = 0; // serverTime - local time, so "ago" values ignore viewer clock skew
  let renderedKey = null;
  let fitted = false;

  const statusEl = document.getElementById("status");
  const titleEl = document.getElementById("status-title");
  const detailEl = document.getElementById("status-detail");
  const connEl = document.getElementById("conn");

  const now = () => Date.now() + clockOffset;

  function formatAgo(ms) {
    const s = Math.max(0, Math.floor(ms / 1000));
    if (s < 60) return `${s}s`;
    const m = Math.floor(s / 60);
    if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
    const h = Math.floor(m / 60);
    if (h < 24) return `${h}h ${String(m % 60).padStart(2, "0")}m`;
    return `${Math.floor(h / 24)}d ${h % 24}h`;
  }

  const formatTime = (iso) =>
    new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "medium" });

  function toDMS(value, pos, neg) {
    const abs = Math.abs(value);
    const d = Math.floor(abs);
    const mFloat = (abs - d) * 60;
    const m = Math.floor(mFloat);
    const s = ((mFloat - m) * 60).toFixed(1);
    return `${d}°${String(m).padStart(2, "0")}'${s.padStart(4, "0")}"${value >= 0 ? pos : neg}`;
  }

  function popupHTML(p) {
    const decimal = `${p.lat.toFixed(6)}, ${p.lon.toFixed(6)}`;
    const dms = `${toDMS(p.lat, "N", "S")} ${toDMS(p.lon, "E", "W")}`;
    const meta = [];
    if (p.accuracy) meta.push(`Accuracy ±${Math.round(p.accuracy)} m`);
    if (typeof p.battery === "number") meta.push(`Battery ${p.battery}%`);
    return `<div class="coords-popup">
      <h3>Current location</h3>
      <div class="coords-row"><input readonly value="${decimal}" aria-label="Decimal coordinates"><button type="button" class="copy-btn" data-text="${decimal}">Copy</button></div>
      <div class="coords-row"><input readonly value="${dms.replace(/"/g, "&quot;")}" aria-label="DMS coordinates"><button type="button" class="copy-btn" data-text="${dms.replace(/"/g, "&quot;")}">Copy</button></div>
      <div class="coords-meta">Fix taken ${formatTime(p.time)} (<span data-ago="${p.time}"></span> ago)</div>
      ${meta.length ? `<div class="coords-meta">${meta.join(" · ")}</div>` : ""}
      <div class="coords-meta"><a href="https://www.google.com/maps?q=${p.lat},${p.lon}" target="_blank" rel="noopener">Open in Google Maps</a></div>
    </div>`;
  }

  async function copyText(text, button) {
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // Clipboard API needs HTTPS; fall back to selecting the adjacent input.
      const input = button.parentElement.querySelector("input");
      input.focus();
      input.select();
      document.execCommand("copy");
    }
    button.textContent = "Copied";
    button.classList.add("copied");
    setTimeout(() => {
      button.textContent = "Copy";
      button.classList.remove("copied");
    }, 1500);
  }

  document.addEventListener("click", (e) => {
    const btn = e.target.closest(".copy-btn");
    if (btn) copyText(btn.dataset.text, btn);
  });

  function render(s) {
    state = s;
    clockOffset = Date.parse(s.serverTime) - Date.now();
    const points = (s.journey && s.journey.points) || [];
    const latest = points[points.length - 1];
    const key = `${s.journey ? s.journey.id : ""}|${points.length}|${latest ? latest.time : ""}`;

    if (key !== renderedKey) {
      renderedKey = key;
      historyLayer.clearLayers();
      points.slice(0, -1).forEach((p) => {
        L.marker([p.lat, p.lon], { icon: orangeIcon, opacity: 0.5, keyboard: false })
          .bindTooltip(formatTime(p.time))
          .addTo(historyLayer);
      });
      route.setLatLngs(points.map((p) => [p.lat, p.lon]));

      if (latest) {
        const ll = [latest.lat, latest.lon];
        if (!blueMarker) {
          blueMarker = L.marker(ll, { icon: blueIcon, zIndexOffset: 1000, title: "Current location" })
            .bindPopup(popupHTML(latest), { maxWidth: 360, minWidth: 300, autoPanPaddingTopLeft: [20, 120] })
            .addTo(map);
        } else {
          blueMarker.setLatLng(ll).setPopupContent(popupHTML(latest));
        }
        if (!fitted) {
          fitted = true;
          if (points.length > 1) map.fitBounds(route.getBounds(), { padding: [60, 60], maxZoom: 16 });
          else map.setView(ll, 16);
        } else if (!map.getBounds().contains(ll)) {
          map.panTo(ll);
        }
      } else if (blueMarker) {
        blueMarker.remove();
        blueMarker = null;
      }
    }
    tick();
  }

  function setStatus(cls, title, detail) {
    statusEl.className = `status ${cls}`;
    titleEl.textContent = title;
    detailEl.textContent = detail;
  }

  function tick() {
    document.querySelectorAll("[data-ago]").forEach((el) => {
      el.textContent = formatAgo(now() - Date.parse(el.dataset.ago));
    });
    if (!state) return;
    const j = state.journey;
    if (!j) {
      setStatus("waiting", "No journey yet", "Waiting for the first location…");
      return;
    }
    const lastSeen = j.lastSeen ? Date.parse(j.lastSeen) : null;
    if (j.ended) {
      setStatus(
        "ended",
        "✅ Journey ended safely",
        `Stopped ${formatTime(j.endedAt)} (${formatAgo(now() - Date.parse(j.endedAt))} ago)`,
      );
      return;
    }
    if (!lastSeen) {
      setStatus("waiting", "Journey started", `Waiting for the first location… (started ${formatAgo(now() - Date.parse(j.startedAt))} ago)`);
      return;
    }
    const age = now() - lastSeen;
    const cls = age >= ALERT_AFTER_MS ? "alert" : age >= WARN_AFTER_MS ? "warn" : "ok";
    setStatus(cls, `Last update ${formatAgo(age)} ago`, "Journey in progress — not yet ended");
  }

  function connect() {
    const es = new EventSource("api/events");
    es.addEventListener("state", (e) => render(JSON.parse(e.data)));
    es.onopen = () => (connEl.hidden = true);
    es.onerror = () => (connEl.hidden = false); // EventSource retries automatically
  }

  async function poll() {
    try {
      const res = await fetch("api/state", { cache: "no-store" });
      if (res.ok) render(await res.json());
    } catch {
      // The event stream reports connectivity problems.
    }
  }

  connect();
  setInterval(tick, 1000);
  setInterval(poll, 30000);
})();
