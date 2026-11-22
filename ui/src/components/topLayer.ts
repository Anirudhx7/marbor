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
