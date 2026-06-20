import { useState, useEffect } from 'react';
import { NavLink, useLocation } from 'react-router-dom';
import {
  LayoutDashboard,
  Cpu,
  Key,
  Route,
  BarChart3,
  Settings,
  Moon,
  Sun,
  TrendingUp,
  Package,
  Compass,
  Activity,
  Menu,
  X,
  ArrowLeft,
} from 'lucide-react';
import { useTheme } from '../hooks/useTheme';
import { forcedDemo } from '../hooks/useDemoMode';

const navItems = [
  { path: '/', label: 'Dashboard', icon: LayoutDashboard },
  { path: '/gpu-nodes', label: 'GPU Nodes', icon: Cpu },
  { path: '/models', label: 'Models', icon: Package },
  { path: '/model-advisor', label: 'Model Advisor', icon: Compass },
  { path: '/analytics', label: 'Analytics', icon: TrendingUp },
  { path: '/requests', label: 'Requests', icon: Activity },
  { path: '/api-keys', label: 'API Keys', icon: Key },
  { path: '/routing', label: 'Routing', icon: Route },
  { path: '/metrics', label: 'Metrics', icon: BarChart3 },
  { path: '/settings', label: 'Settings', icon: Settings },
];

export function Sidebar() {
  const [isOpen, setIsOpen] = useState(false);
  const { theme, toggleTheme } = useTheme();
  const location = useLocation();

  useEffect(() => {
    setIsOpen(false);
  }, [location.pathname]);

  useEffect(() => {
    document.body.style.overflow = isOpen ? 'hidden' : '';
    return () => { document.body.style.overflow = ''; };
  }, [isOpen]);

  const linkClass = ({ isActive }: { isActive: boolean }) =>
    `flex items-center gap-3 px-3 py-2 text-sm font-medium rounded-md transition-colors ${
      isActive
        ? 'bg-primary/10 text-primary'
        : 'text-muted-foreground hover:text-foreground hover:bg-secondary'
    }`;

  const sidebarContent = (
    <>
      <div className="h-16 flex items-center px-6 border-b border-border shrink-0">
        <div className="flex items-center gap-3">
          <div className="w-8 h-8 rounded-lg flex items-center justify-center shrink-0" style={{ background: '#1a1714' }}>
            <svg width="22" height="22" viewBox="0 0 100 100" fill="none" aria-hidden="true">
              <path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65" stroke="#d4a853" strokeWidth="8" strokeLinecap="round" strokeLinejoin="round"/>
              <circle cx="75" cy="75" r="8" fill="#a87f3a" />
            </svg>
          </div>
          <div className="flex flex-col">
            <span className="font-semibold text-foreground text-sm tracking-tight">
              ollama<span className="text-primary">-mesh</span>
            </span>
            <span className="text-[10px] font-medium text-muted-foreground leading-none">v{__APP_VERSION__}</span>
          </div>
        </div>
      </div>

      <nav className="flex-1 px-3 py-4 space-y-1 overflow-y-auto">
        {navItems.map((item) => {
          const Icon = item.icon;
          return (
            <NavLink key={item.path} to={item.path} className={linkClass}>
              <Icon className="w-4 h-4 shrink-0" />
              {item.label}
            </NavLink>
          );
        })}
      </nav>

      <div className="p-4 border-t border-border shrink-0 space-y-1">
        {forcedDemo && (
          <a
            href="/ollama-mesh/"
            className="w-full flex items-center gap-3 px-3 py-2 text-sm font-medium rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
          >
            <ArrowLeft className="w-4 h-4 shrink-0" />
            <span>Back to website</span>
          </a>
        )}
        <button
          onClick={toggleTheme}
          className="w-full flex items-center gap-3 px-3 py-2 text-sm font-medium rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
        >
          {theme === 'dark' ? (
            <><Sun className="w-4 h-4 shrink-0" /><span>Light Mode</span></>
          ) : (
            <><Moon className="w-4 h-4 shrink-0" /><span>Dark Mode</span></>
          )}
        </button>
      </div>
    </>
  );

  return (
    <>
      {/* Mobile hamburger */}
      <button
        className="md:hidden fixed top-3 left-3 z-50 p-2 rounded-md bg-card border border-border text-foreground shadow-sm"
        onClick={() => setIsOpen(o => !o)}
        aria-label={isOpen ? 'Close menu' : 'Open menu'}
      >
        {isOpen ? <X className="w-5 h-5" /> : <Menu className="w-5 h-5" />}
      </button>

      {/* Mobile overlay */}
      {isOpen && (
        <div
          className="md:hidden fixed inset-0 z-40 bg-black/50 backdrop-blur-sm"
          onClick={() => setIsOpen(false)}
        />
      )}

      {/* Sidebar - desktop always visible, mobile slide-in */}
      <aside
        className={`
          fixed left-0 top-0 z-50 w-64 bg-card border-r border-border flex flex-col h-screen
          transition-transform duration-300 ease-in-out
          ${isOpen ? 'translate-x-0' : '-translate-x-full md:translate-x-0'}
        `}
      >
        {sidebarContent}
      </aside>
    </>
  );
}
