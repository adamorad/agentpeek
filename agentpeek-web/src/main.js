import './style.css'

// ── Copy helper ───────────────────────────────────────────────
function attachCopy(btn, getText) {
  btn.addEventListener('click', () => {
    navigator.clipboard.writeText(getText().trim())
      .then(() => {
        btn.textContent = 'Copied!'
        btn.classList.add('copied')
        setTimeout(() => { btn.textContent = 'Copy'; btn.classList.remove('copied') }, 2000)
      })
      .catch(() => {
        btn.textContent = 'Failed'
        setTimeout(() => { btn.textContent = 'Copy' }, 2000)
      })
  })
}

// ── Wire all copy buttons with data-copy attribute ────────────
document.querySelectorAll('.copy-btn[data-copy]').forEach(btn => {
  attachCopy(btn, () => document.getElementById(btn.dataset.copy)?.textContent ?? '')
})
