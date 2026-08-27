// Deprecated alias: Activity is the canonical fleet timeline at /activity.
// Route /system-audit redirects to /activity?view=audit. This file exists only
// to prevent import breakage and must not add a second fetch or table.
export { Activity as SystemAudit } from './Activity';
