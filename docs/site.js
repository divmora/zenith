(function () {
  // Theme toggle (remembered per browser; storage may be unavailable).
  var root = document.documentElement, btn = document.getElementById("theme");
  try { var saved = localStorage.getItem("zenith-theme"); if (saved) root.setAttribute("data-theme", saved); } catch (e) {}
  btn.addEventListener("click", function () {
    var dark = root.getAttribute("data-theme") ? root.getAttribute("data-theme") === "dark" : !matchMedia("(prefers-color-scheme: light)").matches;
    var next = dark ? "light" : "dark";
    root.setAttribute("data-theme", next);
    try { localStorage.setItem("zenith-theme", next); } catch (e) {}
  });
  // Tabs.
  var tabs = document.querySelectorAll('[role="tab"]');
  tabs.forEach(function (t) {
    t.addEventListener("click", function () {
      tabs.forEach(function (o) {
        var on = o === t;
        o.setAttribute("aria-selected", on ? "true" : "false");
        document.getElementById(o.getAttribute("aria-controls")).hidden = !on;
      });
    });
  });
  // Copy buttons.
  document.querySelectorAll(".copy").forEach(function (b) {
    b.addEventListener("click", function () {
      var text = document.getElementById(b.getAttribute("data-copy")).innerText;
      (navigator.clipboard ? navigator.clipboard.writeText(text) : Promise.reject()).then(function () {
        b.textContent = "Copied"; setTimeout(function () { b.textContent = "Copy"; }, 1500);
      }, function () { b.textContent = "Select & copy"; });
    });
  });
})();
