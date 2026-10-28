import { useCallback, useReducer, useState } from 'react';
import { createSubmitGuard, formReducer, initialFormState } from './formState';

// Error, busy and stale-result handling for one create modal. The rules live
// in formState.ts (formReducer and createSubmitGuard); this only wires them to React.
export function useModalForm() {
  const [state, dispatch] = useReducer(formReducer, initialFormState);
  // Lazy initial state: created once, not on every render.
  const [guard] = useState(createSubmitGuard);

  const edit = useCallback(() => dispatch({ type: 'edit' }), []);
  const fail = useCallback((errors: string[], invalid: string[] = []) => dispatch({ type: 'fail', errors, invalid }), []);
  const reset = useCallback(() => {
    guard.reset();
    dispatch({ type: 'reset' });
  }, [guard]);
  const begin = useCallback((): number | null => {
    const token = guard.begin();
    if (token !== null) dispatch({ type: 'busy', value: true });
    return token;
  }, [guard]);
  const finish = useCallback((token: number): boolean => {
    const current = guard.finish(token);
    if (current) dispatch({ type: 'busy', value: false });
    return current;
  }, [guard]);

  return { state, edit, fail, reset, begin, finish };
}

export function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error && err.message ? err.message : fallback;
}
