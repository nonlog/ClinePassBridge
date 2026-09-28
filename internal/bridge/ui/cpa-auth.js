/* Compatibility with the official management center's cli-proxy-auth storage format.
 * Reads only the saved login for this exact CPA endpoint; never creates another stored key.
 */
(() => {
  'use strict';
  function readSavedLogin() {
    try {
      if (localStorage.getItem('isLoggedIn') !== 'true') return null;
      let value = localStorage.getItem('cli-proxy-auth');
      if (!value) return null;
      let prefix = '';
      let seed = '';
      if (value.startsWith('enc::v2::')) {
        prefix = 'enc::v2::';
        seed = 'cli-proxy-api-webui::secure-storage|v2|' + location.host;
      } else if (value.startsWith('enc::v1::')) {
        prefix = 'enc::v1::';
        seed = 'cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent;
      }
      if (prefix) {
        const bytes = Uint8Array.from(atob(value.slice(prefix.length)), c => c.charCodeAt(0));
        const mask = new TextEncoder().encode(seed);
        value = new TextDecoder().decode(bytes.map((byte, i) => byte ^ mask[i % mask.length]));
      }
      const saved = JSON.parse(value)?.state;
      if (!saved || saved.rememberPassword !== true || typeof saved.managementKey !== 'string' || !saved.managementKey.trim()) return null;
      const server = new URL(saved.apiBase);
      const own = new URL(document.querySelector('meta[name="cpa-api-base"]').content, location.origin);
      const cleanPath = path => path.replace(/\/v0\/management\/?$/, '').replace(/\/+$/, '');
      const ownRoot = own.pathname.replace(/\/v0\/management\/clinepassbridge\/?$/, '');
      if (server.username || server.password || server.search || server.hash || server.origin !== own.origin || cleanPath(server.pathname) !== ownRoot) return null;
      return { key: saved.managementKey.trim(), base: server.href };
    } catch { return null; }
  }
  window.PassBridgeAuthHeaders = () => {
    const login = readSavedLogin();
    return login ? { Authorization: `Bearer ${login.key}` } : {};
  };
})();
