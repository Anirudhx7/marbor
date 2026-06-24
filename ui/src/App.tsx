import { lazy, Suspense, Component, ReactNode, useState } from 'react';

class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  constructor(props: { children: ReactNode }) {
    super(props);
    this.state = { error: null };
  }
  static getDerivedStateFromError(error: Error) { return { error }; }
  render() {
    if (this.state.error) {
      return (
        <div className="p-8 text-center">
          <p className="text-destructive font-semibold mb-2">Page failed to load</p>
          <p className="text-xs text-muted-foreground font-mono">{this.state.error.message}</p>
          <button onClick={() => this.setState({ error: null })} className="mt-4 px-3 py-1.5 text-xs bg-secondary rounded-lg text-foreground hover:bg-secondary/80 cursor-pointer">Retry</button>
        </div>
      );
    }
    return this.props.children;
  }
}
import { BrowserRouter, Routes, Route } from 'react-router-dom';
import { ThemeProvider } from './hooks/useTheme';
import { forcedDemo } from './hooks/useDemoMode';
import { Sidebar } from './components/Sidebar';
import { DemoBanner } from './components/DemoBanner';
import { Login } from './components/Login';
import { clearAdminToken } from './lib/api';

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

const basename = forcedDemo ? '/ollama-mesh/demo' : '/';

function App() {
  const [authed, setAuthed] = useState<boolean>(
    () => forcedDemo || !!localStorage.getItem('adminToken')
  );

  function handleLogout() {
    clearAdminToken();
    setAuthed(false);
  }

  if (!authed) {
    return (
      <ThemeProvider>
        <Login onSuccess={() => setAuthed(true)} />
      </ThemeProvider>
    );
  }

  return (
    <ThemeProvider>
      <BrowserRouter basename={basename}>
        <div className="min-h-screen bg-background text-foreground transition-colors duration-300">
          <Sidebar onLogout={handleLogout} />
          <main className="md:ml-64 min-h-screen">
            <DemoBanner />
            <div className="pt-14 md:pt-0 p-4 sm:p-6 lg:p-8 max-w-[1600px] mx-auto">
              <ErrorBoundary>
              <Suspense fallback={<div className="flex items-center justify-center h-32 text-muted-foreground text-sm">Loading...</div>}>
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
                </Routes>
              </Suspense>
              </ErrorBoundary>
            </div>
          </main>
        </div>
      </BrowserRouter>
    </ThemeProvider>
  );
}

export default App;
