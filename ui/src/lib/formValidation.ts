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

const PORT_TEXT = /^\d{1,5}$/;

// A host is a bare name or address: no scheme, path or port (the scheme and
// port have their own fields). A bracketed IPv6 literal may contain colons.
function isBareHost(host: string): boolean {
  if (/[\s/\\@?#]/.test(host)) return false;
  if (host.startsWith('[') && host.endsWith(']')) return host.length > 2;
  return !host.includes(':');
}

export function parseNodeForm(name: string, host: string, port: string = '11434'): Parsed<{ name: string; host: string; port: number }> {
  const errors: string[] = [];
  const invalid: string[] = [];
  const trimmedName = name.trim();
  const trimmedHost = host.trim();
  const portText = port.trim();
  const portNumber = PORT_TEXT.test(portText) ? Number(portText) : NaN;
  if (!trimmedName) { errors.push('Node name is required'); invalid.push('name'); }
  if (!trimmedHost) {
    errors.push('Host is required'); invalid.push('host');
  } else if (!isBareHost(trimmedHost)) {
    errors.push('Host must be a bare name or address, without http://, a port or a path'); invalid.push('host');
  }
  if (!(portNumber >= 1 && portNumber <= 65535)) { errors.push('Port must be a number from 1 to 65535'); invalid.push('port'); }
  return { errors, invalid, values: { name: trimmedName, host: trimmedHost, port: portNumber } };
}

export function parseUserForm(username: string, email: string = ''): Parsed<{ username: string; email: string }> {
  const values = { username: username.trim(), email: email.trim() };
  return values.username
    ? { errors: [], invalid: [], values }
    : { errors: ['Username is required'], invalid: ['username'], values };
}
