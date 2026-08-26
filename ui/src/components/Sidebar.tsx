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

  const isAdmin = session?.role === 'admin';

  // Smooth label: keep in DOM but collapse width/opacity for ultra-smooth fade
  const labelClass = `whitespace-nowrap overflow-hidden transition-all duration-300 ease-[cubic-bezier(0.32,0.72,0,1)] ${collapsed ? 'max-w-0 opacity-0 translate-x-1' : 'max-w-[160px] opacity-100 translate-x-0'}`;

  const linkBase = 'group flex items-center rounded-lg text-sm font-medium transition-colors duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-0';

  const sidebarContent = (
    <>
      {/* Header */}
      <div className={`h-16 flex items-center border-b border-border shrink-0 transition-[padding] duration-300 ease-[cubic-bezier(0.32,0.72,0,1)] ${collapsed ? 'px-2 justify-center' : 'px-5 justify-between'}`}>
        <div className={`flex items-center transition-all duration-300 ${collapsed ? 'gap-0' : 'gap-3'}`}>
          <div className="w-8 h-8 rounded-lg flex items-center justify-center shrink-0 transition-transform duration-300" style={{ background: '#1a1714' }}>
            <svg width="22" height="22" viewBox="0 0 100 100" fill="none" aria-hidden="true">
              <path d="M30 35 L30 65 M30 50 L50 35 L50 65 M50 50 L70 35 L70 65" stroke="#d4a853" strokeWidth="8" strokeLinecap="round" strokeLinejoin="round"/>
              <circle cx="75" cy="75" r="8" fill="#a87f3a" />
            </svg>
          </div>
          <div className={`flex flex-col overflow-hidden transition-all duration-300 ease-[cubic-bezier(0.32,0.72,0,1)] ${collapsed ? 'max-w-0 opacity-0 -ml-2' : 'max-w-[120px] opacity-100 ml-0'}`}>
            <span className="font-semibold text-foreground text-sm tracking-tight whitespace-nowrap">
              Marbor
            </span>
            <span className="text-[10px] font-medium text-muted-foreground leading-none whitespace-nowrap">v{version}</span>
          </div>
        </div>
        <button
          onClick={() => setIsOpen(false)}
          className="md:hidden min-w-[44px] min-h-[44px] flex items-center justify-center p-2 rounded-md text-muted-foreground hover:text-foreground hover:bg-secondary transition-colors cursor-pointer shrink-0"
          aria-label="Close menu"
        >
          <X className="w-5 h-5" strokeWidth={1.75} />
        </button>
      </div>

      {/* Nav - slim overlay scrollbar, hidden when collapsed */}
      <nav className={`sidebar-nav flex-1 py-3 space-y-1 overflow-y-auto overflow-x-hidden transition-[padding] duration-300 ease-[cubic-bezier(0.32,0.72,0,1)] ${collapsed ? 'collapsed px-1.5' : 'px-3'}`}>
        {navItems.map((item) => {
          const Icon = item.icon;
          return (
            <div key={item.path} className="relative group/nav">
              <NavLink
                to={item.path}
                title={collapsed ? item.label : undefined}
                className={({ isActive }) =>
                  `${linkBase} ${collapsed ? 'justify-center p-2.5 mx-0.5' : 'gap-0 px-3 py-2.5'} ${
                    isActive
                      ? 'bg-primary/10 text-primary shadow-[inset_0_0_0_1px_hsl(var(--primary)/0.12)]'
                      : 'text-muted-foreground hover:text-foreground hover:bg-secondary/70'
                  }`
                }
              >
                <span className={`flex items-center justify-center shrink-0 rounded-md transition-all duration-300 ${collapsed ? 'w-6 h-6' : 'w-5 h-5'}`}>
                  <Icon className={`${collapsed ? 'w-[18px] h-[18px]' : 'w-[18px] h-[18px]'} shrink-0 transition-transform duration-300 group-hover/nav:scale-[1.04]`} strokeWidth={1.75} />
                </span>
                <span className={labelClass}>{item.label}</span>
              </NavLink>
              {/* Premium tooltip when collapsed */}
              {collapsed && (
                <span className="pointer-events-none absolute left-full top-1/2 z-50 ml-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg opacity-0 group-hover/nav:opacity-100 transition-opacity duration-150">
                  {item.label}
                </span>
              )}
            </div>
          );
        })}
        {isAdmin && (
          <>
            <div className="relative group/nav">
              <NavLink
                to="/users"
                title={collapsed ? 'Users' : undefined}
                className={({ isActive }) =>
                  `${linkBase} ${collapsed ? 'justify-center p-2.5 mx-0.5' : 'gap-0 px-3 py-2.5'} ${
                    isActive ? 'bg-primary/10 text-primary shadow-[inset_0_0_0_1px_hsl(var(--primary)/0.12)]' : 'text-muted-foreground hover:text-foreground hover:bg-secondary/70'
                  }`
                }
              >
                <span className={`flex items-center justify-center shrink-0 rounded-md ${collapsed ? 'w-6 h-6' : 'w-5 h-5'}`}>
                  <Users className={`${collapsed ? 'w-[18px] h-[18px]' : 'w-[18px] h-[18px]'} shrink-0`} strokeWidth={1.75} />
                </span>
                <span className={`${labelClass} flex-1`}>Users</span>
                {!collapsed && pendingCount > 0 && (
                  <span className="ml-auto px-1.5 py-0.5 text-[10px] font-bold bg-amber-500/15 text-amber-700 dark:text-amber-300 rounded-full leading-none border border-amber-500/20">
                    {pendingCount}
                  </span>
                )}
              </NavLink>
              {collapsed && (
                <span className="pointer-events-none absolute left-full top-1/2 z-50 ml-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg opacity-0 group-hover/nav:opacity-100 transition-opacity duration-150">
                  Users{pendingCount > 0 ? ` (${pendingCount})` : ''}
                </span>
              )}
              {collapsed && pendingCount > 0 && (
                <span className="absolute -top-1 -right-1 w-2 h-2 bg-amber-500 rounded-full ring-2 ring-card pointer-events-none" />
              )}
            </div>
            <div className="relative group/nav">
              <NavLink
                to="/system-audit"
                title={collapsed ? 'Audit Trail' : undefined}
                className={({ isActive }) =>
                  `${linkBase} ${collapsed ? 'justify-center p-2.5 mx-0.5' : 'gap-0 px-3 py-2.5'} ${
                    isActive ? 'bg-primary/10 text-primary shadow-[inset_0_0_0_1px_hsl(var(--primary)/0.12)]' : 'text-muted-foreground hover:text-foreground hover:bg-secondary/70'
                  }`
                }
              >
                <span className={`flex items-center justify-center shrink-0 rounded-md ${collapsed ? 'w-6 h-6' : 'w-5 h-5'}`}>
                  <Shield className={`${collapsed ? 'w-[18px] h-[18px]' : 'w-[18px] h-[18px]'} shrink-0`} strokeWidth={1.75} />
                </span>
                <span className={labelClass}>Audit Trail</span>
              </NavLink>
              {collapsed && (
                <span className="pointer-events-none absolute left-full top-1/2 z-50 ml-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg opacity-0 group-hover/nav:opacity-100 transition-opacity duration-150">
                  Audit Trail
                </span>
              )}
            </div>
          </>
        )}
        <div className="relative group/nav">
          <NavLink
            to="/settings"
            title={collapsed ? 'Settings' : undefined}
            className={({ isActive }) =>
              `${linkBase} ${collapsed ? 'justify-center p-2.5 mx-0.5' : 'gap-0 px-3 py-2.5'} ${
                isActive ? 'bg-primary/10 text-primary shadow-[inset_0_0_0_1px_hsl(var(--primary)/0.12)]' : 'text-muted-foreground hover:text-foreground hover:bg-secondary/70'
              }`
            }
          >
            <span className={`flex items-center justify-center shrink-0 rounded-md ${collapsed ? 'w-6 h-6' : 'w-5 h-5'}`}>
              <Settings className={`${collapsed ? 'w-[18px] h-[18px]' : 'w-[18px] h-[18px]'} shrink-0`} strokeWidth={1.75} />
            </span>
            <span className={labelClass}>Settings</span>
          </NavLink>
          {collapsed && (
            <span className="pointer-events-none absolute left-full top-1/2 z-50 ml-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg opacity-0 group-hover/nav:opacity-100 transition-opacity duration-150">
              Settings
            </span>
          )}
        </div>
      </nav>

      {/* Footer */}
      <div className={`border-t border-border shrink-0 space-y-1 transition-[padding] duration-300 ease-[cubic-bezier(0.32,0.72,0,1)] ${collapsed ? 'p-2' : 'p-3'}`}>
        {/* Desktop collapse/expand toggle */}
        {onToggleCollapsed && (
          <div className="relative group/nav">
            <button
              onClick={onToggleCollapsed}
              aria-label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
              aria-expanded={!collapsed}
              className={`hidden md:flex items-center w-full rounded-lg text-sm font-medium text-muted-foreground hover:text-foreground hover:bg-secondary/70 transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-0 cursor-pointer ${collapsed ? 'justify-center p-2.5' : 'gap-3 px-3 py-2.5'}`}
            >
              <span className={`flex items-center justify-center shrink-0 rounded-md ${collapsed ? 'w-6 h-6' : 'w-5 h-5'}`}>
                {collapsed ? <PanelLeft className="w-[18px] h-[18px] transition-transform duration-300" strokeWidth={1.75} /> : <PanelLeftClose className="w-[18px] h-[18px] transition-transform duration-300" strokeWidth={1.75} />}
              </span>
              <span className={labelClass}>Collapse</span>
            </button>
            {collapsed && (
              <span className="pointer-events-none absolute left-full top-1/2 z-50 ml-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg opacity-0 group-hover/nav:opacity-100 transition-opacity duration-150">
                Expand sidebar
              </span>
            )}
          </div>
        )}
        {session?.username && (
          <div className={`overflow-hidden transition-all duration-300 ease-[cubic-bezier(0.32,0.72,0,1)] ${collapsed ? 'max-h-0 opacity-0' : 'max-h-10 opacity-100'}`}>
            <div className="px-3 py-2 text-xs text-muted-foreground truncate">
              Signed in as <span className="font-medium text-foreground">{session.username}</span>
            </div>
          </div>
        )}
        {forcedDemo && (
          <div className="relative group/nav">
            <a
              href="../"
              className={`flex items-center rounded-lg text-sm font-medium text-muted-foreground hover:text-foreground hover:bg-secondary/70 transition-colors duration-200 ${collapsed ? 'justify-center p-2.5' : 'gap-3 px-3 py-2.5'}`}
            >
              <span className={`flex items-center justify-center shrink-0 ${collapsed ? 'w-6 h-6' : 'w-5 h-5'}`}>
                <ArrowLeft className={`${collapsed ? 'w-[18px] h-[18px]' : 'w-[18px] h-[18px]'} shrink-0`} strokeWidth={1.75} />
              </span>
              <span className={labelClass}>Back to website</span>
            </a>
            {collapsed && (
              <span className="pointer-events-none absolute left-full top-1/2 z-50 ml-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg opacity-0 group-hover/nav:opacity-100 transition-opacity duration-150">
                Back to website
              </span>
            )}
          </div>
        )}
        <div className="relative group/nav">
          <button
            onClick={toggleTheme}
            className={`flex items-center w-full rounded-lg text-sm font-medium text-muted-foreground hover:text-foreground hover:bg-secondary/70 transition-colors duration-200 ${collapsed ? 'justify-center p-2.5' : 'gap-3 px-3 py-2.5'}`}
          >
            <span className={`flex items-center justify-center shrink-0 ${collapsed ? 'w-6 h-6' : 'w-5 h-5'}`}>
              {theme === 'dark' ? <Sun className={`${collapsed ? 'w-[18px] h-[18px]' : 'w-[18px] h-[18px]'} shrink-0`} strokeWidth={1.75} /> : <Moon className={`${collapsed ? 'w-[18px] h-[18px]' : 'w-[18px] h-[18px]'} shrink-0`} strokeWidth={1.75} />}
            </span>
            <span className={labelClass}>{theme === 'dark' ? 'Light Mode' : 'Dark Mode'}</span>
          </button>
          {collapsed && (
            <span className="pointer-events-none absolute left-full top-1/2 z-50 ml-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg opacity-0 group-hover/nav:opacity-100 transition-opacity duration-150">
              {theme === 'dark' ? 'Light Mode' : 'Dark Mode'}
            </span>
          )}
        </div>
        {onLogout && (
          <div className="relative group/nav">
            <button
              onClick={onLogout}
              className={`flex items-center w-full rounded-lg text-sm font-medium text-muted-foreground hover:text-destructive hover:bg-destructive/10 transition-colors duration-200 ${collapsed ? 'justify-center p-2.5' : 'gap-3 px-3 py-2.5'}`}
            >
              <span className={`flex items-center justify-center shrink-0 ${collapsed ? 'w-6 h-6' : 'w-5 h-5'}`}>
                <LogOut className={`${collapsed ? 'w-[18px] h-[18px]' : 'w-[18px] h-[18px]'} shrink-0`} strokeWidth={1.75} />
              </span>
              <span className={labelClass}>Logout</span>
            </button>
            {collapsed && (
              <span className="pointer-events-none absolute left-full top-1/2 z-50 ml-2 -translate-y-1/2 whitespace-nowrap rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs font-medium text-popover-foreground shadow-lg opacity-0 group-hover/nav:opacity-100 transition-opacity duration-150">
                Logout
              </span>
            )}
          </div>
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
            <Menu className="w-5 h-5" strokeWidth={1.75} />
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
          <span className="px-2 py-0.5 text-[10px] font-bold bg-amber-500/15 text-amber-700 dark:text-amber-300 rounded-full leading-none border border-amber-500/20">
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
          fixed left-0 top-0 z-50 bg-card border-r border-border flex flex-col h-screen supports-[height:100dvh]:h-dvh will-change-[width]
          transition-[width,transform] duration-300 ease-[cubic-bezier(0.32,0.72,0,1)]
          w-64 ${collapsed ? 'md:w-[68px]' : 'md:w-64'}
          ${isOpen ? 'translate-x-0' : '-translate-x-full md:translate-x-0'}
        `}
      >
        {sidebarContent}
      </aside>
    </>
  );
}
