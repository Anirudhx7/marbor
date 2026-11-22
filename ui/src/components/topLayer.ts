import { useEffect } from 'react';

// Menus and pickers render in the top layer as popovers so they are not
// clipped by a dialog's overflow, and into the nearest open dialog when there
// is one: everything outside a modal dialog is inert, so a menu parked in
// <body> would show but ignore clicks.

export function portalTarget(anchor: Element | null | undefined): HTMLElement {
  return (anchor?.closest('dialog') as HTMLElement | null) ?? document.body;
}

// Callback ref for a popover="manual" element: shows it as soon as it mounts.
export function showAsPopover(el: HTMLElement | null) {
  if (!el || typeof el.showPopover !== 'function') return;
  try {
    if (!el.matches(':popover-open')) el.showPopover();
  } catch {
    // Already shown, or not connected yet: nothing to do.
  }
}

// The vertical room a popup may use: the nearest open dialog when the anchor is
// inside one (a popup that pokes out above or below its dialog reads as a
// separate window), else the viewport.
export function popupBounds(anchor: Element | null | undefined): { top: number; bottom: number } {
  const dialog = anchor?.closest('dialog');
  if (dialog) {
    const r = dialog.getBoundingClientRect();
    return { top: Math.max(0, r.top), bottom: Math.min(window.innerHeight, r.bottom) };
  }
  return { top: 0, bottom: window.innerHeight };
}

// While a popup is open, Escape closes the popup only. It is handled in the
// capture phase and swallowed, so an enclosing dialog stays open and the next
// Escape closes the dialog: one layer per press.
export function useEscapeDismiss(isOpen: boolean, close: () => void, restoreFocusTo?: () => HTMLElement | null | undefined) {
  useEffect(() => {
    if (!isOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.stopPropagation();
      e.preventDefault();
      close();
      restoreFocusTo?.()?.focus();
    };
    document.addEventListener('keydown', onKey, true);
    return () => document.removeEventListener('keydown', onKey, true);
    // close and restoreFocusTo are fresh closures each render; the listener only
    // needs to exist while the popup is open.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isOpen]);
}
