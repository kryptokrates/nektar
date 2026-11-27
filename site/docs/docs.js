(function () {
  for (const btn of document.querySelectorAll('.cp-btn')) {
    btn.addEventListener('click', async () => {
      const code = btn.closest('.code-wrap')?.querySelector('pre code')?.textContent || '';
      await navigator.clipboard.writeText(code).catch(() => {});
      btn.textContent = 'Copied ✓';
      setTimeout(() => (btn.textContent = 'Copy'), 1400);
    });
  }

  const vid = document.getElementById('doc-video');
  const cov = document.getElementById('doc-play');
  if (vid && cov) {
    cov.addEventListener('click', () => {
      cov.classList.add('hidden');
      vid.play().catch(() => {});
    });
    vid.addEventListener('play', () => cov.classList.add('hidden'));
  }

  function normPath(p) {
    return (p || '/').replace(/\/index\.html$/i, '/').replace(/\/+$/, '') || '/';
  }

  function scrollToContent(smooth) {
    const target = document.getElementById('content');
    if (!target) return;
    const top = Math.max(0, Math.round(target.getBoundingClientRect().top + window.scrollY - 64));
    if (smooth) {
      window.scrollTo({ top, behavior: 'smooth' });
    } else {
      window.scrollTo(0, top);
    }
  }

  // On mobile stacked layout (where #content sits below .side), ensure #content lands cleanly under the sticky topbar.
  if (location.hash === '#content') {
    const target = document.getElementById('content');
    if (target && target.getBoundingClientRect().top > 120) {
      requestAnimationFrame(() => scrollToContent(false));
    }
  }

  // Clicking a sidebar link to the current page (especially on mobile) must always scroll down to #content.
  // Deferring to requestAnimationFrame prevents Chromium's click-focus handling from cancelling the smooth scroll.
  document.addEventListener('click', (e) => {
    const link = e.target && e.target.closest ? e.target.closest('.side a.slink[href*="#content"]') : null;
    if (!link) return;
    let samePage = link.classList.contains('on');
    if (!samePage) {
      try {
        const url = new URL(link.href, location.href);
        samePage = url.origin === location.origin && normPath(url.pathname) === normPath(location.pathname);
      } catch (_) {}
    }
    if (!samePage) return;
    e.preventDefault();
    if (location.hash !== '#content') {
      try { history.replaceState(null, '', '#content'); } catch (_) {}
    }
    if (typeof link.blur === 'function') link.blur();
    requestAnimationFrame(() => requestAnimationFrame(() => scrollToContent(true)));
  });
})();
