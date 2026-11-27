import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import '../assets/css/fonts.css';
import '../assets/css/tokens.css';
import '../assets/css/base.css';
import '../assets/css/components.css';
import '../assets/css/bg.css';
import '../assets/css/live.css';

import { App } from './App';

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <App />
  </StrictMode>
);
