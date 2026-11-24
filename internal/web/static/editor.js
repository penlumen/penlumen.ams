// The formatted-text editor for questions and written answers (the
// "editor" template in layout.html). It writes a little HTML into the
// form's textarea, which is what gets sent; the server keeps only bold,
// italic, underline, superscript, subscript, lists and pictures uploaded
// here (see package richtext). Without JavaScript the textarea shows.
(function () {
  "use strict";

  var MAX_SIDE = 1600;          // pictures are shrunk to fit this (px)
  var MAX_BYTES = 2 * 1024 * 1024;
  var KEEP_UNDER = 600 * 1024;  // smaller pictures that fit go up untouched
  var SPACER = "​";        // an invisible place for the cursor; never saved

  function init(box) {
    var area = box.querySelector("[data-editor-area]");
    var field = box.querySelector("textarea");
    var toolbar = box.querySelector("[role=toolbar]");
    var status = box.querySelector("[data-editor-status]");
    var picker = box.querySelector("[data-editor-file]");
    var upload = box.dataset.upload;
    var saved = null; // where the cursor was, for inserting a picture

    area.innerHTML = field.value;
    toolbar.classList.replace("hidden", "flex");
    area.classList.remove("hidden");
    status.classList.remove("hidden");
    field.hidden = true;
    // Labels pointing at the textarea now point at the editor.
    if (field.id) {
      document.querySelectorAll("label[for='" + field.id + "']").forEach(function (l) {
        l.addEventListener("click", function () { area.focus(); });
      });
    }

    function empty() {
      return !area.textContent.trim() && !area.querySelector("img");
    }

    // Copy the editor into the textarea, and tell listeners (exam.js saves
    // answers on "input") that it changed.
    function sync() {
      var html = empty() ? "" : area.innerHTML.split(SPACER).join("");
      if (field.value === html) return;
      field.value = html;
      field.dispatchEvent(new Event("input", { bubbles: true }));
    }

    function refresh() {
      toolbar.querySelectorAll("[data-cmd]").forEach(function (b) {
        var on = false;
        try { on = document.queryCommandState(b.dataset.cmd); } catch (e) { /* unsupported */ }
        b.setAttribute("aria-pressed", on ? "true" : "false");
      });
    }

    // Turning subscript or superscript off while typing in it: browsers
    // nest another one instead, so step the cursor out past its end.
    function leaveScript(cmd) {
      var tag = { subscript: "sub", superscript: "sup" }[cmd];
      var sel = window.getSelection();
      if (!tag || !sel.rangeCount || !sel.isCollapsed) return false;
      var node = sel.anchorNode;
      var el = node.nodeType === 1 ? node : node.parentElement;
      var inside = el && el.closest(tag);
      if (!inside || !area.contains(inside)) return false;
      var after = document.createTextNode(SPACER);
      inside.parentNode.insertBefore(after, inside.nextSibling);
      var r = document.createRange();
      r.setStart(after, 1);
      r.collapse(true);
      sel.removeAllRanges();
      sel.addRange(r);
      return true;
    }

    function remember() {
      var sel = window.getSelection();
      if (sel.rangeCount && area.contains(sel.anchorNode)) saved = sel.getRangeAt(0).cloneRange();
    }

    function restore() {
      area.focus();
      if (!saved) return;
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(saved);
    }

    area.addEventListener("input", sync);
    area.addEventListener("keyup", remember);
    area.addEventListener("mouseup", remember);
    document.addEventListener("selectionchange", function () {
      if (document.activeElement === area) { remember(); refresh(); }
    });

    toolbar.querySelectorAll("[data-cmd]").forEach(function (b) {
      // Keep the selection in the editor when a button is pressed.
      b.addEventListener("mousedown", function (e) { e.preventDefault(); });
      b.addEventListener("click", function () {
        // The cursor is still where it was (the button doesn't take focus);
        // only come back to the editor if it was somewhere else.
        var sel = window.getSelection();
        if (!sel.rangeCount || !area.contains(sel.anchorNode)) restore();
        if (!leaveScript(b.dataset.cmd)) document.execCommand(b.dataset.cmd, false, null);
        sync();
        refresh();
      });
    });

    // Pasted text comes in plain (no fonts or colours from other apps);
    // a pasted picture is uploaded.
    area.addEventListener("paste", function (e) {
      var data = e.clipboardData;
      if (!data) return;
      e.preventDefault();
      var file = pictureIn(data.files);
      if (file) { remember(); addPicture(file); return; }
      document.execCommand("insertText", false, data.getData("text/plain"));
    });
    area.addEventListener("drop", function (e) {
      var file = e.dataTransfer && pictureIn(e.dataTransfer.files);
      e.preventDefault();
      if (file) { remember(); addPicture(file); }
    });

    if (picker) {
      box.querySelector("[data-editor-picture]").addEventListener("click", function () {
        remember();
        picker.click();
      });
      picker.addEventListener("change", function () {
        if (picker.files[0]) addPicture(picker.files[0]);
        picker.value = "";
      });
    }

    function addPicture(file) {
      if (!upload) return;
      if (!/^image\/(png|jpeg|gif|webp)$/.test(file.type)) {
        status.textContent = "Use a PNG, JPEG, GIF or WebP picture.";
        return;
      }
      status.textContent = "Adding the picture…";
      shrink(file).then(function (blob) {
        if (blob.size > MAX_BYTES) throw new Error("That picture is too big. Use one under 2 MB.");
        return fetch(upload, {
          method: "POST", credentials: "same-origin", body: blob,
          headers: { "Content-Type": blob.type, "Accept": "application/json" }
        });
      }).then(function (res) {
        return res.json().catch(function () { return {}; }).then(function (body) {
          if (!res.ok) throw new Error(body.error || "The picture couldn't be added. Try again.");
          return body.url;
        });
      }).then(function (url) {
        restore();
        document.execCommand("insertImage", false, url);
        status.textContent = "";
        sync();
      }).catch(function (err) {
        status.textContent = err && err.message && !/fetch|network/i.test(err.message)
          ? err.message : "The picture couldn't be added. Check your connection and try again.";
      });
    }
  }

  function pictureIn(files) {
    for (var i = 0; files && i < files.length; i++) {
      if (/^image\//.test(files[i].type)) return files[i];
    }
    return null;
  }

  // Big photos are made smaller before uploading: a phone photo can be
  // 5 MB, and nobody needs that to read a question.
  function shrink(file) {
    if (file.type === "image/gif") return Promise.resolve(file); // may be animated
    return new Promise(function (resolve) {
      var img = new Image();
      var url = URL.createObjectURL(file);
      img.onload = function () {
        URL.revokeObjectURL(url);
        var scale = Math.min(1, MAX_SIDE / Math.max(img.naturalWidth, img.naturalHeight));
        if (scale === 1 && file.size <= KEEP_UNDER) { resolve(file); return; }
        var canvas = document.createElement("canvas");
        canvas.width = Math.round(img.naturalWidth * scale);
        canvas.height = Math.round(img.naturalHeight * scale);
        var ctx = canvas.getContext("2d");
        ctx.fillStyle = "#fff"; // transparent parts stay white as JPEG
        ctx.fillRect(0, 0, canvas.width, canvas.height);
        ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
        // Diagrams (PNG) stay sharp as PNG unless that's too big.
        var type = file.type === "image/png" ? "image/png" : "image/jpeg";
        canvas.toBlob(function (blob) {
          if (blob && blob.size <= MAX_BYTES) { resolve(blob); return; }
          canvas.toBlob(function (jpeg) { resolve(jpeg || file); }, "image/jpeg", 0.85);
        }, type, 0.85);
      };
      img.onerror = function () { URL.revokeObjectURL(url); resolve(file); };
      img.src = url;
    });
  }

  document.querySelectorAll("[data-editor]").forEach(init);
})();
