// Applies the Appearance choice before first paint, so a dark page never flashes white. It is a
// file, not an inline script, because the daemon's Content-Security-Policy refuses inline
// scripts. src/lib/theme.ts does the same after the app starts; keep the two in step.
(function () {
  var choice = null;
  try {
    choice = localStorage.getItem('mailrules.theme');
  } catch (e) {
    // Storage off: System.
  }
  var scheme = choice === 'light' || choice === 'dark' ? choice : matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  document.documentElement.dataset.theme = scheme;
  var bar = document.querySelector('meta[name="theme-color"]');
  if (bar) bar.setAttribute('content', scheme === 'dark' ? '#171411' : '#16130F');
})();
