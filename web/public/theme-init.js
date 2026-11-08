// Applies the saved theme before the first paint (an external file, because
// the Content-Security-Policy forbids inline scripts).
(function () {
  var theme = "system";
  try {
    theme = window.localStorage.getItem("syncr-theme") || "system";
  } catch (e) {
    // Storage may be unavailable; fall back to the system preference.
  }
  var dark =
    theme === "dark" ||
    (theme === "system" && window.matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.classList.toggle("dark", dark);
})();
