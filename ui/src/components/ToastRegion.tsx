import { useEffect, useRef, useState, useSyncExternalStore } from 'react';
import { X } from 'lucide-react';
import { dismissToast, getToasts, subscribeToasts, type Toast } from '../lib/toast';

const AUTO_DISMISS_MS = 8000;
const AUTO_DISMISS_UNDO_MS = 12000;

// The single place toasts appear: a named region held in the top layer with
// popover="manual", so it stays up until a toast is closed or times out and is
// never light-dismissed. Toasts stack inside it. Each is announced through
// ariaNotify where the browser has it, and through an always-present live
// region otherwise.
export function ToastRegion() {
  const toasts = useSyncExternalStore(subscribeToasts, getToasts, getToasts);
  const regionRef = useRef<HTMLDivElement>(null);
  const [announcement, setAnnouncement] = useState('');
  const announced = useRef(new Set<number>());

  useEffect(() => {
    const el = regionRef.current;
    if (!el || typeof el.showPopover !== 'function') return;
    const open = el.matches(':popover-open');
    if (toasts.length > 0 && !open) el.showPopover();
    if (toasts.length === 0 && open) el.hidePopover();
  }, [toasts.length]);

  useEffect(() => {
    for (const t of toasts) {
      if (announced.current.has(t.id)) continue;
      announced.current.add(t.id);
      const text = t.undo ? `${t.message} Actions available` : t.message;
      const notify = (document as unknown as { ariaNotify?: (s: string) => void }).ariaNotify;
      if (typeof notify === 'function') notify.call(document, text);
      else setAnnouncement(text);
    }
  }, [toasts]);

  return (
    <>
      <div className="sr-only" role="status" aria-live="polite" aria-atomic="true">{announcement}</div>
      <div
        ref={regionRef}
        popover="manual"
        role="region"
        aria-label="Notifications"
        className="toast-region"
      >
        {toasts.map((t) => <ToastItem key={t.id} toast={t} />)}
      </div>
    </>
  );
}

function ToastItem({ toast }: { toast: Toast }) {
  const ref = useRef<HTMLDivElement>(null);
  const [paused, setPaused] = useState(false);

  // Timed out unless the pointer or focus is on it; the timer restarts when
  // the user leaves, so a toast being read or used is never taken away.
  useEffect(() => {
    if (paused) return;
    const id = window.setTimeout(() => dismissToast(toast.id), toast.undo ? AUTO_DISMISS_UNDO_MS : AUTO_DISMISS_MS);
    return () => window.clearTimeout(id);
  }, [paused, toast.id, toast.undo]);

  const close = () => {
    // Keep keyboard users where they were: hand focus to the next toast's
    // close button, or let it fall back to the page when none remain.
    const next = ref.current?.nextElementSibling?.querySelector<HTMLElement>('button');
    dismissToast(toast.id);
    next?.focus();
  };

  return (
    <div
      ref={ref}
      className="toast-item"
      onMouseEnter={() => setPaused(true)}
      onMouseLeave={() => setPaused(false)}
      onFocus={() => setPaused(true)}
      onBlur={(e) => { if (!e.currentTarget.contains(e.relatedTarget)) setPaused(false); }}
    >
      <p className="toast-text">{toast.message}</p>
      {toast.undo && (
        <button type="button" className="toast-action" onClick={() => { toast.undo?.(); dismissToast(toast.id); }}>
          Undo
        </button>
      )}
      <button type="button" className="toast-close" aria-label="Close notification" onClick={close}>
        <X className="w-4 h-4" aria-hidden="true" />
      </button>
    </div>
  );
}
