// Apply a saved theme before first paint so nothing flashes; "auto" leaves the
// attribute off and the stylesheet follows the system preference.
(function () {
  try {
    var t = localStorage.getItem('tasmonaut.theme');
    if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
  } catch (e) {}
})();
