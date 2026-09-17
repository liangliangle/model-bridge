/**
 * App 根组件 — 顶部导航 + 主内容区 + 主题切换
 */
import { useEffect, useState } from "react";
import { BrowserRouter, Routes, Route, NavLink, useLocation } from "react-router-dom";
import Dashboard from "./components/Dashboard";
import AuditPanel from "./components/AuditPanel";
import ChannelConfig from "./components/ChannelConfig";
import McpConfig from "./components/McpConfig";
import ModelPriceConfig from "./components/ModelPriceConfig";
import Settings from "./components/Settings";
import AuthPage from "./components/AuthPage";
import { clearAdminToken, getAdminToken, invoke, setAdminToken, type AuthStatus } from "./lib/tauri";

interface NavItem {
  to: string;
  icon: string;
  label: string;
  children?: { to: string; label: string }[];
}

const NAV_ITEMS: NavItem[] = [
  { to: "/", icon: "◈", label: "仪表盘" },
  { to: "/audit", icon: "◉", label: "审计日志" },
  { to: "/channels", icon: "⬡", label: "渠道配置" },
  { to: "/mcp", icon: "⇄", label: "MCP 中继" },
  { to: "/prices", icon: "$", label: "模型价格" },
  { to: "/settings", icon: "⚙", label: "系统设置" },
];

function useTheme() {
  const [dark, setDark] = useState(() => {
    const saved = localStorage.getItem("theme");
    if (saved) return saved === "dark";
    return window.matchMedia("(prefers-color-scheme: dark)").matches;
  });

  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    localStorage.setItem("theme", dark ? "dark" : "light");
  }, [dark]);

  return { dark, toggle: () => setDark((d) => !d) };
}

function NavContent() {
  const location = useLocation();
  const { dark, toggle } = useTheme();
  const [openMenu, setOpenMenu] = useState<string | null>(null);

  useEffect(() => {
    setOpenMenu(null);
  }, [location.pathname]);

  return (
    <div className="flex h-screen flex-col bg-th-bg transition-colors duration-300">
      <nav className="z-20 flex h-16 shrink-0 items-center gap-6 border-b border-th-border bg-th-base px-6 shadow-[0_1px_3px_rgba(24,34,48,0.04)]">
        <NavLink to="/" end className="flex shrink-0 items-center gap-3">
          <span className="flex h-9 w-9 items-center justify-center rounded-md bg-slate-950 text-sm font-bold text-white">MB</span>
          <span className="hidden whitespace-nowrap sm:block"><span className="block font-display text-[16px] font-semibold tracking-tight text-th-text">模型网关</span><span className="mt-0.5 block text-[10px] text-th-text-m">代理与 MCP 管理</span></span>
        </NavLink>

        <div className="flex min-w-0 flex-1 items-center gap-1 overflow-visible">
          {NAV_ITEMS.map((item) => item.children ? (
            <div key={item.to} className="group relative shrink-0">
              <button onClick={() => setOpenMenu((current) => current === item.to ? null : item.to)} aria-expanded={openMenu === item.to} className={`flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${location.pathname.startsWith(item.to) ? "bg-th-accent-g font-medium text-th-accent" : "text-th-text-s hover:bg-th-hover hover:text-th-text"}`}>
                <span className="text-sm opacity-75">{item.icon}</span><span>{item.label}</span><span className="text-[10px]">▾</span>
              </button>
              <div className={`${openMenu === item.to ? "visible opacity-100" : "invisible opacity-0 pointer-events-none"} absolute left-0 top-full z-30 w-36 rounded-md border border-th-border bg-th-base p-1 shadow-lg transition-opacity`}>
                {item.children.map((child) => <NavLink key={child.to} to={child.to} className={({ isActive }) => `block rounded px-3 py-2 text-sm ${isActive ? "bg-th-accent-g font-medium text-th-accent" : "text-th-text-s hover:bg-th-hover hover:text-th-text"}`}>{child.label}</NavLink>)}
              </div>
            </div>
          ) : (
            <NavLink key={item.to} to={item.to} end={item.to === "/"} className={({ isActive }) => `flex shrink-0 items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${isActive ? "bg-th-accent-g font-medium text-th-accent" : "text-th-text-s hover:bg-th-hover hover:text-th-text"}`}><span className="text-sm opacity-75">{item.icon}</span><span>{item.label}</span></NavLink>
          ))}
        </div>

        <div className="flex shrink-0 items-center gap-3 border-l border-th-border pl-5 text-th-text-m">
          <span className="hidden text-xs text-th-text-s md:block">管理员</span>
          <span className="hidden items-center gap-1.5 text-[10px] text-th-text-m lg:flex"><i className="h-2 w-2 rounded-full bg-emerald-500" />运行中</span>
          <button onClick={toggle} className="flex h-8 w-8 items-center justify-center rounded-full border border-th-border text-xs font-medium hover:bg-th-hover" title={dark ? "切换亮色" : "切换暗色"}>管</button>
        </div>
      </nav>

      {/* 主内容区 */}
      <main className="min-h-0 flex-1 overflow-auto">
        <div className="p-7 max-w-[1500px] mx-auto animate-fade-in" key={location.pathname}>
          <Routes>
            <Route path="/" element={<Dashboard />} />
            <Route path="/audit" element={<AuditPanel />} />
            <Route path="/channels" element={<ChannelConfig />} />
            <Route path="/mcp" element={<McpConfig />} />
            <Route path="/prices" element={<ModelPriceConfig />} />
            <Route path="/settings" element={<Settings />} />
          </Routes>
        </div>
      </main>
    </div>
  );
}

export default function App() {
  const [authState, setAuthState] = useState<"checking" | "required" | "authenticated">("checking");
  const [authError, setAuthError] = useState<string | null>(null);
  const [authLoading, setAuthLoading] = useState(false);

  const checkAuth = async (token?: string) => {
    try {
      const status = await invoke<AuthStatus>("get_auth_status");
      if (!status.required || status.valid) {
        setAuthError(null);
        setAuthState("authenticated");
      } else {
        clearAdminToken();
        setAuthError(token ? "Invalid or missing admin token" : null);
        setAuthState("required");
      }
    } catch (error) {
      setAuthError(error instanceof Error ? error.message : "Unable to verify admin token");
      setAuthState("required");
    }
  };

  useEffect(() => {
    void checkAuth(getAdminToken() || undefined);
    const handleExpired = () => {
      clearAdminToken();
      setAuthError("Your admin token is no longer valid. Enter it again to continue.");
      setAuthState("required");
    };
    window.addEventListener("model-bridge-auth-expired", handleExpired);
    return () => window.removeEventListener("model-bridge-auth-expired", handleExpired);
  }, []);

  const handleAuthSubmit = async (token: string) => {
    setAuthLoading(true);
    setAuthError(null);
    setAdminToken(token);
    try {
      const status = await invoke<AuthStatus>("get_auth_status");
      if (!status.required || status.valid) {
        setAuthState("authenticated");
      } else {
        clearAdminToken();
        setAuthError("Invalid or missing admin token");
      }
    } catch (error) {
      clearAdminToken();
      setAuthError(error instanceof Error ? error.message : "Unable to verify admin token");
    } finally {
      setAuthLoading(false);
    }
  };

  if (authState === "checking") {
    return <div className="flex min-h-screen items-center justify-center bg-th-bg text-th-text-m"><div className="h-5 w-5 animate-spin rounded-full border-2 border-th-accent/30 border-t-th-accent" /></div>;
  }

  if (authState === "required") {
    return <AuthPage onSubmit={handleAuthSubmit} error={authError} loading={authLoading} />;
  }

  return (
    <BrowserRouter>
      <NavContent />
    </BrowserRouter>
  );
}
