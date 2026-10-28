import { useEffect, useRef, type RefObject } from 'react';
import { isFocusIdle, nextFocusState } from '../lib/formState';

// The one place a create modal shows validation and server errors: directly
// above the submit button. The submit button stays enabled; the form shows
// this on submit and clears it when any field is edited. The alert is keyed on
// the failed-submit counter so an identical message is announced again. Focus
// moves once per failed submit, to the first invalid field or to the alert
// itself when the error is not about a field (such as one from the server);
// re-renders with the same attempt never move it.
interface FormErrorProps {
  errors: string[];
  attempt: number;
  id?: string;
  focusFieldId?: string;
  // The row holding Cancel and submit. Focus on a button inside it counts as
  // idle, so a late server error may take focus from the button just clicked.
  actionRowRef?: RefObject<HTMLElement | null>;
}

export function FormError({ errors, attempt, id = 'form-error', focusFieldId, actionRowRef }: FormErrorProps) {
  const ref = useRef<HTMLDivElement>(null);
  const lastFocusedAttempt = useRef(0);
  const hasErrors = errors.length > 0;

  useEffect(() => {
    // A field error is the immediate result of the submit and always takes
    // focus. An error with no field (a server failure) may arrive late, so it
    // only takes focus while focus is idle.
    const field = focusFieldId ? document.getElementById(focusFieldId) : null;
    const active = document.activeElement;
    const inActionRow = !!active && !!actionRowRef?.current?.contains(active);
    const mayFocus = field !== null || isFocusIdle(active?.tagName, inActionRow);
    const next = nextFocusState(lastFocusedAttempt.current, attempt, hasErrors, mayFocus);
    lastFocusedAttempt.current = next.last;
    if (!next.focus) return;
    (field ?? ref.current)?.focus();
  });

  if (!hasErrors) return null;
  return (
    <div
      key={attempt}
      ref={ref}
      id={id}
      role="alert"
      tabIndex={-1}
      className="p-3 bg-destructive/10 border border-destructive/20 rounded-lg focus:outline-none"
    >
      {errors.map((line, i) => (
        <p key={i} className="text-sm font-medium text-destructive">{line}</p>
      ))}
    </div>
  );
}
