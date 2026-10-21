// Sets the theme class before first paint so a dark-preferring user never
// sees a light flash on hard load/reload - useTheme.tsx's useEffect only runs
// after paint, which is too late to prevent a flash of the wrong theme.
// Mirrors useTheme.tsx's own persisted-theme-then-media-query logic.
//
// This is a classic same-origin script, not an inline one, because the
// Content-Security-Policy is `script-src 'self'`. It lives under public/assets/
// deliberately unhashed: the admin server only serves /assets/ and a short
// list of root files, and it sends no-cache for all of them.
(function () {
  try {
    var t = localStorage.getItem('marbor-theme');
    if (t !== 'light' && t !== 'dark') {
      t = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
    }
    document.documentElement.classList.add(t);
  } catch (e) {}
})();
