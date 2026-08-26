/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{js,ts,jsx,tsx}"],
  theme: {
    extend: {
      colors: {
        th: {
          bg:       'var(--color-bg)',
          base:     'var(--color-bg-base)',
          hover:    'var(--color-bg-hover)',
          elev:     'var(--color-bg-elevated)',
          border:   'var(--color-border)',
          text:     'var(--color-text)',
          'text-s': 'var(--color-text-sec)',
          'text-m': 'var(--color-text-muted)',
          accent:   'var(--color-accent)',
          'accent-d':'var(--color-accent-dim)',
          'accent-g':'var(--color-accent-glow)',
        },
      },
      fontFamily: {
        display: ['Outfit', 'system-ui', '-apple-system', '"Segoe UI"', 'Roboto', '"PingFang SC"', '"Microsoft YaHei"', 'sans-serif'],
        body: ['Inter', 'system-ui', '-apple-system', '"Segoe UI"', 'Roboto', '"PingFang SC"', '"Microsoft YaHei"', 'sans-serif'],
        mono: ['ui-monospace', 'SFMono-Regular', '"SF Mono"', 'Menlo', 'Consolas', '"Liberation Mono"', 'monospace'],
      },
      animation: {
        "fade-in": "fadeIn 0.4s ease-out",
        "fade-in-up": "fadeInUp 0.5s cubic-bezier(0.16, 1, 0.3, 1) forwards",
        "slide-up": "slideUp 0.35s ease-out",
      },
      keyframes: {
        fadeIn: { "0%": { opacity: "0" }, "100%": { opacity: "1" } },
        slideUp: { "0%": { opacity: "0", transform: "translateY(12px)" }, "100%": { opacity: "1", transform: "translateY(0)" } },
        fadeInUp: { 
          "0%": { opacity: "0", transform: "translateY(16px)" }, 
          "100%": { opacity: "1", transform: "translateY(0)" } 
        },
      },
    },
  },
  plugins: [],
};
