// Error state shared by the create modals (Create API key, Create user, Add
// GPU node). Pure so the open, close, edit and failure rules can be tested
// without a DOM.

export interface FormState {
  // Messages shown above the submit button; empty means no error.
  errors: string[];
  // Keys of the fields the errors are about, for aria-invalid.
  invalid: string[];
  // Counts failed submits, so an identical error is announced again.
  attempt: number;
  // True while a submit is in flight (the button stays enabled, re-entry is ignored).
  busy: boolean;
}

export type FormAction =
  | { type: 'edit' }
  | { type: 'fail'; errors: string[]; invalid: string[] }
  | { type: 'busy'; value: boolean }
  | { type: 'reset' };

export const initialFormState: FormState = { errors: [], invalid: [], attempt: 0, busy: false };

export function formReducer(state: FormState, action: FormAction): FormState {
  switch (action.type) {
    case 'edit':
      // Any field edit clears the error; return the same object when there is nothing to clear.
      return state.errors.length === 0 && state.invalid.length === 0
        ? state
        : { ...state, errors: [], invalid: [] };
    case 'fail':
      // Showing an error says nothing about a request in flight; busy is only
      // changed by 'busy' and 'reset'.
      return { ...state, errors: action.errors, invalid: action.invalid, attempt: state.attempt + 1 };
    case 'busy':
      return { ...state, busy: action.value };
    case 'reset':
      // Opening or closing the modal starts from a clean slate.
      return initialFormState;
  }
}

// Focus moves once per failed submit, not on every re-render while the error
// is showing (the page may re-render on a timer and must not steal focus).
// Returns whether to focus now and the attempt to remember. With no error
// showing the memory resets, because reopening the modal also restarts the
// attempt counter and the next failure must focus again.
// mayFocus is false for an error that arrived late and is not about a field
// (a server failure) while the operator has already moved on: the alert still
// announces it, but focus is not taken.
export function nextFocusState(
  lastFocusedAttempt: number,
  attempt: number,
  hasErrors: boolean,
  mayFocus: boolean = true,
): { focus: boolean; last: number } {
  if (!hasErrors) return { focus: false, last: 0 };
  if (attempt === lastFocusedAttempt) return { focus: false, last: lastFocusedAttempt };
  return { focus: mayFocus, last: attempt };
}

// Focus is "idle" when nothing is focused or it sits on a button in the form's
// action row, such as the submit button just clicked. A field, a select
// trigger, a checkbox or any other control means the operator is working
// elsewhere in the form.
export function isFocusIdle(activeTagName: string | null | undefined, inActionRow: boolean = false): boolean {
  if (!activeTagName || activeTagName === 'BODY') return true;
  return activeTagName === 'BUTTON' && inActionRow;
}

// Synchronous submit bookkeeping for one modal, kept out of React state so a
// double click in the same tick is caught.
//   begin()  - claim a submit; null when one is already running.
//   finish() - release it; false when the modal was closed or reopened since
//              begin(), so a late result must not touch the form.
//   reset()  - modal opened or closed: invalidates every outstanding submit.
export interface SubmitGuard {
  begin(): number | null;
  finish(token: number): boolean;
  reset(): void;
}

export function createSubmitGuard(): SubmitGuard {
  let epoch = 0;
  let busy = false;
  return {
    begin() {
      if (busy) return null;
      busy = true;
      return epoch;
    },
    finish(token) {
      if (token !== epoch) return false;
      busy = false;
      return true;
    },
    reset() {
      epoch += 1;
      busy = false;
    },
  };
}
