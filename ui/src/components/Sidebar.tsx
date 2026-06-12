import { NavLink } from 'react-router-dom';
import {
  LayoutDashboard,
  Cpu,
  Key,
  Route,
  BarChart3,
  Settings,
  Server,
  Moon,
  Sun,
  TrendingUp,
  Package,
  Compass,
  Activity
} from 'lucide-react';
import { useTheme } from '../hooks/useTheme';

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
  const { theme, toggleTheme } = useTheme();

  return (
    <aside className="w-64 bg-card border-r border-border flex flex-col h-screen fixed left-0 top-0 z-40">
      {/* Logo */}
      <div className="h-16 flex items-center px-6 border-b border-border">
        <div className="flex items-center gap-3">
          <div className="w-8 h-8 rounded-lg bg-primary/10 flex items-center justify-center">
            <Server className="w-5 h-5 text-primary" />
          </div>
          <div className="flex flex-col">
            <span className="font-semibold text-foreground text-sm tracking-tight">Ollama Mesh</span>
            <span className="text-[10px] font-medium text-muted-foreground leading-none">v0.2.0</span>
          </div>
        </div>
      </div>

      {/* Navigation */}
      <nav className="flex-1 px-3 py-4 space-y-1">
        {navItems.map((item) => {
          const Icon = item.icon;
          return (
            <NavLink
              key={item.path}
              to={item.path}
              className={({ isActive }) =>
                `flex items-center gap-3 px-3 py-2 text-sm font-medium rounded-md transition-colors ${
                  isActive
                    ? 'bg-primary/10 text-primary'
                    : 'text-muted-foreground hover:text-foreground hover:bg-secondary'
                }`
              }
            >
              <Icon className="w-4 h-4" />
              {item.label}
            </NavLink>
          );
        })}
      </nav>

      {/* Theme Toggle */}
      <div className="p-4 border-t border-border">
        <button
          onClick={toggleTheme}
          className="w-full flex items-center gap-3 px-3 py-2 text-sm font-medium rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors"
        >
          {theme === 'dark' ? (
            <>
              <Sun className="w-4 h-4" />
              <span>Light Mode</span>
            </>
          ) : (
            <>
              <Moon className="w-4 h-4" />
              <span>Dark Mode</span>
            </>
          )}
        </button>
      </div>
    </aside>
  );
}
