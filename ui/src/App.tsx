import { BrowserRouter, Routes, Route } from 'react-router-dom';
import { ThemeProvider } from './hooks/useTheme';
import { forcedDemo } from './hooks/useDemoMode';
import { Sidebar } from './components/Sidebar';
import { DemoBanner } from './components/DemoBanner';
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

const basename = forcedDemo ? '/ollama-mesh' : '/';

function App() {
  return (
    <ThemeProvider>
      <BrowserRouter basename={basename}>
        <div className="min-h-screen bg-background text-foreground transition-colors duration-300">
          <Sidebar />
          <main className="md:ml-64 min-h-screen">
            <DemoBanner />
            <div className="pt-14 md:pt-0 p-4 sm:p-6 lg:p-8 max-w-[1600px] mx-auto">
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
