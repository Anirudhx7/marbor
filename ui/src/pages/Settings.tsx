import { useState, useEffect, useRef, useSyncExternalStore } from 'react';
import { useLocation, useNavigate } from 'react-router-dom';
import { Save, Check, Terminal, Shield, Activity, MonitorPlay, RefreshCw, KeyRound, DollarSign, Sliders, Lock, Container, Webhook, Network, Flame, Ruler, Plus, Trash2, Gauge, HardDrive, Download } from 'lucide-react';
import { Modal } from '../components/Modal';
import { defaultSettings, mockModelCatalog } from '../lib/mockData';
import { mockBackupResponse } from '../lib/backupMock';
import { fetchSettings, updateSettings, reloadFromStore, changePassword, triggerBackupNow, downloadStoredBackup, fetchBackupList, restoreBackup, acknowledgeRestoreResult, uploadBackup, deleteBackup, fetchModels } from '../lib/api';
import { getRestoreWatch, subscribeRestoreWatch, startRestoreWatch, resumeRestoreWatch, dismissRestoreWatch, readProcessStart } from '../lib/restoreWatch';
import type { RestoreWatchPhase } from '../lib/restoreWatch';
import { formatBackupSize, backupStatusLines, backupFileNameFor, errorText, isAbortError, pollDelayMs, POLL_GIVE_UP_AFTER, MAX_MEMORY_DOWNLOAD_BYTES } from '../lib/backupUi';
import type { Settings, BackupFileInfo, BackupStatus } from '../types';

import { useDemoMode, currentAppPath } from '../hooks/useDemoMode';
import { useCurrency, CURRENCY_PRESETS } from '../hooks/useCurrency';
import { CustomSelect, CustomCombobox } from '../components/Select';
import { Toggle } from '../components/Toggle';
import { useTimezone } from '../hooks/useTimezone';
import { formatDateTimeInZone } from '../lib/time';
import { notifyTimezoneChanged } from '../hooks/useTimezone';
import { roundPrice } from '../lib/roundPrice';

const DEMO_BACKUP_TITLE = 'Demo mode: backups, downloads, uploads and restores are turned off. The backup data shown here is sample data.';

// The wording for each state of a followed restore (see lib/restoreWatch.ts).
// None of these points at another line of the page: that line may not exist.
const RESTORE_FOLLOW_TEXT: Record<RestoreWatchPhase, string> = {
  restarting: 'Restore accepted - marbor is restarting. This page reloads when it is back.',
  checking: 'The connection dropped while the restore was being requested. marbor may be restarting to finish it - checking...',
  restarted: 'marbor is back. Checking the result of the restore...',
  succeeded: 'The restore succeeded. Reloading...',
  failed: 'The restore failed. marbor is still using the previous database.',
  signin: 'marbor is back but this session has ended. Reloading so you can sign in again...',
  unknown: 'marbor is back, but this page could not confirm the result of the restore. Reload the page and check your data before relying on it.',
  'still-running': 'marbor kept running, so the restore most likely did not start. Try again, or check the marbor log.',
  timeout: 'marbor has not come back within 90 seconds. If it was started by hand, start it again; otherwise check its supervisor and log.',
};



// isValidBindAddress checks a metrics.bind_address value has the host:port
// shape the backend's http.Server.Addr requires, so a typo'd value is caught
// client-side instead of crashing the backend at listen time.
const isValidBindAddress = (value: string): boolean => {
  const trimmed = value.trim();
  if (trimmed === '' || trimmed !== value) return false;
  const idx = trimmed.lastIndexOf(':');
  if (idx <= -1 || idx === trimmed.length - 1) return false;
  const host = trimmed.slice(0, idx);
  const port = trimmed.slice(idx + 1);
  if (host === '' || !/^\d+$/.test(port)) return false;
  const portNum = parseInt(port, 10);
  return portNum >= 1 && portNum <= 65535;
};

// isLoopbackBindAddress reports whether a validated bind address only binds
// the local machine - used to decide whether widening it needs a confirm.
const isLoopbackBindAddress = (value: string): boolean => {
  const host = value.slice(0, value.lastIndexOf(':'));
  return host === '127.0.0.1' || host === 'localhost' || host === '::1';
};

const getTimezoneOffsetMinutes = (tz: string): number => {
  if (tz === 'Local') return -999999;
  try {
    const formatter = new Intl.DateTimeFormat('en-US', {
      timeZone: tz,
      timeZoneName: 'shortOffset'
    });
    const parts = formatter.formatToParts(new Date());
    const offsetPart = parts.find(p => p.type === 'timeZoneName');
    const offset = offsetPart ? offsetPart.value : '';
    if (offset === 'GMT' || offset === 'UTC') return 0;
    const match = offset.match(/(?:GMT|UTC)([+-])(\d+)(?::(\d+))?/);
    if (match) {
      const sign = match[1] === '+' ? 1 : -1;
      const hours = parseInt(match[2], 10);
      const minutes = parseInt(match[3] || '0', 10);
      return sign * (hours * 60 + minutes);
    }
    return 0;
  } catch {
    // A time zone this browser cannot format (unknown name): sort it as UTC.
    return 0;
  }
};

const timezones: string[] = (() => {
  let list = ['Local'];
  try {
    list = ['Local', ...Intl.supportedValuesOf('timeZone')];
  } catch {
    // Older browsers lack Intl.supportedValuesOf: fall back to a short fixed list.
    list = [
      'Local', 'UTC', 'America/New_York', 'America/Los_Angeles', 'America/Chicago',
      'Europe/London', 'Europe/Paris', 'Asia/Kolkata', 'Asia/Tokyo', 'Asia/Shanghai',
      'Asia/Singapore', 'Australia/Sydney'
    ];
  }
  return list.sort((a, b) => {
    const offsetA = getTimezoneOffsetMinutes(a);
    const offsetB = getTimezoneOffsetMinutes(b);
    if (offsetA !== offsetB) {
      return offsetA - offsetB;
    }
    return a.localeCompare(b);
  });
})();

const getTimezoneLabel = (tz: string): string => {
  if (tz === 'Local') return 'Local (Server Timezone)';
  try {
    const formatter = new Intl.DateTimeFormat('en-US', {
      timeZone: tz,
      timeZoneName: 'shortOffset'
    });
    const parts = formatter.formatToParts(new Date());
    const offsetPart = parts.find(p => p.type === 'timeZoneName');
    const offset = offsetPart ? offsetPart.value : '';

    let formattedOffset = offset;
    if (offset === 'GMT' || offset === 'UTC') {
      formattedOffset = 'UTC+00:00';
    } else {
      const match = offset.match(/(?:GMT|UTC)([+-])(\d+)(?::(\d+))?/);
      if (match) {
        const sign = match[1];
        const hours = match[2].padStart(2, '0');
        const minutes = match[3] || '00';
        formattedOffset = `UTC${sign}${hours}:${minutes}`;
      }
    }
    const displayOffset = formattedOffset ? `(${formattedOffset}) ` : '';
    return `${displayOffset}${tz.replace(/_/g, ' ')}`;
  } catch {
    // A time zone this browser cannot format: show its bare name.
    return tz.replace(/_/g, ' ');
  }
};

export function SettingsPage() {
  const tz = useTimezone();
  const { demoMode, setDemoMode } = useDemoMode();
  const location = useLocation();
  const navigate = useNavigate();
  const { currency, setCurrency, toDisplay, toUSD } = useCurrency();
  const roundDisplay = (n: number) => Math.round(n * 100) / 100;
  const [settings, setSettings] = useState<Settings>(defaultSettings);
  // Tracks unsaved edits so the settings-load effect (re-triggered by a
  // Demo mode toggle, among other things) never silently discards them.
  // lastLoadedSettingsRef holds the exact object reference just applied by
  // a load; any onChange handler replaces settings with a brand-new object
  // (spread syntax), so a reference mismatch in the effect below means the
  // operator has an in-progress edit.
  const dirtyRef = useRef(false);
  const lastLoadedSettingsRef = useRef<Settings>(defaultSettings);
  useEffect(() => {
    if (settings !== lastLoadedSettingsRef.current) dirtyRef.current = true;
  }, [settings]);
  const [saved, setSaved] = useState(false);
  const [reloaded, setReloaded] = useState(false);
  const [reloading, setReloading] = useState(false);
  const [reloadConfirmOpen, setReloadConfirmOpen] = useState(false);
  const [demoModeConfirmOpen, setDemoModeConfirmOpen] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Model context windows
  const [newCtxModel, setNewCtxModel] = useState('');
  const [newCtxTokens, setNewCtxTokens] = useState('');

  // Known model names for the searchable context-windows combobox -
  // suggestions only, never enforced: an operator-declared name for a
  // not-yet-pulled or temporarily unavailable model must stay a valid
  // config entry. (Local fallback chain comboboxes moved to Routing.tsx.)
  const [knownModelNames, setKnownModelNames] = useState<string[]>([]);

  useEffect(() => {
    if (currentAppPath() !== '/settings') return;
    if (demoMode) {
      setKnownModelNames((mockModelCatalog.models || []).map((m) => m.name));
      return;
    }
    let active = true;
    fetchModels().then((data) => {
      if (!active || currentAppPath() !== '/settings') return;
      setKnownModelNames((data.models || []).map((m) => m.name));
    }).catch((err: unknown) => {
      // The names only feed suggestions; an empty list still lets a model name
      // be typed by hand, so a failure here is not shown to the operator. It is
      // logged so a broken model list can still be diagnosed.
      console.warn('Settings: could not load known model names', err);
    });
    return () => { active = false; };
  }, [demoMode, location.pathname]);

  // Admin credentials change
  const [credCurrentPw, setCredCurrentPw] = useState('');
  const [credNewPw, setCredNewPw] = useState('');
  const [credConfirmPw, setCredConfirmPw] = useState('');
  const [credSaving, setCredSaving] = useState(false);
  const [credError, setCredError] = useState<string | null>(null);
  const [credSaved, setCredSaved] = useState(false);

  // Backup & restore
  const [backupDownloading, setBackupDownloading] = useState(false);
  const [backupDownloadError, setBackupDownloadError] = useState<string | null>(null);
  const [backupDownloadNotice, setBackupDownloadNotice] = useState<string | null>(null);

  // Set false on unmount so a request that finishes after the operator left the
  // page never touches state, and every transfer still running is cancelled (no
  // file is offered for saving after the operator navigated away).
  const mountedRef = useRef(true);
  const backupAbortRef = useRef<AbortController | null>(null);
  const storedAbortRef = useRef<AbortController | null>(null);
  const uploadAbortRef = useRef<AbortController | null>(null);
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      backupAbortRef.current?.abort();
      storedAbortRef.current?.abort();
      uploadAbortRef.current?.abort();
    };
  }, []);

  // A restore asked for earlier is followed by a module-level watcher, so it
  // survives leaving this page and a reload of the tab.
  const restoreWatch = useSyncExternalStore(subscribeRestoreWatch, getRestoreWatch);
  useEffect(() => {
    resumeRestoreWatch();
  }, []);

  // Restore-from-backup picker
  const [backupList, setBackupList] = useState<BackupFileInfo[]>([]);
  const [backupStatus, setBackupStatus] = useState<BackupStatus | null>(null);
  const [backupSkipped, setBackupSkipped] = useState(0);
  const [backupListLoading, setBackupListLoading] = useState(false);
  // backupListError: the first load failed and nothing is on screen yet.
  // backupStale: a later refresh failed while the earlier list is still shown.
  const [backupListError, setBackupListError] = useState<string | null>(null);
  const [backupStale, setBackupStale] = useState<string | null>(null);
  const [selectedBackupName, setSelectedBackupName] = useState<string>('');
  const [restoreTarget, setRestoreTarget] = useState<BackupFileInfo | null>(null);
  const [restoring, setRestoring] = useState(false);
  const [restoreError, setRestoreError] = useState<string | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<BackupFileInfo | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [deleteError, setDeleteError] = useState<string | null>(null);
  // How many status refreshes in a row failed, and whether the automatic
  // refresh gave up. retryClicked turns the failure line into an alert once the
  // operator has asked for another try.
  const failuresRef = useRef(0);
  const [pollFailures, setPollFailures] = useState(0);
  const [pollGaveUp, setPollGaveUp] = useState(false);
  const [retryClicked, setRetryClicked] = useState(false);
  const lastStatusRef = useRef<BackupStatus | null>(null);
  // The on-demand attempt whose file did not reach this computer (cancelled or
  // failed in the browser), identified by its attempt time.
  const [notSavedAttempt, setNotSavedAttempt] = useState<string | null>(null);

  // Guards against two list fetches (mount, an upload's refetch, the "running"
  // poll, a backup finishing) resolving out of order - only the result of the
  // most recently issued fetch is ever applied to state. The in-flight flag lets
  // the poll skip a tick instead of superseding a request that is still running.
  const backupListRequestId = useRef(0);
  const backupListInFlight = useRef(false);
  const backupLoadedRef = useRef(false);

  // loadBackups reads the backup list and status through the guard above. The
  // demo (build-time or the runtime toggle) reads the sample data instead of
  // the server. quiet skips the "Loading" placeholder, for refreshes of a list
  // that is already on screen. poll marks a timer tick, which is skipped while
  // another read is in flight. Returns whether this call's result was applied.
  const loadBackups = async (opts: { quiet?: boolean; select?: string; poll?: boolean } = {}): Promise<boolean> => {
    if (!mountedRef.current) return false;
    if (opts.poll && backupListInFlight.current) return false;
    const requestId = ++backupListRequestId.current;
    backupListInFlight.current = true;
    if (!opts.quiet) setBackupListLoading(true);
    try {
      const r = demoMode ? mockBackupResponse() : await fetchBackupList();
      if (!mountedRef.current || requestId !== backupListRequestId.current) return false;
      backupLoadedRef.current = true;
      failuresRef.current = 0;
      lastStatusRef.current = r.status;
      setPollFailures(0);
      setPollGaveUp(false);
      setRetryClicked(false);
      setBackupList(r.backups);
      setBackupStatus(r.status);
      setBackupSkipped(r.skipped);
      setBackupListError(null);
      setBackupStale(null);
      setSelectedBackupName(prev => {
        if (opts.select && r.backups.some(b => b.name === opts.select)) return opts.select;
        return (prev && r.backups.some(b => b.name === prev)) ? prev : (r.backups[0]?.name || '');
      });
      return true;
    } catch (err) {
      if (!mountedRef.current || requestId !== backupListRequestId.current) return false;
      const message = errorText(err, 'Could not load the backup list.');
      failuresRef.current += 1;
      setPollFailures(failuresRef.current);
      if (backupLoadedRef.current) setBackupStale(message);
      else setBackupListError(message);
      return false;
    } finally {
      if (requestId === backupListRequestId.current) {
        backupListInFlight.current = false;
        if (mountedRef.current) setBackupListLoading(false);
      }
    }
  };

  useEffect(() => {
    if (currentAppPath() !== '/settings') return;
    backupLoadedRef.current = false;
    setBackupStale(null);
    void loadBackups();
    // Invalidate this effect's request when the page or mode changes.
    return () => {
      backupListRequestId.current++;
      backupListInFlight.current = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [demoMode, location.pathname]);

  // While a backup is running (a scheduled run, or one started elsewhere) the
  // status is refreshed every few seconds so "running" clears by itself. Ticks
  // pause while the tab is hidden and catch up when it is shown again. After a
  // failed refresh the wait doubles (up to 30 s); after POLL_GIVE_UP_AFTER in a
  // row the automatic refresh stops and the operator is told.
  const backupRunning = backupStatus?.running === true;
  useEffect(() => {
    if (!backupRunning || demoMode || pollGaveUp) return;
    let cancelled = false;
    let timer: number | undefined;
    const schedule = () => {
      timer = window.setTimeout(async () => {
        if (cancelled) return;
        if (!document.hidden) await loadBackups({ quiet: true, poll: true });
        if (cancelled) return;
        if (failuresRef.current >= POLL_GIVE_UP_AFTER) {
          setPollGaveUp(true);
          return;
        }
        schedule();
      }, pollDelayMs(failuresRef.current));
    };
    const onVisible = () => {
      if (document.hidden || cancelled) return;
      window.clearTimeout(timer);
      void loadBackups({ quiet: true, poll: true }).then(() => {
        if (!cancelled) schedule();
      });
    };
    schedule();
    document.addEventListener('visibilitychange', onVisible);
    return () => {
      cancelled = true;
      window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [backupRunning, demoMode, pollGaveUp]);

  const handleBackupNow = async () => {
    if (backupDownloading || demoMode) return;
    setBackupDownloadError(null);
    setBackupDownloadNotice(null);
    const ctl = new AbortController();
    backupAbortRef.current = ctl;
    setBackupDownloading(true);
    let saved = false;
    try {
      const result = await triggerBackupNow(backupFileNameFor(new Date()), ctl.signal);
      if (!mountedRef.current) return;
      if (result.kind === 'cancelled') {
        setBackupDownloadNotice('The backup was not saved to this computer.');
      } else {
        saved = true;
        setBackupDownloadNotice(`Backup saved as ${result.name} in your browser's downloads folder.${result.note ? ` ${result.note}` : ''}`);
      }
    } catch (err) {
      if (!mountedRef.current) return;
      if (isAbortError(err)) setBackupDownloadNotice('Cancelled. The backup was not saved to this computer.');
      else setBackupDownloadError(errorText(err, 'The backup failed.'));
    } finally {
      if (backupAbortRef.current === ctl) backupAbortRef.current = null;
      if (mountedRef.current) {
        setBackupDownloading(false);
        // The run changed the status (success, refusal or failure): refresh it
        // through the guarded loader so the card shows what actually happened.
        const applied = await loadBackups({ quiet: true });
        if (mountedRef.current) {
          setNotSavedAttempt(!saved && applied ? (lastStatusRef.current?.manual?.attempt_at ?? '') : null);
        }
      }
    }
  };

  const selectedBackup = backupList.find(b => b.name === selectedBackupName) || null;

  const [storedDownloading, setStoredDownloading] = useState(false);
  const [storedDownloadError, setStoredDownloadError] = useState<string | null>(null);
  const [storedDownloadNotice, setStoredDownloadNotice] = useState<string | null>(null);

  const handleStoredDownload = async () => {
    if (!selectedBackup || storedDownloading || uploadingBackup || demoMode) return;
    setStoredDownloadError(null);
    setStoredDownloadNotice(null);
    const ctl = new AbortController();
    storedAbortRef.current = ctl;
    setStoredDownloading(true);
    try {
      const result = await downloadStoredBackup(selectedBackup.name, ctl.signal);
      if (!mountedRef.current) return;
      if (result.kind === 'cancelled') setStoredDownloadNotice('Save cancelled.');
      else setStoredDownloadNotice(`Saved ${result.name}.${result.note ? ` ${result.note}` : ''}`);
    } catch (err) {
      if (!mountedRef.current) return;
      if (isAbortError(err)) setStoredDownloadNotice('Download cancelled. Nothing was saved.');
      else setStoredDownloadError(errorText(err, 'The download failed.'));
    } finally {
      if (storedAbortRef.current === ctl) storedAbortRef.current = null;
      if (mountedRef.current) setStoredDownloading(false);
    }
  };

  const [uploadingBackup, setUploadingBackup] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const [uploadNotice, setUploadNotice] = useState<string | null>(null);
  const [ackingRestore, setAckingRestore] = useState(false);
  const [ackError, setAckError] = useState<string | null>(null);

  // Forget the remembered restore result once the operator has read it.
  const handleAckRestore = async () => {
    setAckError(null);
    setAckingRestore(true);
    try {
      await acknowledgeRestoreResult(backupStatus?.last_restore?.ref);
      if (mountedRef.current) {
        const applied = await loadBackups({ quiet: true });
        if (mountedRef.current && !applied) {
          setAckError('The result was cleared, but the status could not be refreshed. Use Try again to reload it.');
        }
      }
    } catch (err) {
      if (mountedRef.current) {
        setAckError(errorText(err, 'Could not clear the restore result.'));
        // A different result may have replaced the one that was shown.
        void loadBackups({ quiet: true });
      }
    } finally {
      if (mountedRef.current) setAckingRestore(false);
    }
  };
  const uploadInputRef = useRef<HTMLInputElement>(null);

  const handleBackupFileChosen = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = ''; // allow re-selecting the same file later
    if (!file || demoMode || storedDownloading) return;
    setUploadError(null);
    setUploadNotice(null);
    const ctl = new AbortController();
    uploadAbortRef.current = ctl;
    setUploadingBackup(true);
    try {
      // uploadBackup vets the extension and size before sending anything.
      const { filename, duplicate, warning } = await uploadBackup(file, ctl.signal);
      // Select the stored file only when the refetch still applies; a refetch
      // that fails is shown by the list's own stale line.
      const applied = await loadBackups({ quiet: true, select: filename });
      const parts: string[] = [];
      if (applied && duplicate) parts.push(`This is the same backup as ${filename}, already in the list - selected it instead of adding a duplicate.`);
      if (warning) parts.push(warning);
      if (mountedRef.current && parts.length > 0) setUploadNotice(parts.join(' '));
    } catch (err) {
      if (!mountedRef.current) return;
      if (isAbortError(err)) {
        setUploadNotice('Upload cancelled. If the file had already finished arriving it is in the list.');
        void loadBackups({ quiet: true });
      } else {
        setUploadError(errorText(err, 'The upload failed.'));
      }
    } finally {
      if (uploadAbortRef.current === ctl) uploadAbortRef.current = null;
      if (mountedRef.current) setUploadingBackup(false);
    }
  };

  const handleDeleteConfirm = async () => {
    if (!deleteTarget || demoMode) return;
    setDeleteError(null);
    setDeleting(true);
    try {
      await deleteBackup(deleteTarget.name);
      if (!mountedRef.current) return;
      setDeleteTarget(null);
      void loadBackups({ quiet: true });
    } catch (err) {
      if (mountedRef.current) {
        setDeleteError(errorText(err, 'Could not delete the backup.'));
        void loadBackups({ quiet: true });
      }
    } finally {
      if (mountedRef.current) setDeleting(false);
    }
  };

  const handleRestoreConfirm = async () => {
    if (!restoreTarget || demoMode) return;
    setRestoreError(null);
    setRestoring(true);
    try {
      // The process identifier is read first so a restart can be told from the
      // old process still shutting down.
      const before = await readProcessStart();
      const { outcome, ref } = await restoreBackup(restoreTarget.name);
      // The watcher outlives this page, so it starts even if the page is gone.
      startRestoreWatch(before, ref, outcome === 'accepted' ? 'restarting' : 'checking');
      if (!mountedRef.current) return;
      setRestoreTarget(null);
    } catch (err) {
      if (mountedRef.current) setRestoreError(errorText(err, 'The restore failed.'));
    } finally {
      if (mountedRef.current) setRestoring(false);
    }
  };

  const closeRestoreDialog = () => {
    if (restoring) return;
    setRestoreTarget(null);
    setRestoreError(null);
  };

  useEffect(() => {
    if (currentAppPath() !== '/settings') return;
    let active = true;
    fetchSettings()
      .then((settingsData) => {
        if (!active || currentAppPath() !== '/settings') return;
        const loaded: Settings = {
          proxyPort: settingsData.proxy?.port || 11434,
          authMode: settingsData.auth?.enabled ? 'api-key' : 'no-auth',
          liteLLMEnabled: settingsData.litellm?.enabled || false,
          liteLLMEndpoint: settingsData.litellm?.url || '',
          liteLLMApiKey: settingsData.litellm?.api_key || '',
          pollingInterval: settingsData.routing?.poll_interval_ms || 2000,
          prometheusEnabled: settingsData.metrics?.enabled || false,
          prometheusPort: settingsData.metrics?.port || 9090,
          prometheusBindAddress: settingsData.metrics?.bind_address || '127.0.0.1:9090',
          logLevel: settingsData.proxy?.log_level || 'info',
          timezone: settingsData.timezone || 'Local',
          cloudDailyUsdCap: settingsData.cloud_budget?.daily_usd_cap || 0,
          cloudMonthlyUsdCap: settingsData.cloud_budget?.monthly_usd_cap || 0,
          cloudSoftBudgetPct: settingsData.cloud_budget?.soft_budget_pct || 0,
          hideDemoBanner: settingsData.hide_demo_banner || false,
          hideBudgetBanner: settingsData.hide_budget_banner || false,
          huggingFaceToken: settingsData.huggingface?.token || '',
          allowManagementEndpoints: settingsData.routing?.allow_management_endpoints || false,

          adminBindAddress: settingsData.admin?.bind_address || ':8080',
          adminCorsOrigin: settingsData.admin?.cors_origin || '',
          proxyAccessLog: settingsData.proxy?.access_log !== false,
          proxyTrustProxyHeaders: settingsData.proxy?.trust_proxy_headers || false,

          routingFallback: settingsData.routing?.fallback || 'least-connections',
          routingUpstreamTimeoutMs: settingsData.routing?.upstream_timeout_ms || 120000,
          routingMaxRetries: settingsData.routing?.max_retries ?? 2,
          routingSessionAffinity: settingsData.routing?.session_affinity || false,
          routingSessionAffinityTtl: settingsData.routing?.session_affinity_ttl || '10m',
          routingNvidiaPollIntervalMs: settingsData.routing?.nvidia_poll_interval_ms || 30000,
          routingQueueMaxDepth: settingsData.routing?.queue_max_depth ?? 100,
          routingQueueTimeoutMs: settingsData.routing?.queue_timeout_ms || 30000,
          routingHealthFailureThreshold: settingsData.routing?.health_failure_threshold ?? 3,
          routingHealthSuccessThreshold: settingsData.routing?.health_success_threshold ?? 2,
          routingOverflowSlaMs: settingsData.routing?.overflow_sla_ms ?? 0,
          routingMaxInFlightPerNode: settingsData.routing?.max_in_flight_per_node ?? 0,
          thermalWatchdogEnabled: settingsData.routing?.thermal_watchdog?.enabled || false,
          thermalWatchdogMaxTempCelsius: settingsData.routing?.thermal_watchdog?.max_temp_celsius || 0,
          thermalWatchdogConsecutiveBreaches: settingsData.routing?.thermal_watchdog?.consecutive_breaches ?? 3,
          routingPrefixLocalityEnabled: settingsData.routing?.prefix_locality_enabled || false,
          routingPrefixLocalityWeight: settingsData.routing?.prefix_locality_weight ?? 10,

          dockerEnabled: settingsData.docker?.enabled || false,
          dockerSocket: settingsData.docker?.socket || '',
          dockerPollIntervalMs: settingsData.docker?.poll_interval_ms ?? 30000,

          auditEnabled: settingsData.audit?.enabled || false,
          auditRetentionDays: settingsData.audit?.retention_days ?? 30,
          systemAuditRetentionDays: settingsData.audit?.system_audit_retention_days ?? 0,
          webhookEnabled: settingsData.webhook?.enabled || false,
          webhookUrl: settingsData.webhook?.url || '',
          webhookSecret: settingsData.webhook?.secret || '',
          savingsReferenceCostPer1k: roundPrice(settingsData.savings?.reference_cost_per_1k ?? 0.002),

          warmupEnabled: settingsData.warmup?.enabled || false,
          warmupIntervalMs: settingsData.warmup?.interval_ms ?? 300000,
          warmupKeepAlive: settingsData.warmup?.keep_alive || '10m',

          contextWindows: settingsData.context_windows || {},
          // Local fallback chains are owned by the Routing page now and are
          // never sent from a Settings save (the backend merges partial
          // payloads, so omitting them preserves Routing-side edits).

          backupEnabled: settingsData.backup?.enabled || false,
          backupIntervalHours: settingsData.backup?.interval_hours ?? 24,
          backupRetentionCount: settingsData.backup?.retention_count ?? 7,
          backupTargetDir: settingsData.backup?.target_dir || '',
          backupLastAt: settingsData.backup?.last_backup_at || undefined,
          backupLastError: settingsData.backup?.last_backup_error || '',
        };
        if (dirtyRef.current) {
          // An edit is in progress (e.g. this reload was triggered by a
          // Demo mode toggle mid-edit) - never clobber it silently. The
          // fresh server-side values are simply not applied; the operator's
          // pending edits stay on screen until they save or reload manually.
          lastLoadedSettingsRef.current = loaded;
        } else {
          lastLoadedSettingsRef.current = loaded;
          setSettings(loaded);
        }
        setError(null);
      })
      .catch(err => {
        if (!active || currentAppPath() !== '/settings') return;
        setError(err.message || 'Failed to load settings');
      });
    return () => {
      active = false;
    };
  }, [demoMode, location.pathname]);

  const handleSave = async () => {
    if (settings.prometheusEnabled && !isValidBindAddress(settings.prometheusBindAddress)) {
      setError('Metrics bind address must be a valid host:port (e.g. 127.0.0.1:9090).');
      return;
    }
    if (
      settings.prometheusEnabled &&
      !isLoopbackBindAddress(settings.prometheusBindAddress) &&
      !window.confirm(
        `This will expose the unauthenticated metrics endpoint on ${settings.prometheusBindAddress}, reachable by anyone who can reach that address. Continue?`
      )
    ) {
      return;
    }
    try {
      // Map UI settings to backend config format (also used in demo mode → localStorage)
      const payload = {
        timezone: settings.timezone,
        proxy: { port: settings.proxyPort, log_level: settings.logLevel, access_log: settings.proxyAccessLog, trust_proxy_headers: settings.proxyTrustProxyHeaders },
        admin: { bind_address: settings.adminBindAddress, cors_origin: settings.adminCorsOrigin },
        auth: { enabled: settings.authMode === 'api-key' },
        routing: {
          poll_interval_ms: settings.pollingInterval,
          allow_management_endpoints: settings.allowManagementEndpoints || false,
          fallback: settings.routingFallback,
          upstream_timeout_ms: settings.routingUpstreamTimeoutMs,
          max_retries: settings.routingMaxRetries,
          session_affinity: settings.routingSessionAffinity,
          session_affinity_ttl: settings.routingSessionAffinityTtl,
          nvidia_poll_interval_ms: settings.routingNvidiaPollIntervalMs,
          queue_max_depth: settings.routingQueueMaxDepth,
          queue_timeout_ms: settings.routingQueueTimeoutMs,
          health_failure_threshold: settings.routingHealthFailureThreshold,
          health_success_threshold: settings.routingHealthSuccessThreshold,
          overflow_sla_ms: settings.routingOverflowSlaMs,
          max_in_flight_per_node: settings.routingMaxInFlightPerNode,
          thermal_watchdog: {
            enabled: settings.thermalWatchdogEnabled,
            max_temp_celsius: settings.thermalWatchdogMaxTempCelsius,
            consecutive_breaches: settings.thermalWatchdogConsecutiveBreaches,
          },
          prefix_locality_enabled: settings.routingPrefixLocalityEnabled,
          prefix_locality_weight: settings.routingPrefixLocalityWeight,
        },
        metrics: { enabled: settings.prometheusEnabled, port: settings.prometheusPort, bind_address: settings.prometheusBindAddress },
        litellm: { enabled: settings.liteLLMEnabled, url: settings.liteLLMEndpoint, api_key: settings.liteLLMApiKey },
        huggingface: { token: settings.huggingFaceToken || '' },
        cloud_budget: { daily_usd_cap: settings.cloudDailyUsdCap, monthly_usd_cap: settings.cloudMonthlyUsdCap, soft_budget_pct: settings.cloudSoftBudgetPct },
        hide_demo_banner: settings.hideDemoBanner || false,
        hide_budget_banner: settings.hideBudgetBanner || false,
        docker: { enabled: settings.dockerEnabled, socket: settings.dockerSocket, poll_interval_ms: settings.dockerPollIntervalMs },
        audit: {
          enabled: settings.auditEnabled,
          retention_days: settings.auditRetentionDays,
          system_audit_retention_days: settings.systemAuditRetentionDays,
        },
        webhook: { enabled: settings.webhookEnabled, url: settings.webhookUrl, secret: settings.webhookSecret },
        savings: { reference_cost_per_1k: settings.savingsReferenceCostPer1k },
        warmup: { enabled: settings.warmupEnabled, interval_ms: settings.warmupIntervalMs, keep_alive: settings.warmupKeepAlive },
        context_windows: settings.contextWindows,
        backup: {
          enabled: settings.backupEnabled,
          interval_hours: settings.backupIntervalHours,
          retention_count: settings.backupRetentionCount,
          target_dir: settings.backupTargetDir,
        },
      };

      await updateSettings(payload);
      window.dispatchEvent(new Event('marbor-settings-change'));
      // Wake TimezoneProvider instantly so Activity/etc re-render without waiting
      // for the 15s poll (instant re-render is a required behavior here).
      notifyTimezoneChanged();
      // The just-saved values are now the pristine baseline - clear the
      // dirty flag so a subsequent settings reload (e.g. a Demo mode
      // toggle) is free to apply fresh server-side values again.
      lastLoadedSettingsRef.current = settings;
      dirtyRef.current = false;
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
      setError(null);
    } catch (err: any) {
      setError(err instanceof TypeError
        ? 'Could not reach the marbor backend. Check that the process is running and reachable from this browser.'
        : (err.message || 'Failed to save settings'));
    }
  };

  const handleReload = async () => {
    if (demoMode) {
      setReloaded(true);
      setReloadConfirmOpen(false);
      setTimeout(() => setReloaded(false), 2000);
      return;
    }
    setReloading(true);
    try {
      await reloadFromStore();
      setReloaded(true);
      setError(null);
      setTimeout(() => setReloaded(false), 2000);
    } catch (err: any) {
      setError(err instanceof TypeError
        ? 'Could not reach the marbor backend. Check that the process is running and reachable from this browser.'
        : (err.message || 'Reload failed'));
    } finally {
      setReloading(false);
      setReloadConfirmOpen(false);
    }
  };

  const handleChangeCredentials = async () => {
    if (credNewPw && credNewPw !== credConfirmPw) {
      setCredError('New passwords do not match');
      return;
    }
    if (!credCurrentPw) {
      setCredError('Current password is required');
      return;
    }
    setCredSaving(true);
    setCredError(null);
    try {
      await changePassword(credCurrentPw, credNewPw || '');
      setCredSaved(true);
      setCredCurrentPw('');
      setCredNewPw('');
      setCredConfirmPw('');
      setTimeout(() => setCredSaved(false), 3000);
    } catch (err: any) {
      setCredError(err.message || 'Failed to update credentials');
    } finally {
      setCredSaving(false);
    }
  };

  return (
    <>
    <div className="space-y-6 animate-fade-in max-w-7xl mx-auto">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 border-b border-border pb-6">
        <div>
          <h1 className="text-2xl font-bold tracking-tight text-foreground">Settings</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Configure proxy settings, authentication, and integrations
          </p>
        </div>
        <div className="flex items-center gap-2">
          <button
            onClick={() => setReloadConfirmOpen(true)}
            disabled={reloading}
            title="Reload config from disk without restarting (equivalent to SIGHUP)"
            className="flex items-center gap-2 px-4 py-2 bg-secondary hover:bg-secondary/80 disabled:opacity-50 text-foreground font-medium rounded-lg transition-colors border border-border"
          >
            {reloaded ? (
              <>
                <Check className="w-4 h-4 text-primary" />
                Reloaded
              </>
            ) : (
              <>
                <RefreshCw className={`w-4 h-4 ${reloading ? 'animate-spin' : ''}`} />
                Reload Config
              </>
            )}
          </button>
          <button
            onClick={handleSave}
            className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground font-medium rounded-lg transition-colors shadow-sm"
          >
            {saved ? (
              <>
                <Check className="w-4 h-4" />
                Saved
              </>
            ) : (
              <>
                <Save className="w-4 h-4" />
                Save Changes
              </>
            )}
          </button>
        </div>
      </div>

      {error && (
        <div className="p-4 bg-destructive/10 border border-destructive/20 rounded-xl text-destructive text-sm font-medium">
          {error}
        </div>
      )}

      {/* Demo mode Toggle */}
      <div className="bg-card border border-border shadow-sm rounded-xl p-6">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-3">
            <div className="p-2 bg-amber-500/10 rounded-lg">
              <MonitorPlay className="w-5 h-5 text-amber-600 dark:text-amber-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Demo mode</h3>
              <p className="text-xs font-medium text-muted-foreground">Use mock data for testing UI without a real backend</p>
            </div>
          </div>
          <button
            type="button" role="switch" aria-checked={demoMode} aria-label="Demo mode"
            onClick={() => demoMode ? setDemoMode(false) : setDemoModeConfirmOpen(true)}
            className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${
              demoMode ? 'bg-amber-500' : 'bg-muted-foreground/30'
            }`}
          >
            <span
              className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                demoMode ? 'translate-x-6' : 'translate-x-1'
              }`}
            />
          </button>
        </div>
      </div>

      {/* Hardware benchmark - hidden diagnostic page, no Sidebar entry */}
      <div className="bg-card border border-border shadow-sm rounded-xl p-6">
        <div className="flex items-center justify-between gap-4">
          <div className="flex items-center gap-3">
            <div className="p-2 bg-primary/10 rounded-lg">
              <Gauge className="w-5 h-5 text-primary" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Hardware benchmark</h3>
              <p className="text-xs font-medium text-muted-foreground">Measure real cold-vs-warm TTFT on your own hardware, through this marbor</p>
            </div>
          </div>
          <button
            onClick={() => navigate('/benchmark')}
            className="px-4 py-2 bg-secondary hover:bg-secondary/80 text-foreground text-sm font-medium rounded-lg transition-colors border border-border shrink-0"
          >
            Open
          </button>
        </div>
      </div>

      <div className="space-y-8 pt-2">
        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">General</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* Proxy Settings */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-primary/10 rounded-lg">
              <Terminal className="w-5 h-5 text-primary" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Proxy configuration</h3>
              <p className="text-xs font-medium text-muted-foreground">Core proxy server settings</p>
            </div>
          </div>

          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Proxy Port
              </label>
              <input
                type="number"
                value={settings.proxyPort}
                onChange={(e) => setSettings({ ...settings, proxyPort: parseInt(e.target.value) || settings.proxyPort })}
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
              />
              <p className="text-[10px] text-amber-700 dark:text-amber-400 mt-1">
                Requires a marbor restart to take effect - saving here only stores the new port.
              </p>
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Timezone
              </label>
              <CustomSelect
                value={settings.timezone}
                onChange={(val) => setSettings({ ...settings, timezone: val })}
                options={timezones.map(tz => ({ value: tz, label: getTimezoneLabel(tz) }))}
              />
              <p className="text-[10px] text-muted-foreground mt-1">
                Scheduler and prediction cycles will evaluate relative to this timezone.
              </p>
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-2">
                Authentication Mode
              </label>
              <div className="space-y-2">
                <label className="flex items-center gap-3 p-3 rounded-lg border border-border bg-secondary/30 cursor-pointer hover:border-primary/40 transition-colors">
                  <input
                    type="radio"
                    name="authMode"
                    checked={settings.authMode === 'api-key'}
                    onChange={() => setSettings({ ...settings, authMode: 'api-key' })}
                    className="accent-primary"
                  />
                  <div>
                    <p className="text-sm font-medium text-foreground">API Key Authentication</p>
                    <p className="text-xs text-muted-foreground">Require valid API key for all requests</p>
                  </div>
                </label>
                <label className="flex items-center gap-3 p-3 rounded-lg border border-border bg-secondary/30 cursor-pointer hover:border-primary/40 transition-colors">
                  <input
                    type="radio"
                    name="authMode"
                    checked={settings.authMode === 'no-auth'}
                    onChange={() => setSettings({ ...settings, authMode: 'no-auth' })}
                    className="accent-primary"
                  />
                  <div>
                    <p className="text-sm font-medium text-foreground">No authentication</p>
                    <p className="text-xs text-muted-foreground">Allow all requests (development only)</p>
                  </div>
                </label>
              </div>
            </div>
          </div>
        </div>

        {/* Dashboard preferences */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-indigo-500/10 rounded-lg">
              <Sliders className="w-5 h-5 text-indigo-600 dark:text-indigo-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Dashboard preferences</h3>
              <p className="text-xs font-medium text-muted-foreground">Customize UI warning banners visibility</p>
            </div>
          </div>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Hide demo banner</p>
                <p className="text-xs text-muted-foreground">Do not show warning banner in demo mode</p>
              </div>
              <button
                type="button" role="switch" aria-checked={settings.hideDemoBanner} aria-label="Hide demo banner"
                onClick={() => setSettings({ ...settings, hideDemoBanner: !settings.hideDemoBanner })}
                className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${
                  settings.hideDemoBanner ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.hideDemoBanner ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>

            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Hide budget banner</p>
                <p className="text-xs text-muted-foreground">Do not show cloud spend warning banners</p>
              </div>
              <button
                type="button" role="switch" aria-checked={settings.hideBudgetBanner} aria-label="Hide budget banner"
                onClick={() => setSettings({ ...settings, hideBudgetBanner: !settings.hideBudgetBanner })}
                className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${
                  settings.hideBudgetBanner ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.hideBudgetBanner ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>
          </div>
        </div>

          </div>
        </section>

        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">Security & Access</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* Admin & security */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-rose-500/10 rounded-lg">
              <Lock className="w-5 h-5 text-rose-600 dark:text-rose-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Admin & security</h3>
              <p className="text-xs font-medium text-muted-foreground">Dashboard listen address and CORS - takes effect on next restart</p>
            </div>
          </div>
          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Admin Bind Address</label>
              <input type="text" value={settings.adminBindAddress} onChange={(e) => setSettings({ ...settings, adminBindAddress: e.target.value })} placeholder=":8080" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
              <p className="text-[10px] text-muted-foreground mt-1">
                Use 127.0.0.1:8080 to restrict the dashboard to localhost.
              </p>
              <p className="text-[10px] text-amber-700 dark:text-amber-400 mt-1">
                Requires a marbor restart to take effect - changing this can lock you out until you reach it via the new address.
              </p>
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">CORS Origin</label>
              <input type="text" value={settings.adminCorsOrigin} onChange={(e) => setSettings({ ...settings, adminCorsOrigin: e.target.value })} placeholder="https://your-frontend.example.com" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
              <p className="text-[10px] text-muted-foreground mt-1">Leave blank for same-origin only. Must be one concrete origin, not "*".</p>
            </div>
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Proxy access log</p>
                <p className="text-xs text-muted-foreground">Structured JSON access-log line per request on stdout</p>
                <p className="text-[10px] text-amber-700 dark:text-amber-400 mt-1">Requires a marbor restart to take effect.</p>
              </div>
              <Toggle label="Proxy access log" on={settings.proxyAccessLog} onToggle={() => setSettings({ ...settings, proxyAccessLog: !settings.proxyAccessLog })} />
            </div>
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Trust proxy headers</p>
                <p className="text-xs text-muted-foreground">Trust X-Forwarded-For/X-Real-IP for the logged client IP. Only enable if marbor sits behind a trusted reverse proxy - otherwise these headers are forgeable by any direct client.</p>
              </div>
              <Toggle label="Trust proxy headers" on={settings.proxyTrustProxyHeaders} onToggle={() => setSettings({ ...settings, proxyTrustProxyHeaders: !settings.proxyTrustProxyHeaders })} />
            </div>
          </div>
        </div>

        {/* Developer integrations & security */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-rose-500/10 rounded-lg">
              <Lock className="w-5 h-5 text-rose-600 dark:text-rose-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Developer integrations & security</h3>
              <p className="text-xs font-medium text-muted-foreground">API tokens and proxy access controls</p>
            </div>
          </div>

          <div className="space-y-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Hugging Face API Token
              </label>
              <input
                type="password"
                value={settings.huggingFaceToken || ''}
                onChange={(e) => setSettings({ ...settings, huggingFaceToken: e.target.value })}
                placeholder="hf_..."
                autoComplete="off"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
              <p className="text-[10px] text-muted-foreground mt-1">
                Used by Model Advisor to query the Hugging Face API for model recommendations. Encrypted at rest; masked here once saved.
              </p>
            </div>

            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Allow management endpoints</p>
                <p className="text-xs text-muted-foreground">Let client API keys reach model create/delete/pull APIs, not just inference. Only enable if every client is trusted.</p>
              </div>
              <button
                type="button" role="switch" aria-checked={settings.allowManagementEndpoints} aria-label="Allow management endpoints"
                onClick={() => setSettings({ ...settings, allowManagementEndpoints: !settings.allowManagementEndpoints })}
                className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${
                  settings.allowManagementEndpoints ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.allowManagementEndpoints ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>
          </div>
        </div>

        {/* Admin credentials - hidden in demo mode */}
        {!demoMode && (
          <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
            <div className="flex items-center gap-3 mb-5">
              <div className="p-2 bg-rose-500/10 rounded-lg">
                <KeyRound className="w-5 h-5 text-rose-600 dark:text-rose-400" />
              </div>
              <div>
                <h3 className="text-sm font-semibold text-foreground">Admin credentials</h3>
                <p className="text-xs font-medium text-muted-foreground">Change your dashboard login password</p>
              </div>
            </div>

            <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Current Password</label>
                <input
                  type="password"
                  value={credCurrentPw}
                  onChange={(e) => setCredCurrentPw(e.target.value)}
                  placeholder="Required to make changes"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">New Password <span className="text-muted-foreground/60">(optional)</span></label>
                <input
                  type="password"
                  value={credNewPw}
                  onChange={(e) => setCredNewPw(e.target.value)}
                  placeholder="Leave blank to keep current"
                  autoComplete="new-password"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Confirm New Password</label>
                <input
                  type="password"
                  value={credConfirmPw}
                  onChange={(e) => setCredConfirmPw(e.target.value)}
                  placeholder="Repeat new password"
                  autoComplete="new-password"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
              </div>
            </div>

            {credError && (
              <p className="mt-3 text-sm text-destructive">{credError}</p>
            )}
            {credSaved && (
              <p className="mt-3 text-sm text-green-600 dark:text-green-400">Credentials updated. Re-login required on other sessions.</p>
            )}

            <div className="mt-4 flex justify-end">
              <button
                onClick={handleChangeCredentials}
                disabled={credSaving || !credCurrentPw}
                className="flex items-center gap-2 px-4 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
              >
                {credSaving ? (
                  'Saving...'
                ) : credSaved ? (
                  <><Check className="w-4 h-4" /> Saved</>
                ) : (
                  <><Save className="w-4 h-4" /> Update Credentials</>
                )}
              </button>
            </div>
          </div>
        )}
          </div>
        </section>

        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">Integrations & Cost</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* LiteLLM integration */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-blue-500/10 rounded-lg">
              <Shield className="w-5 h-5 text-blue-600 dark:text-blue-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">LiteLLM integration</h3>
              <p className="text-xs font-medium text-muted-foreground">Middleware layer configuration</p>
            </div>
          </div>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Enable LiteLLM</p>
                <p className="text-xs text-muted-foreground">Route requests through LiteLLM proxy</p>
              </div>
              <button
                type="button" role="switch" aria-checked={settings.liteLLMEnabled} aria-label="Enable LiteLLM"
                onClick={() => setSettings({ ...settings, liteLLMEnabled: !settings.liteLLMEnabled })}
                className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${
                  settings.liteLLMEnabled ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.liteLLMEnabled ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>

            {settings.liteLLMEnabled && (
              <div className="animate-fade-in">
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  LiteLLM Endpoint URL
                </label>
                <input
                  type="text"
                  value={settings.liteLLMEndpoint}
                  onChange={(e) => setSettings({ ...settings, liteLLMEndpoint: e.target.value })}
                  placeholder="http://localhost:4000"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
                <label className="block text-sm font-medium text-muted-foreground mb-1.5 mt-3">
                  API Key
                </label>
                <input
                  type="password"
                  value={settings.liteLLMApiKey}
                  onChange={(e) => setSettings({ ...settings, liteLLMApiKey: e.target.value })}
                  autoComplete="off"
                  placeholder="sk-litellm-..."
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
                <p className="text-[10px] text-muted-foreground mt-1">
                  Sent as Authorization: Bearer &lt;key&gt; to your LiteLLM proxy.
                </p>
              </div>
            )}

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                /api/ps Polling Interval
              </label>
              <div className="flex items-center gap-3">
                <input
                  type="range"
                  min="1000"
                  max="10000"
                  step="500"
                  value={settings.pollingInterval}
                  onChange={(e) => setSettings({ ...settings, pollingInterval: parseInt(e.target.value) || 1000 })}
                  className="flex-1 accent-primary"
                />
                <code className="font-mono text-sm font-medium text-primary min-w-[80px]">
                  {settings.pollingInterval}ms
                </code>
              </div>
            </div>
          </div>
        </div>

        {/* Cloud spend cap */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-emerald-500/10 rounded-lg">
              <DollarSign className="w-5 h-5 text-emerald-600 dark:text-emerald-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Cloud spend cap</h3>
              <p className="text-xs font-medium text-muted-foreground">Block cloud fallback once spend hits these limits</p>
            </div>
          </div>

          <div className="space-y-4">
            <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  Display Currency
                </label>
                <CustomSelect
                  value={currency.code}
                  onChange={(val) => {
                    const preset = CURRENCY_PRESETS.find(c => c.code === val);
                    setCurrency({ ...currency, code: val, symbol: preset?.symbol || currency.symbol });
                  }}
                  options={CURRENCY_PRESETS.map(c => ({ value: c.code, label: c.code }))}
                />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                  FX Rate (1 USD =)
                </label>
                <input
                  type="number"
                  min={0.0001}
                  step="0.0001"
                  value={currency.fxRate}
                  onChange={(e) => setCurrency({ ...currency, fxRate: parseFloat(e.target.value) || 1 })}
                  disabled={currency.code === 'USD'}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50 disabled:opacity-50"
                />
              </div>
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Daily Cap ({currency.code})
              </label>
              <input
                type="number"
                min={0}
                step="0.01"
                value={roundDisplay(toDisplay(settings.cloudDailyUsdCap))}
                onChange={(e) => setSettings({ ...settings, cloudDailyUsdCap: toUSD(parseFloat(e.target.value) || 0) })}
                placeholder="0 = disabled"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Monthly Cap ({currency.code})
              </label>
              <input
                type="number"
                min={0}
                step="0.01"
                value={roundDisplay(toDisplay(settings.cloudMonthlyUsdCap))}
                onChange={(e) => setSettings({ ...settings, cloudMonthlyUsdCap: toUSD(parseFloat(e.target.value) || 0) })}
                placeholder="0 = disabled"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Warn at (% of cap)
              </label>
              <input
                type="number"
                min={0}
                max={100}
                step="1"
                value={Math.round(settings.cloudSoftBudgetPct * 100)}
                onChange={(e) => {
                  // The max=100 attribute is HTML-advisory only - clamp in
                  // the handler too, or typing e.g. 150 stores a fraction
                  // above 1.0, making the soft-warn threshold fire after
                  // the hard cap would already have blocked traffic.
                  const pct = Math.min(100, Math.max(0, parseFloat(e.target.value) || 0));
                  setSettings({ ...settings, cloudSoftBudgetPct: pct / 100 });
                }}
                placeholder="0 = disabled"
                className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
              />
            </div>

            <p className="text-[10px] text-muted-foreground">
              Checked against real cumulative cloud spend (UTC day/month). 0 disables the check.
              Amounts stored and enforced in USD - currency above is display-only, converted at the manual FX rate you set.
            </p>
          </div>
        </div>

          </div>
        </section>

        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">Routing & Reliability</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* Advanced routing */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-cyan-500/10 rounded-lg">
              <Network className="w-5 h-5 text-cyan-600 dark:text-cyan-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Advanced routing</h3>
              <p className="text-xs font-medium text-muted-foreground">Timeouts, retries, session affinity, and queueing take effect on next restart; prefix locality applies immediately</p>
            </div>
          </div>

          <div className="grid grid-cols-1 sm:grid-cols-2 gap-4">
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Fallback Strategy</label>
              <CustomSelect
                value={settings.routingFallback}
                onChange={(val) => setSettings({ ...settings, routingFallback: val })}
                options={[
                  { value: 'least-connections', label: 'Least Connections' },
                  { value: 'round-robin', label: 'Round Robin' },
                ]}
              />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Upstream Timeout (ms)</label>
              <input type="number" value={settings.routingUpstreamTimeoutMs} onChange={(e) => setSettings({ ...settings, routingUpstreamTimeoutMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Max Retries</label>
              <input type="number" value={settings.routingMaxRetries} onChange={(e) => setSettings({ ...settings, routingMaxRetries: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">nvidia-smi Poll Interval (ms)</label>
              <input type="number" value={settings.routingNvidiaPollIntervalMs} onChange={(e) => setSettings({ ...settings, routingNvidiaPollIntervalMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Queue Max Depth</label>
              <input type="number" value={settings.routingQueueMaxDepth} onChange={(e) => setSettings({ ...settings, routingQueueMaxDepth: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Queue Timeout (ms)</label>
              <input type="number" value={settings.routingQueueTimeoutMs} onChange={(e) => setSettings({ ...settings, routingQueueTimeoutMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Health Failure Threshold</label>
              <input type="number" value={settings.routingHealthFailureThreshold} onChange={(e) => setSettings({ ...settings, routingHealthFailureThreshold: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Health Success Threshold</label>
              <input type="number" value={settings.routingHealthSuccessThreshold} onChange={(e) => setSettings({ ...settings, routingHealthSuccessThreshold: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Cloud Overflow SLA (ms, 0 = disabled)</label>
              <input type="number" value={settings.routingOverflowSlaMs} onChange={(e) => setSettings({ ...settings, routingOverflowSlaMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Max In-Flight Per Node (0 = uncapped)</label>
              <input type="number" value={settings.routingMaxInFlightPerNode} onChange={(e) => setSettings({ ...settings, routingMaxInFlightPerNode: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
          </div>

          <div className="mt-4 flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
            <div>
              <p className="text-sm font-medium text-foreground">Session affinity</p>
              <p className="text-xs text-muted-foreground">Route requests sharing an X-Session-ID to the same node (KV-cache reuse)</p>
            </div>
            <Toggle label="Session affinity" on={settings.routingSessionAffinity} onToggle={() => setSettings({ ...settings, routingSessionAffinity: !settings.routingSessionAffinity })} />
          </div>
          {settings.routingSessionAffinity && (
            <div className="mt-3">
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">Session affinity TTL</label>
              <input type="text" value={settings.routingSessionAffinityTtl} onChange={(e) => setSettings({ ...settings, routingSessionAffinityTtl: e.target.value })} placeholder="10m" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
            </div>
          )}

          <div className="mt-4 pt-4 border-t border-border">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div className="flex items-center gap-2">
                <Flame className="w-4 h-4 text-orange-600 dark:text-orange-400" />
                <div>
                  <p className="text-sm font-medium text-foreground">Thermal watchdog</p>
                  <p className="text-xs text-muted-foreground">Auto-drain a node after sustained overheat (recovery requires manual undrain)</p>
                </div>
              </div>
              <Toggle label="Thermal watchdog" on={settings.thermalWatchdogEnabled} onToggle={() => setSettings({ ...settings, thermalWatchdogEnabled: !settings.thermalWatchdogEnabled })} />
            </div>
            {settings.thermalWatchdogEnabled && (
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-3 mt-3">
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Max Temp (°C)</label>
                  <input type="number" value={settings.thermalWatchdogMaxTempCelsius} onChange={(e) => setSettings({ ...settings, thermalWatchdogMaxTempCelsius: parseFloat(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                </div>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Consecutive Breaches</label>
                  <input type="number" value={settings.thermalWatchdogConsecutiveBreaches} onChange={(e) => setSettings({ ...settings, thermalWatchdogConsecutiveBreaches: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                </div>
              </div>
            )}
          </div>

          <div className="mt-4 pt-4 border-t border-border">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Prefix Locality Hints</p>
                <p className="text-xs text-muted-foreground">Soft preference for the node that completed the most recent matching turn in this conversation (KV-cache reuse) - never overrides warm residency</p>
              </div>
              <Toggle label="Prefix locality hints" on={settings.routingPrefixLocalityEnabled} onToggle={() => setSettings({ ...settings, routingPrefixLocalityEnabled: !settings.routingPrefixLocalityEnabled })} />
            </div>
            {settings.routingPrefixLocalityEnabled && (
              <div className="mt-3">
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Score Weight</label>
                <input type="number" value={settings.routingPrefixLocalityWeight} onChange={(e) => setSettings({ ...settings, routingPrefixLocalityWeight: parseFloat(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                <p className="text-xs text-muted-foreground mt-1">Must stay below warm-residency (50) and queue-depth (15) weights so it can never flip a warm-vs-cold decision. Default 10.</p>
              </div>
            )}
          </div>
        </div>

        {/* Docker auto-discovery */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-blue-500/10 rounded-lg">
              <Container className="w-5 h-5 text-blue-600 dark:text-blue-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Docker auto-discovery</h3>
              <p className="text-xs font-medium text-muted-foreground">Auto-register Ollama containers - takes effect on next restart</p>
            </div>
          </div>
          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Enable Docker discovery</p>
                <p className="text-xs text-muted-foreground">Poll the Docker socket for Ollama containers</p>
              </div>
              <Toggle label="Docker discovery" on={settings.dockerEnabled} onToggle={() => setSettings({ ...settings, dockerEnabled: !settings.dockerEnabled })} />
            </div>
            {settings.dockerEnabled && (
              <>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Docker Socket</label>
                  <input type="text" value={settings.dockerSocket} onChange={(e) => setSettings({ ...settings, dockerSocket: e.target.value })} placeholder="/var/run/docker.sock" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
                </div>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Poll Interval (ms)</label>
                  <input type="number" value={settings.dockerPollIntervalMs} onChange={(e) => setSettings({ ...settings, dockerPollIntervalMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                </div>
              </>
            )}
          </div>
        </div>

        {/* Webhooks */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-violet-500/10 rounded-lg">
              <Webhook className="w-5 h-5 text-violet-600 dark:text-violet-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Webhooks</h3>
              <p className="text-xs font-medium text-muted-foreground">HMAC-signed node health-transition notifications</p>
            </div>
          </div>
          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Enable webhooks</p>
              </div>
              <Toggle label="Webhooks" on={settings.webhookEnabled} onToggle={() => setSettings({ ...settings, webhookEnabled: !settings.webhookEnabled })} />
            </div>
            {settings.webhookEnabled && (
              <>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Webhook URL</label>
                  <input type="text" value={settings.webhookUrl} onChange={(e) => setSettings({ ...settings, webhookUrl: e.target.value })} placeholder="https://hooks.example.com/marbor" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
                </div>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Signing Secret</label>
                  <input type="password" value={settings.webhookSecret} onChange={(e) => setSettings({ ...settings, webhookSecret: e.target.value })} autoComplete="off" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                  <p className="text-[10px] text-muted-foreground mt-1">Stored server-side; masked here once saved.</p>
                </div>
              </>
            )}
          </div>
        </div>

          </div>
        </section>

        <section>
          <h2 className="text-xs font-semibold uppercase tracking-wide text-muted-foreground mb-3">Observability & Limits</h2>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-6 items-start">
        {/* Observability */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-purple-500/10 rounded-lg">
              <Activity className="w-5 h-5 text-purple-600 dark:text-purple-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Observability</h3>
              <p className="text-xs font-medium text-muted-foreground">Metrics and logging configuration</p>
            </div>
          </div>

          <div className="space-y-4">
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Prometheus metrics</p>
                <p className="text-xs text-muted-foreground">Export metrics in Prometheus format</p>
              </div>
              <button
                type="button" role="switch" aria-checked={settings.prometheusEnabled} aria-label="Prometheus metrics"
                onClick={() => setSettings({ ...settings, prometheusEnabled: !settings.prometheusEnabled })}
                className={`relative inline-flex h-6 w-11 shrink-0 items-center rounded-full transition-colors ${
                  settings.prometheusEnabled ? 'bg-primary' : 'bg-muted-foreground/30'
                }`}
              >
                <span
                  className={`inline-block h-4 w-4 transform rounded-full bg-white transition-transform ${
                    settings.prometheusEnabled ? 'translate-x-6' : 'translate-x-1'
                  }`}
                />
              </button>
            </div>

            {settings.prometheusEnabled && (
              <div className="animate-fade-in space-y-4">
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                    Prometheus Port
                  </label>
                  <input
                    type="number"
                    value={settings.prometheusPort}
                    onChange={(e) => setSettings({ ...settings, prometheusPort: parseInt(e.target.value) || settings.prometheusPort })}
                    className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                  />
                </div>
                <div>
                  <label htmlFor="metrics-bind-address" className="block text-sm font-medium text-muted-foreground mb-1.5">
                    Bind Address
                  </label>
                  <input
                    id="metrics-bind-address"
                    type="text"
                    value={settings.prometheusBindAddress}
                    onChange={(e) => setSettings({ ...settings, prometheusBindAddress: e.target.value })}
                    placeholder="127.0.0.1:9090"
                    aria-describedby="metrics-bind-address-help"
                    className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                  />
                  <p id="metrics-bind-address-help" className="text-xs text-muted-foreground mt-1.5">
                    This endpoint has no authentication - defaults to loopback-only outside Docker (every interface inside Docker, for the bundled monitoring overlay). Only widen it (e.g. to 0.0.0.0:9090) on a trusted network.
                  </p>
                </div>
              </div>
            )}

            <div>
              <label className="block text-sm font-medium text-muted-foreground mb-1.5">
                Log Level
              </label>
              <CustomSelect
                value={settings.logLevel}
                onChange={(val) => setSettings({ ...settings, logLevel: val as any })}
                options={[
                  { value: 'debug', label: 'Debug' },
                  { value: 'info', label: 'Info' },
                  { value: 'warn', label: 'Warning' },
                  { value: 'error', label: 'Error' },
                ]}
              />
            </div>
          </div>
        </div>

        {/* Model context windows */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-lime-500/10 rounded-lg">
              <Ruler className="w-5 h-5 text-lime-600 dark:text-lime-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Model context windows</h3>
              <p className="text-xs font-medium text-muted-foreground">Operator-declared max tokens per model, for admission-time checks</p>
            </div>
          </div>

          <div className="space-y-2 mb-4">
            {Object.entries(settings.contextWindows).length === 0 ? (
              <p className="text-sm text-muted-foreground py-2">No context windows declared</p>
            ) : (
              Object.entries(settings.contextWindows).map(([model, tokens]) => (
                <div key={model} className="flex items-center justify-between gap-3 p-2.5 rounded-lg border border-border bg-secondary/30 min-w-0">
                  <div className="min-w-0 flex-1">
                    <p className="text-sm font-medium text-foreground truncate" title={model}>{model}</p>
                    <p className="text-xs text-muted-foreground">{tokens.toLocaleString()} tokens</p>
                  </div>
                  <button
                    onClick={() => {
                      const next = { ...settings.contextWindows };
                      delete next[model];
                      setSettings({ ...settings, contextWindows: next });
                    }}
                    className="p-1.5 text-muted-foreground hover:text-destructive rounded-md hover:bg-secondary transition-colors"
                  >
                    <Trash2 className="w-3.5 h-3.5" />
                  </button>
                </div>
              ))
            )}
          </div>

          <div className="flex flex-col sm:flex-row gap-2">
            <CustomCombobox value={newCtxModel} onChange={setNewCtxModel} options={knownModelNames} placeholder="llama3.2:8b" className="sm:flex-1" />
            <input type="number" value={newCtxTokens} onChange={(e) => setNewCtxTokens(e.target.value)} placeholder="8192" className="w-full sm:w-28 px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
            <button
              onClick={() => {
                const tokens = parseInt(newCtxTokens, 10);
                // !tokens only rejects 0/NaN - a negative value is truthy
                // and was passing straight through to the stored setting.
                if (!newCtxModel.trim() || !Number.isFinite(tokens) || tokens <= 0) return;
                setSettings({ ...settings, contextWindows: { ...settings.contextWindows, [newCtxModel.trim()]: tokens } });
                setNewCtxModel('');
                setNewCtxTokens('');
              }}
              aria-label="Add context window"
              title="Add context window"
              className="flex items-center gap-1.5 px-3 py-2 bg-primary hover:bg-primary/90 text-primary-foreground text-sm font-medium rounded-lg transition-colors"
            >
              <Plus className="w-4 h-4" />
            </button>
          </div>
        </div>

        {/* Global warmup & audit */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-amber-500/10 rounded-lg">
              <Ruler className="w-5 h-5 text-amber-600 dark:text-amber-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Global warmup & audit</h3>
              <p className="text-xs font-medium text-muted-foreground">Distinct from per-node warmup toggles on the Warmup page</p>
            </div>
          </div>
          <div className="grid grid-cols-1 lg:grid-cols-2 gap-4">
            <div className="space-y-4">
              <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
                <div>
                  <p className="text-sm font-medium text-foreground">Enable global warmup</p>
                </div>
                <Toggle label="Global warmup" on={settings.warmupEnabled} onToggle={() => setSettings({ ...settings, warmupEnabled: !settings.warmupEnabled })} />
              </div>
              {settings.warmupEnabled && (
                <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                  <div>
                    <label className="block text-sm font-medium text-muted-foreground mb-1.5">Interval (ms)</label>
                    <input type="number" value={settings.warmupIntervalMs} onChange={(e) => setSettings({ ...settings, warmupIntervalMs: parseInt(e.target.value) || 0 })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                  </div>
                  <div>
                    <label className="block text-sm font-medium text-muted-foreground mb-1.5">Keep Alive</label>
                    <input type="text" value={settings.warmupKeepAlive} onChange={(e) => setSettings({ ...settings, warmupKeepAlive: e.target.value })} placeholder="10m" className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50" />
                  </div>
                </div>
              )}
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Local Token Value ($/1k tokens)</label>
                <input type="number" step="0.001" value={roundPrice(settings.savingsReferenceCostPer1k)} onChange={(e) => setSettings({ ...settings, savingsReferenceCostPer1k: roundPrice(parseFloat(e.target.value) || 0) })} className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50" />
                <p className="text-[10px] text-muted-foreground mt-1">Cloud rate used to value locally-served tokens in the dashboard's savings calculation.</p>
              </div>
            </div>
            <div className="space-y-4">
              <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
                <div>
                  <p className="text-sm font-medium text-foreground">Audit log</p>
                  <p className="text-xs text-muted-foreground">Best-effort request audit trail</p>
                </div>
                <Toggle label="Audit log" on={settings.auditEnabled} onToggle={() => setSettings({ ...settings, auditEnabled: !settings.auditEnabled })} />
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Audit log retention (days)</label>
                <input
                  type="number"
                  min={0}
                  step={1}
                  value={settings.auditRetentionDays}
                  onChange={(e) => setSettings({ ...settings, auditRetentionDays: Math.max(0, parseInt(e.target.value, 10) || 0) })}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                />
                <p className="text-[10px] text-muted-foreground mt-1">
                  Requests older than this are pruned automatically (checked every 12h). Set to 0 to keep audit log entries forever.
                </p>
              </div>
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">System audit retention (days)</label>
                <input
                  type="number"
                  min={0}
                  step={1}
                  value={settings.systemAuditRetentionDays}
                  onChange={(e) => setSettings({ ...settings, systemAuditRetentionDays: Math.max(0, parseInt(e.target.value, 10) || 0) })}
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                />
                <p className="text-[10px] text-muted-foreground mt-1">
                  Applies to the admin action trail (System Audit page), separate from request logs above. Defaults to 0 - keep forever - since this log is low-volume and security-sensitive.
                </p>
              </div>
            </div>
          </div>
        </div>

        {/* Backup & restore */}
        <div className="bg-card border border-border shadow-sm rounded-xl p-6 lg:col-span-2">
          <div className="flex items-center gap-3 mb-5">
            <div className="p-2 bg-emerald-500/10 rounded-lg">
              <HardDrive className="w-5 h-5 text-emerald-600 dark:text-emerald-400" />
            </div>
            <div>
              <h3 className="text-sm font-semibold text-foreground">Backup & restore</h3>
              <p className="text-xs font-medium text-muted-foreground">
                Full docs:{' '}
                <a
                  href="https://marbor.in/docs/backup.html"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-primary hover:underline"
                >
                  docs/backup.md
                </a>
              </p>
            </div>
          </div>
          <div className="space-y-4">
            <div>
              <div className="flex flex-wrap items-center gap-2">
                <button
                  onClick={() => { void handleBackupNow(); }}
                  disabled={backupDownloading || demoMode}
                  title={demoMode ? DEMO_BACKUP_TITLE : undefined}
                  className="flex items-center gap-1.5 px-3 py-2 min-h-[44px] sm:min-h-0 bg-primary hover:bg-primary/90 disabled:opacity-50 disabled:cursor-not-allowed text-primary-foreground text-sm font-medium rounded-lg transition-colors"
                >
                  <Download className="w-4 h-4" />
                  {backupDownloading ? 'Preparing download...' : 'Download Backup Now'}
                </button>
                {backupDownloading && (
                  <button
                    type="button"
                    onClick={() => backupAbortRef.current?.abort()}
                    className="px-3 py-2 min-h-[44px] sm:min-h-0 text-sm font-medium text-muted-foreground hover:text-foreground border border-border rounded-lg transition-colors"
                  >
                    Cancel
                  </button>
                )}
              </div>
              {demoMode && (
                <p className="text-xs text-muted-foreground mt-1.5">{DEMO_BACKUP_TITLE}</p>
              )}
              {backupDownloadError && (
                <p role="alert" className="text-xs text-red-600 dark:text-red-400 mt-1.5 break-words">{backupDownloadError}</p>
              )}
              {backupDownloadNotice && (
                <p role="status" className="text-xs text-emerald-700 dark:text-emerald-400 mt-1.5 break-words">{backupDownloadNotice}</p>
              )}
              <p className="text-[10px] text-muted-foreground mt-1.5">
                Saves a gzip-compressed, consistent point-in-time copy of marbor.db to your browser's downloads folder, taken while marbor keeps running. The backup is collected in memory first, so it is limited to {formatBackupSize(MAX_MEMORY_DOWNLOAD_BYTES)}. For a larger database run "marbor backup now" in a terminal on a machine that can reach marbor, or download one of the stored backups from the list below.
              </p>
              <div aria-live="polite">
                {backupRunning && !backupStale && !backupListError && !pollGaveUp && pollFailures === 0 && (
                  <p className="text-xs text-muted-foreground mt-1.5">
                    A backup is running now{backupStatus?.running_since ? ` (since ${formatDateTimeInZone(backupStatus.running_since, tz)})` : ''}. This updates by itself.
                  </p>
                )}
              </div>
              {(backupListError || backupStale || pollGaveUp) && (
                <div className="mt-1.5 text-xs text-red-600 dark:text-red-400 break-words">
                  <p role={retryClicked ? 'alert' : undefined}>
                    {backupListError
                      ? `Could not load the backup status: ${backupListError}`
                      : backupStale
                        ? `Showing the last known state - the latest refresh failed: ${backupStale}`
                        : 'Showing the last known state.'}
                    {pollGaveUp ? ' Automatic refresh stopped after repeated failures.' : ''}
                  </p>
                  <button
                    type="button"
                    onClick={() => {
                      setRetryClicked(true);
                      failuresRef.current = 0;
                      setPollGaveUp(false);
                      void loadBackups({ quiet: backupLoadedRef.current });
                    }}
                    className="mt-1 min-h-[44px] sm:min-h-0 px-1 underline text-red-700 dark:text-red-300 hover:no-underline"
                  >
                    Try again
                  </button>
                </div>
              )}
              {backupStatus ? (
                backupStatusLines(backupStatus, backupSkipped, (iso) => formatDateTimeInZone(iso, tz), {
                  manualNotSaved: notSavedAttempt !== null && (backupStatus.manual?.attempt_at ?? '') === notSavedAttempt,
                }).map((line) => (
                  <p
                    key={line.key}
                    className={`text-xs mt-1.5 break-words ${
                      line.tone === 'error' ? 'text-red-600 dark:text-red-400'
                        : line.tone === 'warn' ? 'text-amber-700 dark:text-amber-400'
                        : 'text-muted-foreground'
                    }`}
                  >
                    {line.text}
                    {line.key === 'restore' && backupStatus.last_restore?.result && (
                      <button
                        type="button"
                        onClick={() => { void handleAckRestore(); }}
                        disabled={ackingRestore || demoMode}
                        title="Clears this restore result for every operator and for the command line, not only on this page"
                        className="ml-2 inline-flex items-center min-h-[44px] sm:min-h-0 px-1 underline text-muted-foreground hover:no-underline disabled:opacity-50 disabled:cursor-not-allowed"
                      >
                        {ackingRestore ? 'Clearing...' : 'Dismiss for everyone'}
                      </button>
                    )}
                  </p>
                ))
              ) : (
                <>
                  {settings.backupLastAt && (
                    <p className="text-xs text-muted-foreground mt-1.5">
                      Last scheduled backup: {formatDateTimeInZone(settings.backupLastAt, tz)}
                    </p>
                  )}
                  {settings.backupLastError && (
                    <p className="text-xs text-red-600 dark:text-red-400 mt-1.5 break-words">
                      Last scheduled backup failed: {settings.backupLastError}
                    </p>
                  )}
                </>
              )}
              {ackError && <p role="alert" className="text-xs text-red-600 dark:text-red-400 mt-1.5 break-words">{ackError}</p>}
            </div>
            <div className="flex items-center justify-between p-3 rounded-lg border border-border bg-secondary/30">
              <div>
                <p className="text-sm font-medium text-foreground">Scheduled backup</p>
                <p className="text-xs text-muted-foreground">Automatic gzip-compressed backups on a recurring interval</p>
              </div>
              <Toggle label="Scheduled backup" on={settings.backupEnabled} onToggle={() => setSettings({ ...settings, backupEnabled: !settings.backupEnabled })} />
            </div>
            {settings.backupEnabled && (
              <div className="grid grid-cols-1 sm:grid-cols-2 gap-3">
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Interval (hours)</label>
                  <input
                    type="number"
                    min={1}
                    step={1}
                    value={settings.backupIntervalHours}
                    onChange={(e) => setSettings({ ...settings, backupIntervalHours: Math.max(1, parseInt(e.target.value, 10) || 24) })}
                    className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                  />
                </div>
                <div>
                  <label className="block text-sm font-medium text-muted-foreground mb-1.5">Retention (backups kept)</label>
                  <input
                    type="number"
                    min={1}
                    step={1}
                    value={settings.backupRetentionCount}
                    onChange={(e) => setSettings({ ...settings, backupRetentionCount: Math.max(1, parseInt(e.target.value, 10) || 7) })}
                    className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground focus:outline-none focus:border-primary/50"
                  />
                  <p className="text-xs text-muted-foreground mt-1">
                    Files you upload are kept in a separate pool with the same limit, so up to twice this many files can be on disk.
                  </p>
                </div>
              </div>
            )}
            {settings.backupEnabled && (
              <div>
                <label className="block text-sm font-medium text-muted-foreground mb-1.5">Target Directory</label>
                <input
                  type="text"
                  value={settings.backupTargetDir}
                  onChange={(e) => setSettings({ ...settings, backupTargetDir: e.target.value })}
                  placeholder="/backups"
                  className="w-full px-3 py-2 bg-secondary/50 border border-border rounded-lg text-sm text-foreground placeholder-muted-foreground/50 focus:outline-none focus:border-primary/50"
                />
                <p className="text-[10px] text-muted-foreground mt-1">
                  In Docker, defaults to a separate volume from marbor.db's own data volume, so deleting one doesn't take out the other.
                </p>
              </div>
            )}
          </div>

          <div className="mt-5 pt-5 border-t border-border">
            <p className="text-sm font-medium text-foreground mb-1">Restore from a backup</p>
            <p className="text-[10px] text-muted-foreground mb-3">
              Swaps marbor.db for the selected file and restarts the marbor. Requires the deployment to auto-restart
              on exit (systemd, Docker's <code>restart</code> policy, or Kubernetes) - otherwise start it manually
              afterward. This cannot be undone.
            </p>
            {restoreWatch && (
              <div className="mb-3">
                <p
                  role="status"
                  className={`text-xs break-words ${
                    restoreWatch.phase === 'failed' ? 'text-red-600 dark:text-red-400'
                      : ['still-running', 'timeout', 'unknown', 'signin'].includes(restoreWatch.phase)
                        ? 'text-amber-700 dark:text-amber-400'
                        : 'text-emerald-700 dark:text-emerald-400'
                  }`}
                >
                  {RESTORE_FOLLOW_TEXT[restoreWatch.phase]}
                  {restoreWatch.detail ? ` ${restoreWatch.detail}` : ''}
                </p>
                {['failed', 'still-running', 'timeout', 'unknown'].includes(restoreWatch.phase) && (
                  <button
                    type="button"
                    onClick={dismissRestoreWatch}
                    className="min-h-[44px] sm:min-h-0 px-1 text-xs underline text-muted-foreground hover:no-underline"
                  >
                    Hide this message
                  </button>
                )}
              </div>
            )}
            {uploadError && (
              <p role="alert" className="text-xs text-red-600 dark:text-red-400 mb-3 break-words">{uploadError}</p>
            )}
            {uploadNotice && (
              <p role="status" className="text-xs text-amber-700 dark:text-amber-400 mb-3 break-words">{uploadNotice}</p>
            )}
            {storedDownloadError && (
              <p role="alert" className="text-xs text-red-600 dark:text-red-400 mb-3 break-words">{storedDownloadError}</p>
            )}
            {storedDownloadNotice && (
              <p role="status" className="text-xs text-emerald-700 dark:text-emerald-400 mb-3 break-words">{storedDownloadNotice}</p>
            )}
            {backupListLoading ? (
              <p className="text-xs text-muted-foreground">Loading backups...</p>
            ) : (
              <div>
                <div className="flex flex-col gap-2 sm:flex-row sm:items-center">
                  {backupList.length === 0 ? (
                    <p className="flex-1 min-w-0 text-xs text-muted-foreground">
                      {backupListError
                        ? 'The backup list could not be loaded (see above).'
                        : 'No backups found yet in the target directory.'}
                    </p>
                  ) : (
                    <CustomSelect
                      className="flex-1 min-w-0"
                      touchTarget
                      value={selectedBackupName}
                      onChange={setSelectedBackupName}
                      options={backupList.map((b) => ({ value: b.name, label: b.name }))}
                    />
                  )}
                  <div className="flex items-center gap-2 shrink-0">
                    <button
                      type="button"
                      onClick={() => uploadInputRef.current?.click()}
                      disabled={uploadingBackup || storedDownloading || demoMode}
                      title={'Attach a .db or .db.gz file from your computer as a backup'}
                      aria-label="Attach a local .db or .db.gz file as a backup"
                      className="w-11 h-11 sm:w-9 sm:h-9 flex items-center justify-center bg-secondary/50 border border-border rounded-lg text-foreground transition-colors hover:bg-secondary disabled:opacity-50 disabled:cursor-not-allowed"
                    >
                      {uploadingBackup ? (
                        <RefreshCw className="w-4 h-4 animate-spin" />
                      ) : (
                        <Plus className="w-4 h-4" />
                      )}
                    </button>
                    <input
                      ref={uploadInputRef}
                      type="file"
                      accept=".db,.gz"
                      tabIndex={-1}
                      aria-hidden="true"
                      className="hidden"
                      disabled={uploadingBackup}
                      onChange={handleBackupFileChosen}
                    />
                    <button
                      type="button"
                      onClick={() => { void handleStoredDownload(); }}
                      disabled={!selectedBackup || storedDownloading || uploadingBackup || demoMode}
                      title="Download this backup file"
                      aria-label="Download the selected backup"
                      className="w-11 h-11 sm:w-9 sm:h-9 flex items-center justify-center bg-secondary/50 border border-border rounded-lg text-foreground hover:bg-secondary transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                    >
                      {storedDownloading ? <RefreshCw className="w-4 h-4 animate-spin" /> : <Download className="w-4 h-4" />}
                    </button>
                    <button
                      type="button"
                      onClick={() => selectedBackup && setRestoreTarget(selectedBackup)}
                      disabled={!selectedBackup || demoMode || (restoreWatch !== null && ['restarting', 'checking', 'restarted', 'succeeded'].includes(restoreWatch.phase))}
                      className="h-11 sm:h-9 px-4 sm:px-3 bg-red-600 hover:bg-red-600/90 text-white text-xs font-medium rounded-lg transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                    >
                      Restore
                    </button>
                    <button
                      type="button"
                      onClick={() => { setDeleteError(null); if (selectedBackup) setDeleteTarget(selectedBackup); }}
                      disabled={!selectedBackup || demoMode}
                      title="Delete this backup file from the server"
                      aria-label="Delete the selected backup"
                      className="w-11 h-11 sm:w-9 sm:h-9 flex items-center justify-center bg-secondary/50 border border-border rounded-lg text-red-600 dark:text-red-400 hover:bg-secondary transition-colors disabled:opacity-50 disabled:cursor-not-allowed"
                    >
                      <Trash2 className="w-4 h-4" />
                    </button>
                  </div>
                </div>
                {(uploadingBackup || storedDownloading) && (
                  <div className="mt-1.5 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                    <span>
                      {uploadingBackup
                        ? 'Uploading... (no progress is shown; large files take a while). Downloading a stored backup waits until the upload finishes.'
                        : 'Downloading... Uploading a file waits until the download finishes.'}
                    </span>
                    <button
                      type="button"
                      onClick={() => (uploadingBackup ? uploadAbortRef : storedAbortRef).current?.abort()}
                      className="min-h-[44px] sm:min-h-0 px-2 underline hover:no-underline text-foreground"
                    >
                      Cancel
                    </button>
                  </div>
                )}
                {selectedBackup && (
                  <div className="mt-1.5 space-y-1">
                    <p className="text-[10px] text-muted-foreground break-words">
                      {formatDateTimeInZone(selectedBackup.modified_at, tz)} - {formatBackupSize(selectedBackup.size_bytes)}
                      {selectedBackup.compressed ? ' - compressed' : ''}
                      {selectedBackup.uploaded ? ' - uploaded' : ''}
                    </p>
                    {selectedBackup.integrity === 'invalid' && (
                      <p className="text-xs text-red-600 dark:text-red-400 break-words">
                        This file failed the server's check. Restoring it will most likely be refused.
                      </p>
                    )}
                    {selectedBackup.integrity === 'unverified' && (
                      <p className="text-[10px] text-muted-foreground break-words">
                        This file has not been read in full yet; a restore checks it first and leaves the live database alone if it is damaged.
                      </p>
                    )}
                  </div>
                )}
              </div>
            )}
          </div>
        </div>
          </div>
        </section>
      </div>
    </div>

    <Modal
      isOpen={restoreTarget !== null}
      onClose={closeRestoreDialog}
      title="Restore from backup"
      maxWidth="sm"
    >
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          This will stop the marbor, replace the live database with <span className="font-medium text-foreground break-all">{restoreTarget?.name}</span>,
          and restart. Everything written since that backup was taken - new nodes, keys, settings, routing history - will be lost.
        </p>
        {restoreTarget?.compressed && (
          <p className="text-xs text-muted-foreground">
            This backup is compressed. marbor decompresses and checks it first, and needs free disk space for the
            full-size database next to the live one. It cannot tell exactly how much until it has read the file, so if
            there is not enough room it stops and tells you the amount; the live database is left untouched.
          </p>
        )}
        <p className="text-sm text-destructive font-medium">This cannot be undone.</p>
        {restoreError && (
          <p role="alert" className="text-sm text-destructive break-words">{restoreError}</p>
        )}
        <div className="flex justify-end gap-3 pt-4 border-t border-border">
          <button
            onClick={closeRestoreDialog}
            disabled={restoring}
            className="px-4 py-2 min-h-[44px] sm:min-h-0 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            onClick={handleRestoreConfirm}
            disabled={restoring}
            className="px-4 py-2 min-h-[44px] sm:min-h-0 bg-red-600 hover:bg-red-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50"
          >
            {restoring ? 'Restoring...' : 'Restore & Restart'}
          </button>
        </div>
      </div>
    </Modal>

    <Modal
      isOpen={deleteTarget !== null}
      onClose={() => { if (!deleting) { setDeleteTarget(null); setDeleteError(null); } }}
      title="Delete backup"
      maxWidth="sm"
    >
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Delete <span className="font-medium text-foreground break-all">{deleteTarget?.name}</span> from the server?
        </p>
        <p className="text-sm text-destructive font-medium">This cannot be undone.</p>
        {deleteError && (
          <p role="alert" className="text-sm text-destructive break-words">{deleteError}</p>
        )}
        <div className="flex justify-end gap-3 pt-4 border-t border-border">
          <button
            onClick={() => { setDeleteTarget(null); setDeleteError(null); }}
            disabled={deleting}
            className="px-4 py-2 min-h-[44px] sm:min-h-0 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            onClick={() => { void handleDeleteConfirm(); }}
            disabled={deleting}
            className="px-4 py-2 min-h-[44px] sm:min-h-0 bg-red-600 hover:bg-red-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50"
          >
            {deleting ? 'Deleting...' : 'Delete'}
          </button>
        </div>
      </div>
    </Modal>

    <Modal
      isOpen={reloadConfirmOpen}
      onClose={() => setReloadConfirmOpen(false)}
      title="Reload from database"
      maxWidth="sm"
    >
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Re-sync nodes, API keys, and cloud providers from the database into the running process?
        </p>
        <p className="text-xs text-muted-foreground">
          Applies immediately without a restart. Other settings (listen ports/addresses, Docker/Webhook wiring)
          require a restart to take effect.
        </p>
        {error && (
          <p className="text-sm text-destructive">{error}</p>
        )}
        <div className="flex justify-end gap-3 pt-4 border-t border-border">
          <button
            onClick={() => setReloadConfirmOpen(false)}
            disabled={reloading}
            className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            onClick={handleReload}
            disabled={reloading}
            className="px-4 py-2 bg-amber-600 hover:bg-amber-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm disabled:opacity-50"
          >
            Reload
          </button>
        </div>
      </div>
    </Modal>

    <Modal
      isOpen={demoModeConfirmOpen}
      onClose={() => setDemoModeConfirmOpen(false)}
      title="Enable Demo mode"
      maxWidth="sm"
    >
      <div className="space-y-4">
        <p className="text-sm text-muted-foreground">
          Switch this dashboard to mock data? Live fleet data (nodes, requests, savings) will be
          replaced by fabricated demo values until you turn Demo mode back off.
        </p>
        <div className="flex justify-end gap-3 pt-4 border-t border-border">
          <button
            onClick={() => setDemoModeConfirmOpen(false)}
            className="px-4 py-2 text-sm font-medium text-muted-foreground hover:text-foreground transition-colors"
          >
            Cancel
          </button>
          <button
            onClick={() => { setDemoMode(true); setDemoModeConfirmOpen(false); }}
            className="px-4 py-2 bg-amber-600 hover:bg-amber-600/90 text-white font-medium rounded-lg text-sm transition-colors shadow-sm"
          >
            Enable Demo mode
          </button>
        </div>
      </div>
    </Modal>
    </>
  );
}
