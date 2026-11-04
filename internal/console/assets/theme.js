// Runs before the page is drawn, so that a chosen theme never flashes the other.
// With nothing chosen, or without scripts, the system's preference decides.
(function () {
  try {
    var theme = localStorage.getItem('moorage-theme');
    if (theme === 'light' || theme === 'dark') {
      document.documentElement.setAttribute('data-theme', theme);
    }
  } catch (e) {
    // Storage may be blocked: the system's preference it is.
  }
})();
