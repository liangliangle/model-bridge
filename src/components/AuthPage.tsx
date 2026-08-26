import { FormEvent, useEffect, useRef, useState } from "react";

export interface AuthPageProps {
  /** Called with the trimmed token after the form passes local validation. */
  onSubmit: (token: string) => void | Promise<void>;
  /** Authentication error returned by the admin API. */
  error?: string | null;
  /** Prevents duplicate submissions while the token is being checked. */
  loading?: boolean;
}

/**
 * Full-screen admin authentication surface.
 *
 * The component intentionally owns only the input state. Token persistence and
 * the request that validates it belong to the app/request layer, which keeps
 * this page reusable for both the browser build and the embedded desktop UI.
 */
export default function AuthPage({ onSubmit, error, loading = false }: AuthPageProps) {
  const [token, setToken] = useState("");
  const [visible, setVisible] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const value = token.trim();
    if (!value || loading) return;
    void onSubmit(value);
  };

  return (
    <main className="relative flex min-h-screen items-center justify-center overflow-hidden bg-th-bg px-5 py-10 text-th-text transition-colors duration-300 sm:px-8">
      <div className="pointer-events-none absolute inset-0 opacity-90" aria-hidden="true">
        <div className="absolute inset-0 bg-[linear-gradient(to_right,var(--color-border)_1px,transparent_1px),linear-gradient(to_bottom,var(--color-border)_1px,transparent_1px)] bg-[size:42px_42px] opacity-[0.16] [mask-image:radial-gradient(ellipse_at_center,black,transparent_72%)]" />
      </div>

      <section className="relative grid w-full max-w-4xl overflow-hidden rounded-2xl border border-th-border bg-th-base/80 shadow-2xl shadow-slate-950/10 backdrop-blur-2xl lg:grid-cols-[1.05fr_0.95fr] dark:shadow-black/40">
        <div className="hidden border-r border-th-border bg-th-elev/50 p-10 lg:flex lg:flex-col lg:justify-between">
          <div>
            <div className="mb-10 flex items-center gap-3">
              <div className="flex h-10 w-10 items-center justify-center rounded-xl bg-gradient-to-br from-th-accent to-th-accent-d text-lg font-bold text-white shadow-lg shadow-th-accent/30 ring-1 ring-white/20">
                M
              </div>
              <div>
                <div className="font-display text-base font-semibold tracking-tight">Model Bridge</div>
                <div className="font-mono text-[10px] uppercase tracking-[0.22em] text-th-text-m">Admin Console</div>
              </div>
            </div>

            <div className="max-w-sm">
              <p className="mb-4 font-mono text-[10px] uppercase tracking-[0.24em] text-th-accent">Restricted surface</p>
              <h1 className="font-display text-4xl font-semibold leading-[1.05] tracking-tight text-th-text">
                Private access,
                <br />
                clearly controlled.
              </h1>
              <p className="mt-5 max-w-xs text-sm leading-6 text-th-text-s">
                Authenticate to continue to your Model Bridge workspace.
              </p>
            </div>
          </div>

          <div className="flex items-center gap-2 text-[11px] text-th-text-m">
            <span className="h-1.5 w-1.5 rounded-full bg-emerald-400 shadow-[0_0_0_3px_var(--color-accent-glow)]" />
            Local gateway ready
          </div>
        </div>

        <div className="p-7 sm:p-10">
          <div className="mb-8 flex items-center gap-3 lg:hidden">
            <div className="flex h-9 w-9 items-center justify-center rounded-xl bg-gradient-to-br from-th-accent to-th-accent-d text-sm font-bold text-white shadow-lg shadow-th-accent/30 ring-1 ring-white/20">
              M
            </div>
            <div>
              <div className="font-display text-sm font-semibold tracking-tight">Model Bridge</div>
              <div className="font-mono text-[9px] uppercase tracking-[0.2em] text-th-text-m">Admin Console</div>
            </div>
          </div>

          <div className="mb-8">
            <p className="mb-3 font-mono text-[10px] uppercase tracking-[0.22em] text-th-accent">Sign in</p>
            <h2 className="font-display text-2xl font-semibold tracking-tight text-th-text">Enter admin token</h2>
            <p className="mt-2 text-sm text-th-text-s">A valid token is required to access the console.</p>
          </div>

          <form onSubmit={handleSubmit} noValidate>
            <label htmlFor="admin-token" className="mb-2 block text-xs font-medium text-th-text-s">
              Admin token
            </label>
            <div className="relative">
              <input
                ref={inputRef}
                id="admin-token"
                name="token"
                type={visible ? "text" : "password"}
                value={token}
                onChange={(event) => setToken(event.target.value)}
                autoComplete="current-password"
                spellCheck={false}
                placeholder="Paste your token"
                aria-invalid={Boolean(error)}
                aria-describedby={error ? "auth-error" : undefined}
                className="input h-12 pr-12 font-mono text-sm tracking-wide"
              />
              <button
                type="button"
                onClick={() => setVisible((current) => !current)}
                className="absolute right-2 top-1/2 flex h-8 w-8 -translate-y-1/2 items-center justify-center rounded-md text-th-text-m transition-colors hover:bg-th-hover hover:text-th-text"
                title={visible ? "Hide token" : "Show token"}
                aria-label={visible ? "Hide token" : "Show token"}
              >
                <span aria-hidden="true" className="text-sm">{visible ? "◉" : "◌"}</span>
              </button>
            </div>

            {error && (
              <div id="auth-error" role="alert" className="mt-3 flex items-start gap-2 rounded-lg border border-red-500/20 bg-red-500/10 px-3 py-2.5 text-xs leading-5 text-red-500 dark:text-red-300">
                <span className="mt-0.5 shrink-0" aria-hidden="true">!</span>
                <span>{error}</span>
              </div>
            )}

            <button
              type="submit"
              disabled={loading || !token.trim()}
              className="btn-primary mt-6 flex h-11 w-full items-center justify-center gap-2"
            >
              {loading ? (
                <>
                  <span className="h-4 w-4 animate-spin rounded-full border-2 border-white/35 border-t-white" aria-hidden="true" />
                  Checking token
                </>
              ) : (
                <>
                  Continue
                  <span aria-hidden="true">→</span>
                </>
              )}
            </button>
          </form>

          <p className="mt-8 border-t border-th-border pt-4 text-[10px] leading-5 text-th-text-m">
            Your token is sent only to this Model Bridge instance.
          </p>
        </div>
      </section>
    </main>
  );
}
