document.querySelectorAll('.copy').forEach((btn) => {
  btn.addEventListener('click', async () => {
    const text = btn.parentElement.querySelector('code').textContent
      .split('\n').filter((l) => !/^\s*#/.test(l)).join('\n').trim();
    try {
      await navigator.clipboard.writeText(text);
      btn.textContent = 'Copied';
      btn.classList.add('done');
      setTimeout(() => { btn.textContent = 'Copy'; btn.classList.remove('done'); }, 1600);
    } catch { btn.textContent = 'Select it'; }
  });
});
