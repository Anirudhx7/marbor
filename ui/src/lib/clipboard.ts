// Copies text to the clipboard. Uses the async Clipboard API where the page is
// a secure context and falls back to a hidden textarea on plain HTTP, which
// must run synchronously inside a user-gesture handler.
export function copyText(text: string): void {
  const legacyCopy = () => {
    const el = document.createElement('textarea');
    el.value = text;
    el.setAttribute('readonly', '');
    // Off-screen but not opacity:0 - some browsers block copy from invisible elements.
    el.style.cssText = 'position:absolute;left:-9999px;top:auto;width:1px;height:1px';
    document.body.appendChild(el);
    el.focus();
    el.select();
    el.setSelectionRange(0, text.length);
    try {
      document.execCommand('copy');
    } catch {
      // Nothing more can be done silently.
    }
    document.body.removeChild(el);
  };
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(text).catch(legacyCopy);
  } else {
    legacyCopy();
  }
}
