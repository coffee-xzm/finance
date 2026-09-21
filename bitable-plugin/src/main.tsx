import React from 'react';
import { createRoot } from 'react-dom/client';
import App from './App.js';

const el = document.getElementById('root');
if (!el) throw new Error('缺少 #root 挂载点');

createRoot(el).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
