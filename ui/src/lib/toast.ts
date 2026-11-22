// One notification store for the whole dashboard. Outcomes of create, delete
// and save actions land here and are shown by ToastRegion. Wording is fixed per
// action (see the helpers below) so the same action reads the same everywhere.

export interface Toast {
  id: number;
  message: string;
  // A removal that can be reversed offers its undo here.
  undo?: () => void;
}

let toasts: readonly Toast[] = [];
let nextId = 1;
const listeners = new Set<() => void>();

function emit() {
  for (const l of listeners) l();
}

export function subscribeToasts(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

export function getToasts(): readonly Toast[] {
  return toasts;
}

export function dismissToast(id: number) {
  toasts = toasts.filter((t) => t.id !== id);
  emit();
}

const MODAL_OPEN = '[role="dialog"], dialog[open]';

// Toasts are held back while a dialog is open: content outside a modal is
// inert, so a toast shown behind it would be missed. It appears as soon as the
// dialog closes.
export function showToast(message: string, undo?: () => void) {
  const add = () => {
    if (typeof document !== 'undefined' && document.querySelector(MODAL_OPEN)) {
      setTimeout(add, 300);
      return;
    }
    toasts = [...toasts, { id: nextId++, message, undo }];
    emit();
  };
  add();
}

// Fixed wording, one phrase per kind of outcome.
export const toastCreated = (name: string) => showToast(`${name} created.`);
export const toastDeleted = (name: string, undo?: () => void) => showToast(`${name} deleted.`, undo);
export const toastSettingsSaved = () => showToast('Settings saved.');
export const toastWarmupSaved = () => showToast('Warmup saved.');
export const toastKeyCreated = () => showToast('Key created. Copy it now - it will not be shown again.');
