// Self-service usage dashboard. Fetches per-key usage from POST /usage using the key
// as a Bearer token (never in the URL, so it doesn't leak via referrer/logs).
// i18n: shares localStorage key "kiro_lang" and /admin/locales/*.json with the admin
// app, so switching language here stays in sync with the admin panel.

(function () {
  "use strict";

  var keyInput = document.getElementById("keyInput");
  var checkBtn = document.getElementById("checkBtn");
  var errBox = document.getElementById("errBox");
  var dash = document.getElementById("dashboard");
  var rememberBox = document.getElementById("rememberKey");
  var clearKeyBtn = document.getElementById("clearKeyBtn");

  // localStorage slot for the opt-in "remember key on this device" feature. The key is
  // stored in plaintext, so this is a convenience for trusted personal devices only —
  // the checkbox label says as much. Cleared by the Remove button.
  var SAVED_KEY = "kiro_usage_key";

  var currentLang = localStorage.getItem("kiro_lang") || "en";
  var dict = {};
  var lastData = null; // last successful payload, re-rendered on language switch

  function t(key, a0) {
    var s = (dict[currentLang] && dict[currentLang][key]) || key;
    if (a0 !== undefined) s = s.replace("{0}", a0);
    return s;
  }

  function loadLocale(lang) {
    return fetch("/admin/locales/" + lang + ".json?v=" + Date.now(), { cache: "no-store" })
      .then(function (r) { return r.json(); })
      .then(function (j) { dict[lang] = j; })
      .catch(function () { dict[lang] = {}; });
  }

  function fmtNum(n) {
    n = Number(n) || 0;
    if (n >= 1e9) return (n / 1e9).toFixed(2).replace(/\.?0+$/, "") + "B";
    if (n >= 1e6) return (n / 1e6).toFixed(2).replace(/\.?0+$/, "") + "M";
    if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.?0+$/, "") + "K";
    return String(n);
  }

  function fmtDate(unix) {
    if (!unix) return "—";
    var d = new Date(unix * 1000);
    return d.toLocaleDateString(undefined, { year: "numeric", month: "short", day: "numeric" });
  }

  function fmtTime(unix) {
    if (!unix) return "—";
    return new Date(unix * 1000).toLocaleString();
  }

  // countdownText returns a localized time-until-expiry string, choosing the unit by
  // magnitude. Returns null for no-expiry keys and the "expired" label once past.
  function countdownText(expiresAt) {
    if (!expiresAt) return null;
    var secs = expiresAt - Math.floor(Date.now() / 1000);
    if (secs <= 0) return t("usage.expired");
    var days = Math.floor(secs / 86400);
    var hours = Math.floor(secs / 3600);
    var mins = Math.floor(secs / 60);
    if (days >= 1) return days + " " + t("usage.unitDays");
    if (hours >= 1) return hours + " " + t("usage.unitHours") + " " + (mins % 60) + " " + t("usage.unitMinutes");
    return mins + " " + t("usage.unitMinutes");
  }

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function showError(msg) {
    errBox.textContent = msg;
    errBox.classList.remove("hidden");
    dash.classList.add("hidden");
  }
  // __CHUNK2__

  function check() {
    var key = keyInput.value.trim();
    if (!key) {
      showError(t("usage.errEmpty"));
      return;
    }
    errBox.classList.add("hidden");
    checkBtn.disabled = true;
    checkBtn.innerHTML = '<i class="fa-solid fa-spinner fa-spin"></i> ' + t("usage.loading");

    fetch("/usage", {
      method: "POST",
      headers: { "Authorization": "Bearer " + key, "Content-Type": "application/json" },
    })
      .then(function (r) {
        return r.json().then(function (body) {
          return { ok: r.ok, status: r.status, body: body };
        });
      })
      .then(function (res) {
        if (!res.ok) {
          showError((res.body && res.body.error) || ("Error " + res.status));
          return;
        }
        lastData = res.body;
        if (rememberBox && rememberBox.checked) saveKey(key);
        render(res.body);
      })
      .catch(function () {
        showError(t("usage.errServer"));
      })
      .finally(function () {
        checkBtn.disabled = false;
        checkBtn.innerHTML = '<i class="fa-solid fa-magnifying-glass"></i> ' + t("usage.checkBtn");
      });
  }

  // applyStaticI18n localizes the fixed page chrome (title, subtitle, input, buttons)
  // and any element carrying a data-i18n attribute.
  function applyStaticI18n() {
    document.documentElement.lang = currentLang;
    document.querySelectorAll("[data-i18n]").forEach(function (el) {
      el.textContent = t(el.dataset.i18n);
    });
    document.querySelectorAll("[data-i18n-ph]").forEach(function (el) {
      el.placeholder = t(el.dataset.i18nPh);
    });
    checkBtn.innerHTML = '<i class="fa-solid fa-magnifying-glass"></i> ' + t("usage.checkBtn");
    highlightLangBtn();
  }

  // Supported languages: English / Tiếng Việt / 中文. Direct selection (no cycling),
  // so the active button is always the language currently shown.
  var LANGS = ["en", "vi", "zh"];

  // highlightLangBtn marks the active language button so the user can see, at a glance,
  // which language the page is in.
  function highlightLangBtn() {
    document.querySelectorAll("#langBtns [data-lang]").forEach(function (b) {
      var active = b.dataset.lang === currentLang;
      b.classList.toggle("bg-blue-600", active);
      b.classList.toggle("text-white", active);
      b.classList.toggle("text-slate-300", !active);
    });
  }

  function switchLang(next) {
    if (LANGS.indexOf(next) < 0 || next === currentLang) return;
    currentLang = next;
    localStorage.setItem("kiro_lang", next);
    var after = function () {
      applyStaticI18n();
      if (lastData) render(lastData); // re-render dashboard in the new language
    };
    if (dict[next]) after(); else loadLocale(next).then(after);
  }

  // saveKey persists the key and reveals the Remove button.
  function saveKey(key) {
    try { localStorage.setItem(SAVED_KEY, key); } catch (e) { /* storage disabled */ }
    if (clearKeyBtn) clearKeyBtn.classList.remove("hidden");
  }

  // clearKey forgets the saved key, empties the input, hides the dashboard, and
  // unticks the checkbox — leaving the page as if no key was ever entered.
  function clearKey() {
    try { localStorage.removeItem(SAVED_KEY); } catch (e) { /* storage disabled */ }
    keyInput.value = "";
    lastData = null;
    if (rememberBox) rememberBox.checked = false;
    if (clearKeyBtn) clearKeyBtn.classList.add("hidden");
    dash.classList.add("hidden");
    errBox.classList.add("hidden");
  }

  checkBtn.addEventListener("click", check);
  keyInput.addEventListener("keydown", function (e) {
    if (e.key === "Enter") check();
  });
  var langBtns = document.getElementById("langBtns");
  if (langBtns) langBtns.addEventListener("click", function (e) {
    var b = e.target.closest("[data-lang]");
    if (b) switchLang(b.dataset.lang);
  });
  if (clearKeyBtn) clearKeyBtn.addEventListener("click", clearKey);
  // Show/hide the key: flips the input type and swaps the eye icon.
  var toggleKeyBtn = document.getElementById("toggleKeyBtn");
  if (toggleKeyBtn) toggleKeyBtn.addEventListener("click", function () {
    var show = keyInput.type === "password";
    keyInput.type = show ? "text" : "password";
    toggleKeyBtn.innerHTML = show
      ? '<i class="fa-solid fa-eye-slash"></i>'
      : '<i class="fa-solid fa-eye"></i>';
  });
  // Unticking the box forgets an already-saved key immediately.
  if (rememberBox) rememberBox.addEventListener("change", function () {
    if (!rememberBox.checked) {
      try { localStorage.removeItem(SAVED_KEY); } catch (e) { /* storage disabled */ }
      if (clearKeyBtn) clearKeyBtn.classList.add("hidden");
    }
  });

  // Bootstrap: load the active locale, then localize the static chrome.
  loadLocale(currentLang).then(applyStaticI18n);

  // Restore a previously remembered key: prefill, tick the box, reveal Remove, and
  // auto-check so the dashboard is populated on load (survives F5).
  var saved = null;
  try { saved = localStorage.getItem(SAVED_KEY); } catch (e) { /* storage disabled */ }
  if (saved) {
    keyInput.value = saved;
    if (rememberBox) rememberBox.checked = true;
    if (clearKeyBtn) clearKeyBtn.classList.remove("hidden");
    check();
  }

  // __RENDER__

  function render(data) {
    var k = data.key || {};
    var life = data.lifetime || {};
    var daily = data.daily || {};
    var byModel = data.byModel || [];
    var logs = data.logs || [];

    var html = "";

    // --- Key header card ---
    var statusPill = k.enabled && !k.expired
      ? '<span class="pill ok">' + t("usage.enabled") + "</span>"
      : '<span class="pill bad">' + (k.expired ? t("usage.expired") : t("usage.disabled")) + "</span>";
    var dl = countdownText(k.expiresAt);
    html += '<div class="card p-5">';
    html += '<div class="flex items-center justify-between flex-wrap gap-2">';
    html += '<div><div class="text-lg font-semibold">' + esc(k.name || t("usage.keyName")) + "</div>";
    html += '<div class="text-xs text-slate-400 font-mono">' + esc(k.masked || "") + "</div></div>";
    html += "<div>" + statusPill + "</div></div>";
    html += '<div class="grid grid-cols-2 sm:grid-cols-4 gap-3 mt-4 text-sm">';
    html += statBox(t("usage.created"), fmtDate(k.createdAt));
    html += statBox(t("usage.expiresAt"), fmtDate(k.expiresAt));
    html += statBox(t("usage.countdown"), dl == null ? t("usage.never") : dl);
    html += statBox(t("usage.lastUsed"), fmtTime(k.lastUsedAt));
    html += "</div></div>";

    // --- Top stat row ---
    // Requests uses the true lifetime total (survives a "Reset Usage"), falling back to
    // the current-period count for older responses that lack the lifetime field.
    var lifeRequests = life.lifetimeRequests != null ? life.lifetimeRequests : life.requests;
    html += '<div class="grid grid-cols-2 sm:grid-cols-4 gap-3">';
    html += statBox(t("usage.requests"), fmtNum(lifeRequests) + '<div class="text-[10px] text-slate-500">' + t("usage.lifetime") + "</div>");
    html += statBox(t("usage.dailyTokens"), fmtNum(daily.tokens) + '<div class="text-[10px] text-slate-500">' + t("usage.sinceMidnight") + "</div>");
    html += statBox("RPM", k.rpmLimit ? fmtNum(k.rpmLimit) : "∞");
    html += statBox("TPM", k.tpmLimit ? fmtNum(k.tpmLimit) : "∞");
    html += "</div>";

    html += renderQuotas(k, life);
    html += renderByModel(byModel);
    html += renderLogs(logs);

    dash.innerHTML = html;
    dash.classList.remove("hidden");
    errBox.classList.add("hidden");

    var refreshBtn = document.getElementById("refreshBtn");
    if (refreshBtn) refreshBtn.addEventListener("click", check);
  }

  function statBox(label, valueHtml) {
    return '<div class="stat p-3"><div class="text-[11px] text-slate-400 mb-1">' +
      esc(label) + '</div><div class="text-base font-semibold">' + valueHtml + "</div></div>";
  }

  // renderQuotas draws a progress bar for the overall token limit (an enforced quota).
  function renderQuotas(k, life) {
    if (!k.tokenLimit || k.tokenLimit <= 0) return "";
    return '<div class="card p-5"><div class="text-sm font-semibold mb-3">' + t("usage.quotaTokens") + "</div>" +
      bar(t("usage.quotaTotal"), life.tokensUsed || 0, k.tokenLimit) + "</div>";
  }

  function bar(label, used, limit) {
    var pct = limit > 0 ? Math.min(100, (used / limit) * 100) : 0;
    return '<div class="mb-4 last:mb-0">' +
      '<div class="flex justify-between text-xs mb-1">' +
      '<span class="text-slate-300">' + esc(label) + "</span>" +
      '<span class="text-slate-400">' + fmtNum(used) + " / " + fmtNum(limit) +
      "  (" + pct.toFixed(0) + "%)</span></div>" +
      '<div class="bar-track h-2"><div class="bar-fill" style="width:' + pct + '%"></div></div></div>';
  }

  function renderByModel(byModel) {
    var html = '<div class="card p-5"><div class="text-sm font-semibold mb-3">' + t("usage.byModel") + " " +
      '<span class="text-xs text-slate-500 font-normal">' + t("usage.byModelHint") + "</span></div>";
    if (!byModel.length) {
      html += '<div class="text-sm text-slate-500">' + t("usage.noData") + "</div></div>";
      return html;
    }
    html += '<div class="overflow-x-auto"><table><thead><tr>' +
      "<th>" + t("usage.model") + "</th><th>" + t("usage.requests") + "</th><th>" + t("usage.failures") + "</th>" +
      "<th>↑ " + t("usage.input") + "</th><th>" + t("usage.cache") + "</th><th>↓ " + t("usage.output") + "</th></tr></thead><tbody>";
    byModel.forEach(function (m) {
      html += "<tr>" +
        "<td class='font-mono'>" + esc(m.model) + "</td>" +
        "<td>" + fmtNum(m.requests) + "</td>" +
        "<td class='" + (m.failures ? "text-red-400" : "") + "'>" + fmtNum(m.failures) + "</td>" +
        "<td>" + fmtNum(m.inputTok) + "</td>" +
        "<td>" + fmtNum(m.cacheTok) + "</td>" +
        "<td>" + fmtNum(m.outputTok) + "</td></tr>";
    });
    html += "</tbody></table></div></div>";
    return html;
  }

  function renderLogs(logs) {
    // Show only the 50 most recent requests (backend returns more; UI caps for brevity).
    logs = logs.slice(0, 50);
    var html = '<div class="card p-5">' +
      '<div class="flex items-center justify-between mb-3">' +
      '<div class="text-sm font-semibold">' + t("usage.requestLog") + " " +
      '<span class="text-xs text-slate-500 font-normal">' + t("usage.recentN", logs.length) + "</span></div>" +
      '<button id="refreshBtn" class="bg-slate-700 hover:bg-slate-600 text-white text-xs font-semibold px-4 py-2 rounded-lg">' +
      '<i class="fa-solid fa-rotate-right"></i> ' + t("usage.refresh") + "</button></div>";
    if (!logs.length) {
      html += '<div class="text-sm text-slate-500">' + t("usage.noRequests") + "</div></div>";
      return html;
    }
    html += '<div class="overflow-x-auto"><table><thead><tr>' +
      "<th>" + t("usage.time") + "</th><th>" + t("usage.model") + "</th><th>" + t("usage.type") + "</th><th>" + t("usage.status") + "</th><th>IP</th>" +
      "<th>↑ " + t("usage.input") + "</th><th>" + t("usage.cache") + "</th><th>↓ " + t("usage.output") + "</th><th>" + t("usage.duration") + "</th></tr></thead><tbody>";
    logs.forEach(function (l) {
      var ok = l.status === "success";
      html += "<tr>" +
        "<td>" + fmtTime(l.time) + "</td>" +
        "<td class='font-mono'>" + esc(l.model) + "</td>" +
        "<td>" + esc(l.endpoint) + "</td>" +
        "<td><span class='pill " + (ok ? "ok" : "bad") + "'>" + (ok ? t("usage.success") : t("usage.error")) + "</span></td>" +
        "<td class='font-mono'>" + esc(l.ip || "—") + "</td>" +
        "<td>" + fmtNum(l.inputTokens) + "</td>" +
        "<td>" + fmtNum(l.cacheTokens) + "</td>" +
        "<td>" + fmtNum(l.outputTokens) + "</td>" +
        "<td>" + fmtNum(l.duration) + " ms</td></tr>";
    });
    html += "</tbody></table></div></div>";
    return html;
  }
})();
