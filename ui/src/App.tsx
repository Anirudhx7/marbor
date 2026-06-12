import { BrowserRouter, Routes, Route } from 'react-router-dom';
import { ThemeProvider } from './hooks/useTheme';
import { useDemoMode } from './hooks/useDemoMode';
import { Sidebar } from './components/Sidebar';
import { Dashboard } from './pages/Dashboard';
import { GPUNodes } from './pages/GPUNodes';
import { APIKeys } from './pages/APIKeys';
import { Routing } from './pages/Routing';
import { Metrics } from './pages/Metrics';
import { SettingsPage } from './pages/Settings';
import { Analytics } from './pages/Analytics';
import { Models } from './pages/Models';
import { ModelAdvisor } from './pages/ModelAdvisor';
import { Requests } from './pages/Requests';

function DemoBanner() {
  const { demoMode } = useDemoMode();
  if (!demoMode) return null;
  return (
    <div className="bg-amber-500 text-black text-sm font-medium text-center py-1.5 px-4">
      Demo mode — all data shown is mock data, not your cluster. Disable it in Settings.
    </div>
  );
}

function App() {
  return (
    <ThemeProvider>
      <BrowserRouter>
        <div className="min-h-screen bg-background text-foreground transition-colors duration-300">
          <Sidebar />
          <main className="ml-64 min-h-screen">
            <DemoBanner />
            <div className="p-6 lg:p-8 max-w-[1600px] mx-auto">
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
            </div>
          </main>
        </div>
      </BrowserRouter>
    </ThemeProvider>
  );
}

export default App;
