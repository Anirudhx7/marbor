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
  LogOut,
  Users,
  Flame,
  Shield,
  PanelLeftClose,
  PanelLeft,
} from 'lucide-react';
import { useTheme } from '../hooks/useTheme';
import { forcedDemo } from '../hooks/useDemoMode';
import { fetchHealth } from '../lib/api';
import type { SessionData } from '../types';

const navItems = [
  { path: '/', label: 'Dashboard', icon: LayoutDashboard },
  { path: '/gpu-nodes', label: 'GPU Nodes', icon: Cpu },
  { path: '/warmup', label: 'Warmup', icon: Flame },
  { path: '/models', label: 'Models', icon: Package },
  { path: '/model-advisor', label: 'Model Advisor', icon: Compass },
  { path: '/analytics', label: 'Analytics', icon: TrendingUp },
  { path: '/requests', label: 'Requests', icon: Activity },
  { path: '/api-keys', label: 'API Keys', icon: Key },
  { path: '/routing', label: 'Routing', icon: Route },
  { path: '/metrics', label: 'Metrics', icon: BarChart3 },
];

interface SidebarProps {
  onLogout?: () => void;
  session?: SessionData | null;
  pendingCount?: number;
  collapsed?: boolean;
  onToggleCollapsed?: () => void;
}

export function Sidebar({ onLogout, session, pendingCount = 0, collapsed = false, onToggleCollapsed }: SidebarProps) {
  const [isOpen, setIsOpen] = useState(false);
  const { theme, toggleTheme } = useTheme();
  const location = useLocation();
  const [version, setVersion] = useState<string>(__APP_VERSION__);

  useEffect(() => {
    setIsOpen(false);
  }, [location.pathname]);

  useEffect(() => {
    fetchHealth().then(h => {
      if (h.version) setVersion(h.version);
    }).catch(() => {});
  }, []);

  useEffect(() => {
    document.body.style.overflow = isOpen ? 'hidden' : '';
    return () => { document.body.style.overflow = ''; };
  }, [isOpen]);

  const linkClass = ({ isActive }: { isActive: boolean }) =>
    `flex items-center ${collapsed ? 'justify-center' : 'gap-3'} px-3 py-2 min-h-[44px] text-sm font-medium rounded-md transition-colors ${
      isActive
        ? 'bg-primary/10 text-primary'
        : 'text-muted-foreground hover:text-foreground hover:bg-secondary'
    }`;

  const isAdmin = session?.role === 'admin';

  const sidebarContent = (
    <>
      <div className={`h-16 flex items-center border-b border-border shrink-0 ${collapsed ? 'justify-center px-2' : 'justify-between px-6'}`}>
        <div className="flex items-center gap-3">
          <div className="w-8 h-8 rounded-lg flex items-center justify-center shrink-0" style={{ background: '#1a1714' }}>
            <svg width="22" height="22" viewBox="0 0 100 100" fill="none" aria-hidden="true">
              <path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65" stroke="#d4a853" strokeWidth="8" strokeLinecap="round" strokeLinejoin="round"/>
              <circle cx="75" cy="75" r="8" fill="#a87f3a" />
            </svg>
          </div>
          {!collapsed && (
            <div className="flex flex-col">
              <span className="font-semibold text-foreground text-sm tracking-tight">
                Marbor
              </span>
              <span className="text-[10px] font-medium text-muted-foreground leading-none">v{version}</span>
            </div>
          )}
        </div>
        {!collapsed && (
          <button
            onClick={() => setIsOpen(false)}
            className="md:hidden min-w-[44px] min-h-[44px] flex items-center justify-center p-2 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors cursor-pointer"
            aria-label="Close menu"
          >
            <X className="w-5 h-5" />
          </button>
        )}
        {collapsed && (
          <button
            onClick={() => setIsOpen(false)}
            className="md:hidden min-w-[44px] min-h-[44px] flex items-center justify-center p-2 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors cursor-pointer"
            aria-label="Close menu"
          >
            <X className="w-5 h-5" />
          </button>
        )}
      </div>

      <nav className={`flex-1 py-4 space-y-1 overflow-y-auto overflow-x-hidden ${collapsed ? 'px-2' : 'px-3'}`}>
        {navItems.map((item) => {
          const Icon = item.icon;
          return (
            <NavLink
              key={item.path}
              to={item.path}
              className={linkClass}
              title={collapsed ? item.label : undefined}
              aria-label={collapsed ? item.label : undefined}
            >
              <Icon className="w-4 h-4 shrink-0" />
              {!collapsed && item.label}
            </NavLink>
          );
        })}
        {isAdmin && (
          <>
            <NavLink to="/users" className={linkClass} title={collapsed ? 'Users' : undefined} aria-label={collapsed ? 'Users' : undefined}>
              <Users className="w-4 h-4 shrink-0" />
              {!collapsed && (
                <>
                  <span className="flex-1">Users</span>
                  {pendingCount > 0 && (
                    <span className="px-1.5 py-0.5 text-[10px] font-bold bg-amber-500/20 text-amber-600 dark:text-amber-400 rounded-full leading-none">
                      {pendingCount}
                    </span>
                  )}
                </>
              )}
            </NavLink>
            <NavLink to="/system-audit" className={linkClass} title={collapsed ? 'Audit Trail' : undefined} aria-label={collapsed ? 'Audit Trail' : undefined}>
              <Shield className="w-4 h-4 shrink-0" />
              {!collapsed && 'Audit Trail'}
            </NavLink>
          </>
        )}
        <NavLink to="/settings" className={linkClass} title={collapsed ? 'Settings' : undefined} aria-label={collapsed ? 'Settings' : undefined}>
          <Settings className="w-4 h-4 shrink-0" />
          {!collapsed && 'Settings'}
        </NavLink>
      </nav>

      <div className={`border-t border-border shrink-0 space-y-1 ${collapsed ? 'p-2' : 'p-4'}`}>
        {/* Desktop collapse/expand toggle - hidden on mobile where slide-in drawer applies */}
        {onToggleCollapsed && (
          <button
            onClick={onToggleCollapsed}
            aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
            aria-expanded={!collapsed}
            title={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
            className={`hidden md:flex items-center min-h-[44px] text-sm font-medium rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 cursor-pointer w-full ${collapsed ? 'justify-center px-2' : 'gap-3 px-3 py-2'}`}
          >
            {collapsed ? <PanelLeft className="w-4 h-4 shrink-0" /> : <PanelLeftClose className="w-4 h-4 shrink-0" />}
            {!collapsed && <span>{'Collapse'}</span>}
          </button>
        )}
        {session?.username && !collapsed && (
          <div className="px-3 py-2 text-xs text-muted-foreground truncate">
            Signed in as <span className="font-medium text-foreground">{session.username}</span>
          </div>
        )}
        {forcedDemo && (
          <a
            // Relative on purpose: the demo deploys under whichever first
            // path segment this repo's Pages site uses, so "../" always
            // lands on that deployment's own landing page.
            href="../"
            title={collapsed ? 'Back to website' : undefined}
            aria-label={collapsed ? 'Back to website' : undefined}
            className={`flex items-center min-h-[44px] text-sm font-medium rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors ${collapsed ? 'justify-center px-2' : 'gap-3 px-3 py-2 w-full'}`}
          >
            <ArrowLeft className="w-4 h-4 shrink-0" />
            {!collapsed && <span>Back to website</span>}
          </a>
        )}
        <button
          onClick={toggleTheme}
          title={collapsed ? (theme === 'dark' ? 'Light Mode' : 'Dark Mode') : undefined}
          aria-label={collapsed ? (theme === 'dark' ? 'Light Mode' : 'Dark Mode') : undefined}
          className={`flex items-center min-h-[44px] text-sm font-medium rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors w-full ${collapsed ? 'justify-center px-2' : 'gap-3 px-3 py-2'}`}
        >
          {theme === 'dark' ? (
            <><Sun className="w-4 h-4 shrink-0" />{!collapsed && <span>Light Mode</span>}</>
          ) : (
            <><Moon className="w-4 h-4 shrink-0" />{!collapsed && <span>Dark Mode</span>}</>
          )}
        </button>
        {onLogout && (
          <button
            onClick={onLogout}
            title={collapsed ? 'Logout' : undefined}
            aria-label={collapsed ? 'Logout' : undefined}
            className={`flex items-center min-h-[44px] text-sm font-medium rounded-md text-muted-foreground hover:text-destructive hover:bg-destructive/10 transition-colors w-full ${collapsed ? 'justify-center px-2' : 'gap-3 px-3 py-2'}`}
          >
            <LogOut className="w-4 h-4 shrink-0" />
            {!collapsed && <span>Logout</span>}
          </button>
        )}
      </div>
    </>
  );

  return (
    <>
      {/* Mobile Header Bar */}
      <div className="md:hidden fixed top-0 left-0 right-0 h-14 bg-card border-b border-border flex items-center justify-between px-4 z-30 shadow-sm">
        <div className="flex items-center gap-3">
          <button
            onClick={() => setIsOpen(true)}
            className="min-w-[44px] min-h-[44px] flex items-center justify-center p-2 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors cursor-pointer"
            aria-label="Open menu"
          >
            <Menu className="w-5 h-5" />
          </button>
          <div className="flex items-center gap-2.5">
            <div className="w-6 h-6 rounded flex items-center justify-center shrink-0" style={{ background: '#1a1714' }}>
              <svg width="16" height="16" viewBox="0 0 100 100" fill="none" aria-hidden="true">
                <path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65" stroke="#d4a853" strokeWidth="8" strokeLinecap="round" strokeLinejoin="round"/>
                <circle cx="75" cy="75" r="8" fill="#a87f3a" />
              </svg>
            </div>
            <span className="font-semibold text-foreground text-sm tracking-tight">
              Marbor
            </span>
          </div>
        </div>
        {pendingCount > 0 && (
          <span className="px-2 py-0.5 text-[10px] font-bold bg-amber-500/20 text-amber-600 dark:text-amber-400 rounded-full leading-none">
            {pendingCount}
          </span>
        )}
      </div>

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
          fixed left-0 top-0 z-50 bg-card border-r border-border flex flex-col h-screen supports-[height:100dvh]:h-dvh
          transition-all duration-200 ease-in-out
          w-64 ${collapsed ? 'md:w-16' : 'md:w-64'}
          ${isOpen ? 'translate-x-0' : '-translate-x-full md:translate-x-0'}
        `}
      >
        {sidebarContent}
      </aside>
    </>
  );
}
