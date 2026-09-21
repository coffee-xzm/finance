import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

/**
 * 侧边栏插件是**静态前端**：构建产物要能被飞书 webview 直接加载。
 *
 * 因此：
 *   - `base: './'` —— 用相对路径引用资源，放到任何子路径下都能跑（Replit/静态托管都适用）。
 *   - `target: 'es2020'` —— 飞书桌面端/移动端内嵌 webview 的兼容面。
 *   - 不开 devtools 专用的 server.host 之类，保持默认即可。
 */
export default defineConfig({
  base: './',
  plugins: [react()],
  build: {
    target: 'es2020',
    outDir: 'dist',
    emptyOutDir: true,
    // 生产包不带 sourcemap：插件体积越小加载越快（需要排查时用 `npm run build -- --sourcemap`）
    sourcemap: false,
    rollupOptions: {
      output: {
        // 把官方 SDK 单独切出来：它占了绝大部分体积，单独缓存对我们没用但便于排查
        manualChunks: (id: string) =>
          id.includes('@lark-base-open/js-sdk') ? 'lark-sdk' : undefined,
      },
    },
    chunkSizeWarningLimit: 1200,
  },
  server: {
    port: 5173,
    strictPort: true,
  },
});
