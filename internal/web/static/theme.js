(function () {
  "use strict";
  try {
    if (localStorage.getItem("ffmuc-theme") === "light") {
      document.documentElement.dataset.theme = "light";
    }
  } catch (_) {
    // Storage can be unavailable in restricted browser contexts.
  }
})();
