// The assessment form: picking a class shows that class's subjects and its
// branch's terms (from the school portal), with the current term chosen.
(function () {
  "use strict";
  var form = document.querySelector("[data-assessment-form]");
  var dataEl = document.getElementById("class-data");
  if (!form || !dataEl) return;

  // The Config Key box shows only when Safe Exam Browser is required.
  var sebSwitch = form.querySelector('[name="require_seb"]');
  var sebKeys = form.querySelector("[data-seb-keys]");
  if (sebSwitch && sebKeys) {
    sebSwitch.addEventListener("change", function () { sebKeys.hidden = !sebSwitch.checked; });
  }

  var byClass = JSON.parse(dataEl.textContent || "{}");
  var classSel = form.querySelector("[data-class]");
  var subjectSel = form.querySelector("[data-subject]");
  var termSel = form.querySelector("[data-term]");

  function option(value, label, selected) {
    var o = document.createElement("option");
    o.value = value;
    o.textContent = label;
    o.selected = !!selected;
    return o;
  }

  classSel.addEventListener("change", function () {
    var info = byClass[classSel.value];
    var subject = subjectSel.value;
    subjectSel.replaceChildren(option("", info ? "Choose a subject…" : "Choose a class first"));
    subjectSel.disabled = !info;
    termSel.replaceChildren();
    if (!info) return;
    (info.subjects || []).forEach(function (s) {
      subjectSel.append(option(s, s, s === subject));
    });
    (info.terms || []).forEach(function (t) {
      termSel.append(option(t.UUID, t.Label + (t.Current ? " (current)" : ""), t.Current));
    });
  });
})();
