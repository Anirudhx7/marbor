import { X } from 'lucide-react';
import { ReactNode, useEffect, useId, useLayoutEffect, useRef, useState } from 'react';

interface ModalProps {
  isOpen: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
  maxWidth?: 'sm' | 'md' | 'lg' | 'xl' | '2xl' | '3xl' | '4xl';
  // True while a form inside has unsaved changes. Escape, the Close button and
  // a backdrop click then ask before discarding; a clean form closes silently.
  dirty?: boolean;
}

const WIDTHS = { sm: '24rem', md: '28rem', lg: '32rem', xl: '36rem', '2xl': '42rem', '3xl': '48rem', '4xl': '56rem' } as const;

// Matches the exit transition in index.css (.modal-dialog); the dialog is
// unmounted once it has finished fading out.
const EXIT_MS = 160;

// A native modal <dialog>. The browser supplies the inert page, focus
// containment, top layer, ::backdrop, Escape and focus return to the opener,
// so stacked dialogs close one at a time (Escape goes to the topmost) with no
// hand-built stack or focus trap. The dialog grows from the control that
// opened it. Menus and pickers used inside it render into it as popovers
// (see topLayer.ts) so they stay usable.
export function Modal({ isOpen, onClose, title, children, maxWidth = 'md', dirty = false }: ModalProps) {
  const titleId = useId();
  const dialogRef = useRef<HTMLDialogElement>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;
  const isOpenRef = useRef(isOpen);
  isOpenRef.current = isOpen;
  const [confirming, setConfirming] = useState(false);
  // Every way of closing goes through here so the guard cannot be bypassed.
  const requestClose = () => {
    if (dirtyRef.current) setConfirming(true);
    else onCloseRef.current();
  };
  const pressedBackdrop = useRef(false);
  // Rendered while open and for the length of the exit transition.
  const [present, setPresent] = useState(isOpen);
  if (isOpen && !present) setPresent(true);

  useLayoutEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    if (isOpen && !dialog.open) {
      const opener = document.activeElement as HTMLElement | null;
      dialog.showModal();
      // Grow from the opener: its centre, in the dialog's own coordinates.
      if (opener && opener !== document.body) {
        const r = opener.getBoundingClientRect();
        const left = (window.innerWidth - dialog.offsetWidth) / 2;
        const top = (window.innerHeight - dialog.offsetHeight) / 2;
        dialog.style.setProperty('--modal-origin', `${Math.round(r.left + r.width / 2 - left)}px ${Math.round(r.top + r.height / 2 - top)}px`);
      }
      // The safe action in a confirmation (marked data-autofocus), else the
      // dialog itself so Tab starts at its first control.
      (dialog.querySelector<HTMLElement>('[data-autofocus]') ?? dialog).focus({ preventScroll: true });
    } else if (!isOpen && dialog.open) {
      dialog.close();
    }
  }, [isOpen, present]);

  useEffect(() => {
    if (isOpen || !present) return;
    const id = window.setTimeout(() => setPresent(false), EXIT_MS);
    return () => window.clearTimeout(id);
  }, [isOpen, present]);

  // A dialog unmounted while open (callers that render it conditionally) is
  // closed first so the browser returns focus to the opener.
  useLayoutEffect(() => () => {
    const dialog = dialogRef.current;
    if (dialog?.open) dialog.close();
  }, []);

  if (!present) return null;

  return (
    <dialog
      ref={dialogRef}
      aria-labelledby={titleId}
      tabIndex={-1}
      // Escape and the back gesture arrive as a cancel request; the parent
      // owns the open state, so hand it over instead of closing here.
      onCancel={(e) => { e.preventDefault(); e.stopPropagation(); requestClose(); }}
      // The browser only lets a page veto Escape once per user activation, so a
      // second Escape in a row closes the dialog natively and cancel's
      // preventDefault is ignored. Bring the two back in line: a dirty form is
      // shown again with its prompt, a clean one reports the close.
      onClose={() => {
        const dialog = dialogRef.current;
        if (!dialog || dialog.open || !isOpenRef.current) return;
        if (dirtyRef.current) {
          dialog.showModal();
          setConfirming(true);
        } else {
          onCloseRef.current();
        }
      }}
      // A click on the backdrop lands on the dialog element itself. Both the
      // press and the release must be there so a text-selection drag that ends
      // outside does not dismiss it.
      onPointerDown={(e) => { pressedBackdrop.current = e.target === e.currentTarget; }}
      onClick={(e) => { if (pressedBackdrop.current && e.target === e.currentTarget) requestClose(); pressedBackdrop.current = false; }}
      style={{ width: `min(calc(100vw - 2rem), ${WIDTHS[maxWidth]})` }}
      className="modal-dialog m-auto p-0 max-h-[calc(100dvh-2rem)] sm:max-h-[calc(100vh-3rem)] bg-card text-foreground border border-border shadow-xl rounded-xl overflow-hidden min-w-0 focus:outline-none open:flex open:flex-col"
    >
      <div className="px-4 py-3 sm:px-6 sm:py-4 border-b border-border flex items-center justify-between gap-3 shrink-0 min-w-0">
        <h3 id={titleId} className="text-base sm:text-lg font-semibold text-foreground truncate min-w-0">{title}</h3>
        <button
          type="button"
          onClick={requestClose}
          className="p-1 rounded-lg text-muted-foreground hover:bg-secondary hover:text-foreground transition-colors shrink-0 min-w-[40px] min-h-[40px] sm:min-w-0 sm:min-h-0 flex items-center justify-center"
          aria-label="Close"
        >
          <X className="w-5 h-5" />
        </button>
      </div>
      <div className="p-4 sm:p-6 flex-1 overflow-y-auto min-w-0 break-words">
        {children}
      </div>
      <Modal isOpen={confirming} onClose={() => setConfirming(false)} title="Discard changes?" maxWidth="sm">
        <p className="text-sm text-muted-foreground">You have unsaved changes. Closing will lose them.</p>
        <div className="flex justify-end gap-3 pt-4">
          <button
            type="button"
            data-autofocus
            onClick={() => setConfirming(false)}
            className="px-4 py-2 text-sm font-medium rounded-lg bg-secondary text-foreground hover:bg-secondary/70 transition-colors min-h-[44px] sm:min-h-0"
          >
            Keep editing
          </button>
          <button
            type="button"
            onClick={() => { setConfirming(false); onCloseRef.current(); }}
            className="px-4 py-2 text-sm font-medium rounded-lg bg-destructive text-destructive-foreground hover:bg-destructive/90 transition-colors min-h-[44px] sm:min-h-0"
          >
            Discard
          </button>
        </div>
      </Modal>
    </dialog>
  );
}
