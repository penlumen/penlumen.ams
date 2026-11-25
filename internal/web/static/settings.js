// "Test connection" on the setup and Settings pages: sends the form's
// Penlumen School fields to the button's data-test-url and shows the answer.
(function () {
  document.querySelectorAll("[data-test-url]").forEach(function (button) {
    var out = document.getElementById(button.getAttribute("aria-controls"));
    button.addEventListener("click", function () {
      var body = new URLSearchParams(new FormData(button.form));
      button.disabled = true;
      out.className = "text-sm text-muted";
      out.textContent = "Checking…";
      fetch(button.getAttribute("data-test-url"), {
        method: "POST",
        body: body,
        headers: { Accept: "application/json" },
      })
        .then(function (r) { return r.json(); })
        .then(function (res) {
          out.className = "text-sm " + (res.ok ? "text-ok font-medium" : "text-danger");
          out.textContent = res.message;
        })
        .catch(function () {
          out.className = "text-sm text-danger";
          out.textContent = "Couldn't check. Is Exams still running?";
        })
        .finally(function () { button.disabled = false; });
    });
  });
})();
