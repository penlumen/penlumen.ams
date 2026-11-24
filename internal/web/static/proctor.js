// Camera, sound and screen monitoring, for exams that turn it on.
//
// Everything runs on the student's computer. Nothing is recorded: the
// camera and screen are only turned into a photo when something looks
// wrong (leaving the page, talking or noise, a device being turned off),
// plus one photo of the student when they first start. Sound is only
// measured for loudness; it is never saved or sent.
(function () {
  "use strict";

  // Talking is bursty (quiet between words), so noise is "loud for enough of
  // the last few seconds", not "loud without a break". Measured on a laptop
  // mic: silence ≈ 0.001–0.004, speech ≈ 0.02–0.28 (RMS, 0–1).
  var NOISE_LEVEL = 0.03;      // a reading this loud counts as talking
  var NOISE_WINDOW = 15;       // readings kept (every 200 ms: the last 3 s)
  var NOISE_LOUD = 6;          // loud readings in the window that raise an alert
  var NOISE_EVERY = 60000;     // at most one noise alert a minute
  var PHOTO_WIDTH = 640;

  function supported() {
    return !!(navigator.mediaDevices && navigator.mediaDevices.getUserMedia && navigator.mediaDevices.getDisplayMedia);
  }

  // Ask for all three. The screen must be the whole screen, not one window.
  function share() {
    var media, screen;
    return navigator.mediaDevices.getUserMedia({ video: { width: 640 }, audio: true })
      .then(function (s) {
        media = s;
        return navigator.mediaDevices.getDisplayMedia({ video: { displaySurface: "monitor" }, audio: false });
      })
      .then(function (s) {
        screen = s;
        var surface = s.getVideoTracks()[0].getSettings().displaySurface;
        if (surface && surface !== "monitor") {
          stop(s);
          stop(media);
          throw new Error("whole-screen");
        }
        return { media: media, screen: screen };
      })
      .catch(function (err) {
        stop(media);
        stop(screen);
        throw err;
      });
  }

  function stop(stream) {
    if (stream) stream.getTracks().forEach(function (t) { t.stop(); });
  }

  function explain(err) {
    if (!supported()) return "This browser can't share a camera and screen. Use Chrome, Edge or Firefox on a computer.";
    if (err && err.message === "whole-screen") return "Share your entire screen, not a window or tab.";
    if (err && err.name === "NotAllowedError") return "Access was refused. Allow the camera, microphone and screen when your browser asks.";
    if (err && err.name === "NotFoundError") return "No camera or microphone was found. Plug one in, or ask your invigilator.";
    return "Couldn't start the camera, microphone or screen. Close other apps using the camera and try again.";
  }

  // ---------- The start page: check before starting ----------

  var check = document.querySelector("[data-proctor-check]");
  if (check) {
    var startBtn = document.querySelector("[data-proctor-gate]");
    var status = document.querySelector("[data-proctor-status]");
    var preview = document.querySelector("[data-proctor-preview]");
    if (!supported()) {
      status.textContent = explain();
      check.disabled = true;
    }
    check.addEventListener("click", function () {
      status.textContent = "Waiting for your browser…";
      share().then(function (s) {
        preview.srcObject = s.media;
        preview.hidden = false;
        status.textContent = "Camera, microphone and screen are working. You can start.";
        status.dataset.ok = "true";
        startBtn.disabled = false;
        // Turned off again: the exam page asks for them once it opens.
        setTimeout(function () { stop(s.media); stop(s.screen); preview.hidden = true; }, 4000);
      }).catch(function (err) {
        status.textContent = explain(err);
        status.dataset.ok = "false";
      });
    });
    return;
  }

  // ---------- The exam page ----------

  var root = document.querySelector("[data-exam][data-proctor]");
  if (!root) return;
  var attempt = root.dataset.attempt;
  var firstTime = root.dataset.monitored !== "true";
  var gate = root.querySelector("[data-proctor-overlay]");
  var gateBtn = root.querySelector("[data-proctor-share]");
  var gateStatus = root.querySelector("[data-proctor-overlay-status]");
  var banner = root.querySelector("[data-proctor-banner]");
  var bannerBtn = root.querySelector("[data-proctor-reshare]");
  var selfView = root.querySelector("[data-proctor-self]");
  var camVideo = document.createElement("video");
  var screenVideo = document.createElement("video");
  [camVideo, screenVideo].forEach(function (v) { v.muted = true; v.playsInline = true; });
  var current = null;
  var noiseTimer = null, lastNoise = 0, recent = [];

  function event(kind) {
    return fetch("/attempts/" + attempt + "/ping?event=" + kind, {
      method: "POST", credentials: "same-origin", keepalive: true, headers: { "Accept": "application/json" }
    }).catch(function () {});
  }

  function photo(video, source, reason) {
    if (!video.videoWidth) return;
    var scale = Math.min(1, PHOTO_WIDTH / video.videoWidth);
    var canvas = document.createElement("canvas");
    canvas.width = Math.round(video.videoWidth * scale);
    canvas.height = Math.round(video.videoHeight * scale);
    canvas.getContext("2d").drawImage(video, 0, 0, canvas.width, canvas.height);
    canvas.toBlob(function (blob) {
      if (!blob) return;
      fetch("/attempts/" + attempt + "/snapshot?source=" + source + "&reason=" + reason, {
        method: "POST", credentials: "same-origin", body: blob,
        headers: { "Content-Type": "image/jpeg", "Accept": "application/json" }
      }).catch(function () {});
    }, "image/jpeg", 0.6);
  }

  // Something looked wrong: one photo of the student, one of the screen.
  function evidence(reason) {
    if (current && live(current.media, "video")) photo(camVideo, "camera", reason);
    if (current && live(current.screen, "video")) photo(screenVideo, "screen", reason);
  }

  function live(stream, kind) {
    return !!stream && stream.getTracks().some(function (t) { return t.kind === kind && t.readyState === "live"; });
  }

  function lost(kind) {
    event(kind);
    evidence(kind);
    banner.hidden = false;
  }

  function listenForNoise(stream) {
    var Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) return;
    var ctx = new Ctx();
    var analyser = ctx.createAnalyser();
    analyser.fftSize = 1024;
    ctx.createMediaStreamSource(stream).connect(analyser);
    var data = new Float32Array(analyser.fftSize);
    clearInterval(noiseTimer);
    noiseTimer = setInterval(function () {
      analyser.getFloatTimeDomainData(data);
      var sum = 0;
      for (var i = 0; i < data.length; i++) sum += data[i] * data[i];
      var level = Math.sqrt(sum / data.length);
      recent.push(level >= NOISE_LEVEL);
      if (recent.length > NOISE_WINDOW) recent.shift();
      var loud = recent.filter(Boolean).length;
      var now = Date.now();
      if (loud >= NOISE_LOUD && now - lastNoise >= NOISE_EVERY) {
        lastNoise = now;
        recent = [];
        event("noise");
        evidence("noise");
      }
    }, 200);
  }

  function start(s) {
    current = s;
    camVideo.srcObject = s.media;
    screenVideo.srcObject = s.screen;
    camVideo.play().catch(function () {});
    screenVideo.play().catch(function () {});
    selfView.srcObject = s.media;
    selfView.parentElement.hidden = false;
    gate.hidden = true;
    banner.hidden = true;
    s.media.getVideoTracks().forEach(function (t) { t.addEventListener("ended", function () { lost("camera_off"); }); });
    s.media.getAudioTracks().forEach(function (t) { t.addEventListener("ended", function () { lost("mic_off"); }); });
    s.screen.getVideoTracks().forEach(function (t) { t.addEventListener("ended", function () { lost("screen_off"); }); });
    listenForNoise(s.media);
    event("monitoring_on");
    if (firstTime) {
      firstTime = false;
      // One photo of the student at the start, to check who is sitting it.
      setTimeout(function () { photo(camVideo, "camera", "start"); }, 1500);
    }
  }

  function ask(statusEl) {
    statusEl.textContent = "Waiting for your browser…";
    share().then(function (s) { statusEl.textContent = ""; start(s); })
      .catch(function (err) { statusEl.textContent = explain(err); });
  }

  // Leaving the page: the screen shows where they went.
  document.addEventListener("visibilitychange", function () {
    if (document.hidden) setTimeout(function () { if (document.hidden) evidence("left_page"); }, 1500);
  });

  gateBtn.addEventListener("click", function () { ask(gateStatus); });
  bannerBtn.addEventListener("click", function () { ask(banner.querySelector("[data-proctor-banner-status]")); });

  if (firstTime) {
    // Before writing anything, everything must be on.
    gate.hidden = false;
  } else {
    // Back after a reload or a crash: keep writing, but sharing has stopped.
    banner.hidden = false;
    event("screen_off");
  }
})();
