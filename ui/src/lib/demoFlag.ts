// Shared by the demo-mode hook and the API layer so neither imports the other.
export const DEMO_STORAGE_KEY = 'marbor-demo-mode';

// Reads the persisted runtime demo toggle; storage errors read as "off".
export function readStoredDemoFlag(): boolean {
  try {
    return localStorage.getItem(DEMO_STORAGE_KEY) === 'true';
  } catch {
    return false;
  }
}
