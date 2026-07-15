import { lazy, Suspense, Component, ReactNode, useState, useEffect } from 'react';
import { BrowserRouter, HashRouter, Routes, Route, Navigate, useLocation } from 'react-router-dom';
import { ThemeProvider } from './hooks/useTheme';
import { forcedDemo } from './hooks/useDemoMode';
import { Sidebar } from './components/Sidebar';
import { DemoBanner } from './components/DemoBanner';
import { BudgetBanner } from './components/BudgetBanner';
import { Login } from './components/Login';
import { ForceChangePassword } from './components/ForceChangePassword';
import { UserPortal } from './pages/UserPortal';
import { loadSession, logout, getPendingUserCount } from './lib/api';
import type { SessionData } from './types';

// ---------------------------------------------------------------------------
// ErrorBoundary
// Accepts an optional `resetKey` prop — when it changes React unmounts and
// remounts this subtree, automatically clearing any caught error. Without this
// the boundary stays in error state across all subsequent navigations.
// ---------------------------------------------------------------------------
interface ErrorBoundaryProps {
  children: ReactNode;
  resetKey?: string;
}

class ErrorBoundary extends Component<ErrorBoundaryProps, { error: Error | null }> {
  constructor(props: ErrorBoundaryProps) {
    super(props);
    this.state = { error: null };
  }
  static getDerivedStateFromError(error: Error) { return { error }; }
  // Reset when resetKey changes (i.e. on every navigation).
  static getDerivedStateFromProps(
    props: ErrorBoundaryProps,
    state: { error: Error | null; _prevKey?: string }
  ) {
    if (state._prevKey !== undefined && state._prevKey !== props.resetKey) {
      return { error: null, _prevKey: props.resetKey };
    }
    return { _prevKey: props.resetKey };
  }
  render() {
    if (this.state.error) {
      const isChunkLoadError =
        this.state.error.name === 'ChunkLoadError' ||
        /failed to fetch dynamically imported module|chunk|load/i.test(this.state.error.message || '');

      return (
        <div className="p-8 text-center">
          <p className="text-destructive font-semibold mb-2">
            {isChunkLoadError ? 'New application version available' : 'Page failed to load'}
          </p>
          <p className="text-xs text-muted-foreground font-mono mb-1">{this.state.error.message}</p>
          {isChunkLoadError && (
            <p className="text-xs text-muted-foreground mb-3">Please reload the page to load the latest version.</p>
          )}
          <button
            onClick={() => {
              if (isChunkLoadError) {
                window.location.reload();
              } else {
                this.setState({ error: null });
              }
            }}
            className="mt-4 px-3 py-1.5 text-xs bg-secondary rounded-lg text-foreground hover:bg-secondary/80 cursor-pointer"
          >
            {isChunkLoadError ? 'Reload Page' : 'Retry'}
          </button>
        </div>
      );
    }
    return this.props.children;
  }
}

// ---------------------------------------------------------------------------
// Lazy page imports
// ---------------------------------------------------------------------------
const Dashboard    = lazy(() => import('./pages/Dashboard').then(m => ({ default: m.Dashboard })));
const GPUNodes     = lazy(() => import('./pages/GPUNodes').then(m => ({ default: m.GPUNodes })));
const APIKeys      = lazy(() => import('./pages/APIKeys').then(m => ({ default: m.APIKeys })));
const Routing      = lazy(() => import('./pages/Routing').then(m => ({ default: m.Routing })));
const Metrics      = lazy(() => import('./pages/Metrics').then(m => ({ default: m.Metrics })));
const SettingsPage = lazy(() => import('./pages/Settings').then(m => ({ default: m.SettingsPage })));
const Analytics    = lazy(() => import('./pages/Analytics').then(m => ({ default: m.Analytics })));
const Models       = lazy(() => import('./pages/Models').then(m => ({ default: m.Models })));
const ModelAdvisor = lazy(() => import('./pages/ModelAdvisor').then(m => ({ default: m.ModelAdvisor })));
const Requests     = lazy(() => import('./pages/Requests').then(m => ({ default: m.Requests })));
const Warmup       = lazy(() => import('./pages/Warmup').then(m => ({ default: m.Warmup })));
const Users        = lazy(() => import('./pages/Users').then(m => ({ default: m.Users })));
const SystemAudit  = lazy(() => import('./pages/SystemAudit').then(m => ({ default: m.SystemAudit })));

// ---------------------------------------------------------------------------
// RouterComponent declared at module scope so its identity is stable across
// re-renders of App (e.g. pendingCount polls). A component declared inside the
// render function gets a fresh identity every render, causing React to unmount
// and remount the entire router — wiping its history state and breaking
// client-side navigation.
// ---------------------------------------------------------------------------
const RouterComponent = forcedDemo ? HashRouter : BrowserRouter;
const basename = forcedDemo ? '/ollama-mesh/demo' : '/';

// ---------------------------------------------------------------------------
// AppShell — rendered inside the Router so it can call useLocation.
// Passes location.pathname as a resetKey to ErrorBoundary so the boundary
// fully resets (clears stuck error/loading state) on every navigation.
// ---------------------------------------------------------------------------
interface AppShellProps {
  session: SessionData;
  onLogout: () => void;
  pendingCount: number;
}

function AppShell({ session, onLogout, pendingCount }: AppShellProps) {
  const location = useLocation();
  const pathname = location.pathname;

  return (
    <div className="min-h-screen bg-background text-foreground transition-colors duration-300">
      <Sidebar onLogout={onLogout} session={session} pendingCount={pendingCount} />
      <main className="md:ml-64 min-h-screen pt-14 md:pt-0">
        <DemoBanner />
        <BudgetBanner />
        <div className="p-4 sm:p-6 lg:p-8 max-w-[1600px] mx-auto">
          {/*
            resetKey=pathname on ErrorBoundary: getDerivedStateFromProps clears
            `error` whenever pathname changes, so a caught error on one page
            doesn't keep the next page stuck in the error view. This does NOT
            unmount/remount the boundary or its children — it's a state reset,
            not a remount.
          */}
          <ErrorBoundary resetKey={pathname}>
            <Suspense
              fallback={
                <div className="flex items-center justify-center h-32 text-muted-foreground text-sm">
                  Loading...
                </div>
              }
            >
              <Routes>
                <Route path="/" element={<Dashboard />} />
                <Route path="/gpu-nodes" element={<GPUNodes />} />
                <Route path="/api-keys" element={<APIKeys />} />
                <Route path="/routing" element={<Routing />} />
                <Route path="/metrics" element={<Metrics />} />
                <Route path="/settings" element={<SettingsPage />} />
                <Route path="/analytics" element={<Analytics />} />
                <Route path="/models" element={<Models />} />
                <Route path="/model-advisor" element={<ModelAdvisor />} />
                <Route path="/requests" element={<Requests />} />
                <Route path="/warmup" element={<Warmup />} />
                {session.role === 'admin' && <Route path="/users" element={<Users />} />}
                {session.role === 'admin' && <Route path="/system-audit" element={<SystemAudit />} />}
                <Route path="*" element={<Navigate to="/" replace />} />
              </Routes>
            </Suspense>
          </ErrorBoundary>
        </div>
      </main>
    </div>
  );
}

// ---------------------------------------------------------------------------
// App
// ---------------------------------------------------------------------------
// The public GitHub Pages demo (forcedDemo) never talks to a real backend —
// api.ts's DEMO branch serves mockData for every call — so this session is
// cosmetic routing only, not an auth bypass. The real product's adminAuth
// middleware (auth.go, exact Bearer-token match) is what actually gates admin
// APIs, and forcedDemo can only be true if VITE_FORCE_DEMO was set at build
// time, which only .github/workflows/pages.yml does — `make build` (the
// binary users install) never sets it, so real installs always hit Login.
const DEMO_SESSION: SessionData = {
  token: 'demo',
  role: 'admin',
  username: 'demo',
  mustChangePassword: false,
};

function App() {
  const [session, setSession] = useState<SessionData | null>(() => forcedDemo ? DEMO_SESSION : loadSession());
  const [pendingCount, setPendingCount] = useState(0);

  // Poll pending user count when admin
  useEffect(() => {
    if (!session || session.role !== 'admin' || forcedDemo) return;
    let active = true;
    const poll = async () => {
      try {
        const count = await getPendingUserCount();
        if (active) setPendingCount(count);
      } catch { /* ignore */ }
    };
    poll();
    const id = setInterval(poll, 30_000);
    return () => { active = false; clearInterval(id); };
  }, [session]);

  function handleLogout() {
    logout();
    // Demo has no Login screen to fall through to (see DEMO_SESSION above) —
    // reset to the same fake session instead of null so clicking Logout on
    // the public demo doesn't strand the visitor on a real login form.
    setSession(forcedDemo ? DEMO_SESSION : null);
  }

  function handleLoginSuccess(data: SessionData) {
    // Reset the URL to the app root so BrowserRouter lands on the dashboard
    // rather than /admin/login or /login. Target the app base, not '/': under
    // the GitHub Pages demo the app is at /ollama-mesh/demo/ and resetting to
    // '/' would navigate out to the domain root.
    const home = basename === '/' ? '/' : basename + '/';
    if (window.location.pathname !== home) {
      window.history.replaceState({}, '', home);
    }
    setSession(data);
  }

  function handleForceChangeSuccess(updated: SessionData) {
    setSession(updated);
  }

  // Detect if visitor is on the user portal path (/login but not /admin/login)
  const isUserPath = typeof window !== 'undefined' &&
    window.location.pathname.endsWith('/login');

  if (!session) {
    return (
      <ThemeProvider>
        <Login onSuccess={handleLoginSuccess} mode={isUserPath ? 'user' : 'admin'} />
      </ThemeProvider>
    );
  }

  if (session.mustChangePassword) {
    return (
      <ThemeProvider>
        <ForceChangePassword session={session} onSuccess={handleForceChangeSuccess} />
      </ThemeProvider>
    );
  }

  if (session.role === 'user') {
    return (
      <ThemeProvider>
        <UserPortal session={session} onLogout={handleLogout} />
      </ThemeProvider>
    );
  }

  return (
    <ThemeProvider>
      <RouterComponent {...(forcedDemo ? {} : (basename === '/' ? {} : { basename }))}>
        <AppShell session={session} onLogout={handleLogout} pendingCount={pendingCount} />
      </RouterComponent>
    </ThemeProvider>
  );
}

export default App;
