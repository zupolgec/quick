// Reveal once on entry, staggered inside each group. Content is visible without JS.
(() => {
  document.querySelectorAll('[data-reveal-group]').forEach((g) => {
    g.querySelectorAll('[data-reveal]').forEach((el, i) => el.style.setProperty('--d', i));
  });
  const targets = document.querySelectorAll('[data-reveal], .film');
  if (!('IntersectionObserver' in window)) { targets.forEach((el) => el.classList.add('is-in')); return; }
  const io = new IntersectionObserver((entries) => {
    for (const e of entries) if (e.isIntersecting) { e.target.classList.add('is-in'); io.unobserve(e.target); }
  }, { rootMargin: '0px 0px -10% 0px', threshold: 0.08 });
  targets.forEach((el) => io.observe(el));
  addEventListener('load', () => setTimeout(() => {
    targets.forEach((el) => { const r = el.getBoundingClientRect(); if (r.top < innerHeight && r.bottom > 0) el.classList.add('is-in'); });
  }, 2500));
})();

// The live thing: name the site, the command and the CLI's answer follow.
(() => {
  const input = document.getElementById('site-name');
  if (!input) return;
  const slots = document.querySelectorAll('[data-name]');
  const clean = (v) => v.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/^-+/, '').slice(0, 40);
  input.addEventListener('input', () => {
    const v = clean(input.value);
    if (v !== input.value) input.value = v;
    const name = v.replace(/-+$/, '') || 'launch';
    slots.forEach((s) => { s.textContent = name; });
  });
})();

// Copy buttons copy the visible command of their panel.
document.querySelectorAll('.copy').forEach((btn) => {
  btn.addEventListener('click', async () => {
    const code = [...btn.parentElement.querySelectorAll('code')].find((c) => !c.hidden);
    try {
      await navigator.clipboard.writeText(code.textContent.trim());
      btn.textContent = 'Copied';
      btn.classList.add('done');
      setTimeout(() => { btn.textContent = 'Copy'; btn.classList.remove('done'); }, 1600);
    } catch { btn.textContent = 'Select it'; }
  });
});

// Install tabs.
document.querySelectorAll('.tabs').forEach((tabs) => {
  const panel = tabs.parentElement;
  const pick = (os) => {
    tabs.querySelectorAll('button').forEach((b) => b.setAttribute('aria-selected', String(b.dataset.os === os)));
    panel.querySelectorAll('[data-os-code]').forEach((c) => { c.hidden = c.dataset.osCode !== os; });
  };
  tabs.addEventListener('click', (e) => { const b = e.target.closest('button'); if (b) pick(b.dataset.os); });
  if (/Win/.test(navigator.platform)) pick('win');
});

// The film stays still for people who asked for less motion.
if (matchMedia('(prefers-reduced-motion: reduce)').matches) {
  document.querySelectorAll('video[autoplay]').forEach((v) => { v.removeAttribute('autoplay'); v.pause(); });
}
