// NFViS Web 控制台 —— VNC 图形查看器绑定（决策 #485）。
//
// 本模块是**唯一** import noVNC 的地方：noVNC 1.6.0（MPL-2.0）已随包 vendored 到
// `novnc/core`（+ `novnc/vendor/pako`），此处 import 其 RFB 实现，暴露一个全局
// `window.nfvisVnc` 供 app.js（经典风格模块）调用。
//
// 数据通路：HTML5 canvas ← RFB ← WebSocket(`/api/v1/virtual-machine-functions/<name>/console/vnc/ws?ticket=…`)
// ← nfvisd WS↔VNC-TCP 代理 ← libvirt 域 VNC 服务（127.0.0.1:<port>，仅回环）。
// ticket 一次性、短时有效（与串口 console 同一套，资源键隔离：vnc/ ≠ vm/）。

import RFB from './novnc/core/rfb.js';

let current = null;

const nfvisVnc = {
  // open(wsUrl, el, handlers) 在 el 里打开到 wsUrl 的 VNC 会话。
  // handlers: { onConnect(), onDisconnect(detail), onError(err) }（均可省略）。
  open(wsUrl, el, handlers) {
    nfvisVnc.close();
    const h = handlers || {};
    if (!el) {
      if (h.onError) h.onError(new Error('未找到 VNC 显示容器'));
      return;
    }
    let rfb;
    try {
      // wsProtocols 缺省为空（与串口 console 的 new WebSocket(url) 一致，服务端无需子协议协商）。
      rfb = new RFB(el, wsUrl);
    } catch (e) {
      if (h.onError) h.onError(e);
      return;
    }
    current = rfb;
    // 自适应容器大小：随面板缩放，不反向改 guest 分辨率。
    rfb.scaleViewport = true;
    rfb.resizeSession = false;
    rfb.addEventListener('connect', () => { if (h.onConnect) h.onConnect(); });
    rfb.addEventListener('securityfailure', (e) => {
      if (h.onError) h.onError(new Error('VNC 认证失败' + (e && e.detail ? '：' + e.detail.reason : '')));
    });
    rfb.addEventListener('disconnect', (e) => {
      if (current === rfb) current = null;
      if (h.onDisconnect) h.onDisconnect(e && e.detail ? e.detail : {});
    });
  },

  // close() 断开当前会话（幂等）。
  close() {
    if (!current) return;
    const rfb = current;
    current = null;
    try { rfb.disconnect(); } catch (e) { /* 已断开即忽略 */ }
  },

  isOpen() { return !!current; },
};

window.nfvisVnc = nfvisVnc;
