import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import esbuild from 'esbuild';

const root = path.dirname(fileURLToPath(import.meta.url));
const distAssetsDir = path.join(root, 'assets', 'dist');
const distHtmlDir = path.join(root, 'dist');

fs.mkdirSync(distAssetsDir, { recursive: true });
fs.mkdirSync(distHtmlDir, { recursive: true });

await esbuild.build({
  entryPoints: [path.join(root, 'src', 'main.jsx')],
  bundle: true,
  format: 'iife',
  platform: 'browser',
  target: ['es2020'],
  jsx: 'automatic',
  minify: true,
  sourcemap: false,
  outdir: distAssetsDir,
  entryNames: 'app',
  external: ['../fonts/*'],
  define: {
    'process.env.NODE_ENV': '"production"',
  },
  logLevel: 'info',
});

const html = `<!doctype html>
<html lang="en" data-theme="dark" data-bg="matrix">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Fleet panel</title>
  <meta name="color-scheme" content="dark light">
  <link rel="icon" type="image/svg+xml" href="/assets/favicon.svg">
  <link rel="icon" type="image/png" sizes="32x32" href="/assets/favicon-32x32.png">
  <link rel="apple-touch-icon" sizes="180x180" href="/assets/apple-touch-icon.png">
  <script>
  (function () {
    try {
      var t = localStorage.getItem('fleet-theme');
      document.documentElement.setAttribute('data-theme', t === 'light' || t === 'dark' ? t : 'dark');
    } catch (e) {
      document.documentElement.setAttribute('data-theme', 'dark');
    }
  })();
  </script>
  <link rel="stylesheet" href="/assets/dist/app.css">
  <script src="/assets/dist/app.js" defer></script>
</head>
<body>
  <div id="root"></div>
</body>
</html>
`;

fs.writeFileSync(path.join(distHtmlDir, 'index.html'), html, 'utf8');
