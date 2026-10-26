// Required-field checks for the create modals. Each parse function returns
// every problem found (all callers show them all), the keys of the offending
// fields for aria-invalid, and the trimmed values to submit.

export interface Parsed<T> {
  errors: string[];
  invalid: string[];
  values: T;
}

const NON_NEGATIVE_INT = /^\d+$/;

export function parseKeyForm(name: string, rateLimit: string): Parsed<{ name: string; rateLimit: number }> {
  const errors: string[] = [];
  const invalid: string[] = [];
  const trimmedName = name.trim();
  if (!trimmedName) { errors.push('Name is required'); invalid.push('name'); }
  const limitText = rateLimit.trim();
  const parsed = NON_NEGATIVE_INT.test(limitText) ? Number(limitText) : NaN;
  const limit = Number.isSafeInteger(parsed) ? parsed : NaN;
  if (Number.isNaN(limit)) { errors.push('Rate limit must be a non-negative whole number'); invalid.push('rateLimit'); }
  return { errors, invalid, values: { name: trimmedName, rateLimit: limit } };
}

export function parseNodeForm(name: string, host: string): Parsed<{ name: string; host: string }> {
  const errors: string[] = [];
  const invalid: string[] = [];
  const values = { name: name.trim(), host: host.trim() };
  if (!values.name) { errors.push('Node name is required'); invalid.push('name'); }
  if (!values.host) { errors.push('Host is required'); invalid.push('host'); }
  return { errors, invalid, values };
}

export function parseUserForm(username: string): Parsed<{ username: string }> {
  const values = { username: username.trim() };
  return values.username
    ? { errors: [], invalid: [], values }
    : { errors: ['Username is required'], invalid: ['username'], values };
}
