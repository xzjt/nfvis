// NFViS 控制台共享件 · 常量（无依赖的最底层模块；Web 控制台重构 2026-10）。
//
// 单独成件的原因：state.js（会话状态）与 api.js（取数）互相需要——api 要 getToken()、
// state 要 API/TOKEN_KEY。ES 模块循环引用在浏览器下可行、但在桩式自校准的「按依赖序拼接
// 为经典脚本」加载器里会形成 TDZ（顶层 let/const 在被引用段求值前不可用）。把两侧共享的
// 常量下沉到这个零依赖模块，环就断了。

export const API = location.pathname.replace(/\/ui\/.*$/, '') || '/api/v1';
export const TOKEN_KEY = 'nfvis.token';
export const USER_KEY = 'nfvis.user';
export const MAX_IFACE_DETAIL = 12;
