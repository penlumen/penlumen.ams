// The exam screen: one question at a time, answers saved as they change
// (written answers as the student types), a countdown, check-ins for the
// live monitor, and a check before submitting. Without this script the page
// still works: every question shows, and Submit sends all the answers.
(function () {
  "use strict";

  var root = document.querySelector("[data-exam]");
  if (!root) return;

  var attempt = root.dataset.attempt;
  var form = root.querySelector("[data-exam-form]");
  var sections = Array.prototype.slice.call(root.querySelectorAll("[data-question]"));
  var navLinks = Array.prototype.slice.call(root.querySelectorAll("[data-goto]"));
  var segments = Array.prototype.slice.call(root.querySelectorAll("[data-seg]"));
  var saveState = root.querySelector("[data-save-state]");
  var saveText = root.querySelector("[data-save-text]");
  var timerEl = root.querySelector("[data-timer]");
  var timerBox = root.querySelector("[data-timer-box]");
  var answeredEl = root.querySelector("[data-answered]");
  var confirmBox = root.querySelector("[data-confirm]");
  var pendingKey = "ams-pending-" + attempt;
  var current = 0;
  var submitting = false;

  // ---------- One question at a time ----------

  function show(index) {
    current = Math.max(0, Math.min(sections.length - 1, index));
    sections.forEach(function (s, i) { s.hidden = i !== current; });
    navLinks.forEach(function (a, i) {
      a.dataset.current = i === current;
      if (i === current) a.setAttribute("aria-current", "step"); else a.removeAttribute("aria-current");
    });
    var section = sections[current];
    section.querySelector("[data-prev]").disabled = current === 0;
    section.querySelector("[data-next-label]").textContent =
      current === sections.length - 1 ? "Review and submit" : "Next question";
  }

  sections.forEach(function (s) {
    s.querySelector("[data-step-buttons]").hidden = false;
    s.querySelector("[data-prev]").addEventListener("click", function () { show(current - 1); });
    s.querySelector("[data-next]").addEventListener("click", function () {
      if (current === sections.length - 1) openConfirm(); else show(current + 1);
    });
  });
  navLinks.forEach(function (a, i) {
    a.addEventListener("click", function (e) { e.preventDefault(); show(i); });
  });

  // ---------- Answers ----------

  function stateOf(section) {
    var st = { question_id: section.dataset.question, flagged: section.querySelector("[data-flag]").checked };
    var text = section.querySelector("[data-text]");
    if (text) {
      st.text = text.value;
      st.answered = text.value.trim() !== "";
    } else {
      var chosen = section.querySelector('input[type="radio"]:checked');
      st.answer = chosen ? parseInt(chosen.value, 10) : null;
      st.answered = st.answer !== null;
    }
    return st;
  }

  // Written answers are formatted text (editor.js): count the words, not
  // the tags. Line breaks and list items separate words too.
  function words(html) {
    var doc = new DOMParser().parseFromString((html || "").replace(/<(br|\/p|\/div|\/li)>/gi, " $&"), "text/html");
    var t = doc.body.textContent.trim();
    return t ? t.split(/\s+/).length : 0;
  }

  // refreshNav updates the navigator, progress bar and word counts.
  function refreshNav() {
    var answered = 0, flagged = 0;
    sections.forEach(function (s, i) {
      var st = stateOf(s);
      if (st.answered) answered++;
      if (st.flagged) flagged++;
      navLinks[i].dataset.answered = st.answered;
      navLinks[i].dataset.flagged = st.flagged;
      navLinks[i].setAttribute("aria-label", "Question " + (i + 1) +
        (st.answered ? ", answered" : ", not answered") + (st.flagged ? ", flagged" : ""));
      segments[i].dataset.filled = st.answered;
      var count = s.querySelector("[data-words]");
      if (count) count.textContent = words(st.text);
    });
    answeredEl.textContent = answered;
    return { answered: answered, flagged: flagged };
  }

  function readPending() {
    try { return JSON.parse(localStorage.getItem(pendingKey) || "{}"); } catch (e) { return {}; }
  }
  function writePending(p) {
    try { localStorage.setItem(pendingKey, JSON.stringify(p)); } catch (e) { /* private mode: keep in memory only */ }
  }
  var pending = readPending();

  function setSaveState(text, warn) {
    saveText.textContent = text;
    saveState.dataset.warn = !!warn;
  }

  var flushing = false;
  function flush() {
    var ids = Object.keys(pending);
    if (flushing || ids.length === 0) return;
    flushing = true;
    setSaveState("Saving…");
    var id = ids[0];
    var sent = pending[id];
    var body = { question_id: sent.question_id, flagged: sent.flagged };
    if ("text" in sent) body.text = sent.text; else body.answer = sent.answer;
    fetch("/attempts/" + attempt + "/answers", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", "Accept": "application/json" },
      body: JSON.stringify(body)
    }).then(function (res) {
      flushing = false;
      if (res.ok) {
        // Keep it queued if it changed while saving.
        if (pending[id] === sent) delete pending[id];
        writePending(pending);
        if (Object.keys(pending).length) flush(); else setSaveState("All answers saved");
      } else if (res.status === 409) {
        submitNow(); // time is up or already submitted: let the server finish it
      } else {
        setSaveState("Not saved yet, retrying", true);
      }
    }).catch(function () {
      flushing = false;
      setSaveState("Offline: answers kept on this device", true);
    });
  }

  function queue(section) {
    var st = stateOf(section);
    pending[st.question_id] = st;
    writePending(pending);
    refreshNav();
  }

  form.addEventListener("change", function (e) {
    var section = e.target.closest("[data-question]");
    if (!section) return;
    queue(section);
    flush();
  });
  // Written answers save a moment after the student stops typing.
  var typing;
  form.addEventListener("input", function (e) {
    if (!e.target.matches("[data-text]")) return;
    queue(e.target.closest("[data-question]"));
    setSaveState("Typing…");
    clearTimeout(typing);
    typing = setTimeout(flush, 800);
  });
  window.addEventListener("online", flush);
  setInterval(flush, 5000);

  // ---------- Telling the invigilator ----------

  function ping(event) {
    fetch("/attempts/" + attempt + "/ping" + (event ? "?event=" + event : ""), {
      method: "POST", credentials: "same-origin", keepalive: true,
      headers: { "Accept": "application/json" }
    }).catch(function () { /* offline: the monitor shows that anyway */ });
  }
  setInterval(function () { ping(""); }, 30000);
  document.addEventListener("visibilitychange", function () {
    if (!submitting) ping(document.hidden ? "left_page" : "returned");
  });

  // A reload also hides the page, so it sends "left the page" too. The tab
  // remembers when it unloaded; if the exam opens again in the same tab
  // straight away, it was only a reload and that alert is taken back. A
  // closed tab never comes back, so it still counts.
  var unloadKey = "ams-unloaded-" + attempt;
  try {
    var unloadedAt = parseInt(sessionStorage.getItem(unloadKey), 10);
    sessionStorage.removeItem(unloadKey);
    if (unloadedAt && Date.now() - unloadedAt < 30000) ping("reloaded");
  } catch (e) { /* storage blocked: the alert stays */ }
  window.addEventListener("pagehide", function () {
    if (submitting) return;
    try { sessionStorage.setItem(unloadKey, String(Date.now())); } catch (e) {}
  });

  // ---------- Timer ----------

  var endsAt = Date.now() + parseInt(root.dataset.secondsLeft, 10) * 1000;
  function pad(n) { return n < 10 ? "0" + n : "" + n; }
  function tick() {
    var left = Math.max(0, Math.round((endsAt - Date.now()) / 1000));
    var h = Math.floor(left / 3600), m = Math.floor((left % 3600) / 60), s = left % 60;
    timerEl.textContent = pad(h) + ":" + pad(m) + ":" + pad(s);
    timerBox.dataset.warn = left <= 300;
    if (left === 0) submitNow();
  }
  tick();
  setInterval(tick, 1000);

  // ---------- Submitting ----------

  function submitNow() {
    if (submitting) return;
    submitting = true;
    try { localStorage.removeItem(pendingKey); } catch (e) { /* ignore */ }
    // The form carries every answer, so nothing waiting in the queue is lost.
    form.submit();
  }

  function openConfirm() {
    var counts = refreshNav();
    var gaps = confirmBox.querySelector("[data-confirm-gaps]");
    var unanswered = sections.length - counts.answered;
    confirmBox.querySelector("[data-confirm-answered]").textContent = counts.answered;
    gaps.hidden = unanswered === 0 && counts.flagged === 0;
    gaps.textContent = unanswered + " not answered · " + counts.flagged + " flagged for review";
    confirmBox.hidden = false;
    confirmBox.querySelector("[data-cancel]").focus();
  }
  form.addEventListener("submit", function (e) {
    if (submitting) return;
    e.preventDefault();
    openConfirm();
  });
  confirmBox.querySelector("[data-cancel]").addEventListener("click", function () { confirmBox.hidden = true; });
  confirmBox.querySelector("[data-confirm-submit]").addEventListener("click", submitNow);
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && !confirmBox.hidden) confirmBox.hidden = true;
  });

  // Answers saved on this device but not yet sent: put them back on screen.
  Object.keys(pending).forEach(function (id) {
    var st = pending[id];
    var section = root.querySelector('[data-question="' + id + '"]');
    if (!section) return;
    if ("text" in st) {
      var t = section.querySelector("[data-text]");
      if (t) t.value = st.text;
    } else if (st.answer !== null && st.answer !== undefined) {
      var r = section.querySelector('input[type="radio"][value="' + st.answer + '"]');
      if (r) r.checked = true;
    }
    section.querySelector("[data-flag]").checked = !!st.flagged;
  });

  refreshNav();
  show(0);
  flush();
})();
