// 从 app.js 拆出（Web 控制台重构 2026-10）；逻辑逐字保留，仅 import/export 接线变化。
// 镜像域：列表 / 详情 / 画像 / 导入 / 删除。

import { $, el, dash, wbtn, fill, table, list, uiDialog, closeDialog, uiConfirm,
  CONFIRM_COUNTDOWN, rowButton, rowClickable, goDetail } from '../core/dom.js';
import { API, TOKEN_KEY, USER_KEY, MAX_IFACE_DETAIL } from '../core/const.js';
import { api, soft, apiErrText, httpErrText,
  errTextOf, rowsOf, pageWarn, softLoad, expandEndpoint, loadVSwitchStats,
  loadNetworkObjects, loadInterfaceStats } from '../core/api.js';
import { uptime, bytes, pct, mb, fmtTime, fmtClockHint, fmtTimeOpt, fmtBytes,
  portSecText, stormControlText } from '../core/format.js';
import { getToken, setToken, getUser, setUser, getCurrentClass, setCurrentClass,
  getEvents, setEvents, pushEvent, renderEvents, setStream, startStream, stopStream,
  armPolling, setPollRoute, startPolling, stopPolling, scheduleReload,
  showGlobalError } from '../core/state.js';

// reload（重渲染当前路由）与 goDetail 类似，按需动态 import 防静态环。
const jsviewsimagesRouterModule = () => import('../../router.js');
async function reload() {
  return (await jsviewsimagesRouterModule()).render();
}

// ---------- 镜像仓库 ----------

function renderImages(imgs) {
  const tbody = $('img-table').querySelector('tbody');
  tbody.textContent = '';
  const rows = imgs || [];
  if (!rows.length) {
    const tr = el('tr');
    tr.appendChild(el('td', { colspan: '6', class: 'muted', text: '（无）' }));
    tbody.appendChild(tr);
    return;
  }
  rows.forEach((i) => {
    const tr = rowClickable(el('tr'), '#/compute/images/' + encodeURIComponent(i.name));
    [i.name, i.type, i.size_bytes ? bytes(i.size_bytes) : undefined, i.ref_count,
      i.import_state || 'ready'].forEach((c) => {
      tr.appendChild(el('td', { text: String(dash(c)) }));
    });
    const cell = el('td', { class: 'actions' });
    const detBtn = rowButton(el('button', { type: 'button', class: 'ghost small', text: '详情' }));
    detBtn.addEventListener('click', () => goDetail('#/compute/images/' + encodeURIComponent(i.name)));
    cell.appendChild(detBtn);
    // 删除镜像 = super-user 级写入口（DELETE /images/{name}）——**必须经 wbtn 带 data-write**，
    // 否则 read-only/operator 仍会看到它（决策 #327 修 R105-1：动态写入口漏打门禁标记）。
    const btn = rowButton(wbtn({ type: 'button', class: 'danger small', text: '删除' }));
    btn.addEventListener('click', () => imgDelete(i.name, i.ref_count));
    cell.appendChild(btn);
    tr.appendChild(cell);
    tbody.appendChild(tr);
  });
}

// 镜像详情页（#/compute/images/:name）：完整元数据（元数据是静态的，本页不轮询）。
// 启动画像（决策 #485）取自 /vm-image-profiles 列表里该镜像的条目（键＝镜像名）——与 CLI
// `show vm-image-profiles` 同源（同一份 committed 配置）；未配置时如实说明按产品默认启动。
function renderImageDetail(img, profiles, params) {
  const name = (params && params.name) || '';
  $('imd-name').textContent = name;
  const ok = img && !img.__err;
  fill($('imd-head'), ok ? [
    ['类型', img.type],
    ['大小', img.size_bytes ? bytes(img.size_bytes) : undefined],
    ['引用数', img.ref_count],
  ] : [['读取失败', img ? img.__err : '未取到数据']]);
  fill($('imd-info'), ok ? [
    ['名称', img.name],
    ['描述', img.description],
    ['类型', img.type],
    ['格式', img.format],
    ['大小', img.size_bytes ? bytes(img.size_bytes) + '（' + img.size_bytes + ' 字节）' : undefined],
    ['sha256', img.sha256],
    ['引用计数', img.ref_count],
    ['导入状态', img.import_state || 'ready'],
    ['导入时间', img.imported_at ? fmtTime(img.imported_at) : undefined],
    // 容器镜像归档内嵌的 tag：Docker 侧引用按目录项名推导（含冒号原样、否则 `<名>:latest`；
    // 与内嵌 tag 不一致时导入会自动重打），故配置里唯一可用名就是仓库里的名字；
    // 这里如实回显来源 tag，便于核对（重命名不再静默）。
    ['归档内嵌 tag', (img.source_tags && img.source_tags.length) ? img.source_tags.join('、') : undefined],
  ] : []);
  renderImageProfile(Array.isArray(profiles) ? profiles : [], name);
}

// renderImageProfile 填镜像详情页的「启动画像」块：列表里按 image 名取该镜像的画像。
// 只显示**配置值**（未配置的项由 fill 渲染成「—」）；合并后的有效值与来源在虚拟机详情页。
function renderImageProfile(profiles, name) {
  const p = profiles.find((x) => x && x.image === name);
  if (!p) {
    fill($('imd-profile'), [['状态', '未配置启动画像（按产品默认）']]);
    return;
  }
  const cpu = p.cpu_model ? ((p.cpu_mode ? p.cpu_mode + '/' : '') + p.cpu_model) : p.cpu_mode;
  fill($('imd-profile'), [
    ['machine', p.machine],
    ['cpu', cpu],
    ['firmware', p.firmware],
    ['disk-interface', p.disk_interface],
    ['boot-order', p.boot_order],
    ['accel', p.accel],
    ['console', p.console],
    // extra-args 是窄自由口，**已绕过资源账本**（契约 §4.4）：与 CLI 读视图同一标注（决策 #485）。
    ['extra-args', (p.extra_args && p.extra_args.length) ? p.extra_args.join(' ') + '（已绕过资源账本）' : undefined],
  ]);
}

// imgIsFailed / imgOutcome：**不把 2xx 当成功**——服务端可能已受理但导入失败
// （记录会以 import_state=failed 落库，界面必须如实呈现，否则就是"假绿"）。
function imgIsFailed(res) {
  const st = res && res.import_state;
  return st === 'failed';
}

function imgOutcome(res, name) {
  const st = (res && res.import_state) || '';
  if (st === 'failed') {
    const why = (res && (res.error || res.message)) || '服务端未给出原因';
    return '导入失败（' + name + '）：' + why;
  }
  // 容器镜像导入后：显式告诉操作者 Docker 侧引用与配置里该写哪个名字（不静默改名）。
  // 引用推导与服务端同一规则：含冒号原样、否则 `<名>:latest`。
  const tags = (res && res.source_tags) || [];
  const ref = name.indexOf(':') >= 0 ? name : name + ':latest';
  const tagNote = (res && res.type === 'container-image')
    ? '；Docker 侧引用为 ' + ref +
      (tags.length ? '（归档内嵌 tag：' + tags.join('、') + '，不一致时已自动重打）' : '（归档未内嵌 tag）') +
      '，配置中请用 ' + name + ' 引用'
    : '';
  if (st === 'ready' || st === 'imported') return '导入完成：' + name + tagNote;
  if (st) return '已受理（' + name + '）：状态 ' + st + '，可在列表中查看';
  return '已受理：' + name + '（可在列表中查看导入状态）';
}

function imgMsg(text, isErr) {
  const p = $('img-msg');
  p.hidden = !text;
  p.textContent = text || '';
  p.className = isErr ? 'error small' : 'muted small';
}

// 删除镜像：中危（删仓库对象，且可能正被虚拟机/容器引用）。
async function imgDelete(name, refCount) {
  const ok = await uiConfirm('删除镜像', {
    tier: 'mid',
    paragraphs: ['从镜像仓库删除 ' + name + '。'],
    bullets: [
      refCount
        ? '该镜像当前被引用 ' + refCount + ' 次：服务端会拒绝删除（先把引用它的虚拟机/容器改掉或删掉）。'
        : '当前没有对象引用它，服务端会真的删掉仓库里的文件。',
      '删除不可恢复：镜像文件从仓库移除后只能重新导入（本地文件不在仓库里）。',
      '已经用该镜像建好的虚拟机磁盘不受影响（它们是克隆出来的），但重新创建/重建时就不能再用这个名字。',
    ],
    cli: 'request images delete name ' + name,
  });
  if (!ok) return;
  imgMsg('删除 ' + name + '：执行中…', false);
  try {
    await api('/images/' + encodeURIComponent(name), { method: 'DELETE' });
    imgMsg('已删除 ' + name + '。', false);
  } catch (e) {
    imgMsg('删除失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

function imgCommon() {
  const name = $('img-name').value.trim();
  const type = $('img-type').value.trim() || 'vm-image';
  if (!name) { imgMsg('请填写名称。', true); return null; }
  return { name, type };
}

async function imgImportURL() {
  const c = imgCommon();
  if (!c) return;
  const url = $('img-url').value.trim();
  const sha = $('img-sha').value.trim();
  if (!url) { imgMsg('请填写 URL。', true); return; }
  if (!sha) { imgMsg('URL 拉取必须提供 sha256（服务端默认强制校验）。', true); return; }
  const ok = await uiConfirm('拉取并导入镜像', {
    tier: 'low',
    paragraphs: ['从 ' + url + ' 拉取并导入为 ' + c.name + '？' +
      '只往仓库里新增一份镜像，不改现有对象；大镜像可能耗时较久（期间请勿关闭页面）。'],
    cli: 'request images download name ' + c.name + ' type ' + c.type + ' url ' + url + ' sha256 ' + sha,
  });
  if (!ok) return;
  imgMsg('拉取中…（大镜像可能耗时较久，请勿关闭页面）', false);
  try {
    const res = await api('/images', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: c.name, type: c.type, url, sha256: sha }),
    });
    imgMsg(imgOutcome(res, c.name), imgIsFailed(res));
  } catch (e) {
    imgMsg('导入失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

async function imgImportIncoming() {
  const c = imgCommon();
  if (!c) return;
  const file = $('img-incoming').value.trim();
  if (!file) { imgMsg('请填写 incoming 文件名。', true); return; }
  const ok = await uiConfirm('从 incoming 导入镜像', {
    tier: 'low',
    paragraphs: ['把 incoming 目录里的 ' + file + ' 导入为 ' + c.name + '？' +
      '只往仓库里新增一份镜像；导入成功后该源文件会被清理。'],
    cli: 'request images upload name ' + c.name + ' type ' + c.type + ' file ' + file,
  });
  if (!ok) return;
  imgMsg('导入中…', false);
  try {
    const res = await api('/images', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name: c.name, type: c.type, incoming_file: file }),
    });
    imgMsg(imgOutcome(res, c.name), imgIsFailed(res));
  } catch (e) {
    imgMsg('导入失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

async function imgImportFile() {
  const c = imgCommon();
  if (!c) return;
  const f = $('img-file').files[0];
  if (!f) { imgMsg('请选择文件。', true); return; }
  const ok = await uiConfirm('上传并导入镜像', {
    tier: 'low',
    paragraphs: ['上传 ' + f.name + '（' + bytes(f.size) + '）并导入为 ' + c.name + '？' +
      '只往仓库里新增一份镜像，不改现有对象；上传期间请勿关闭页面。'],
    cli: 'request images upload name ' + c.name + ' type ' + c.type + ' file ' + f.name,
  });
  if (!ok) return;
  const fd = new FormData();
  fd.append('name', c.name);
  fd.append('type', c.type);
  fd.append('file', f);
  imgMsg('上传中…（大镜像可能耗时较久，请勿关闭页面）', false);
  try {
    const res = await fetch(API + '/images', { method: 'POST', headers: { Authorization: 'Bearer ' + getToken() }, body: fd });
    if (!res.ok) {
      imgMsg('导入失败：' + await httpErrText(res), true);
    } else {
      const body = await res.json().catch(() => ({}));
      imgMsg(imgOutcome(body, c.name), imgIsFailed(body));
    }
  } catch (e) {
    imgMsg('导入失败：' + e.message, true);
  }
  await reload().catch(() => {});
}

// 各 view 的渲染注册（从 app.js 的 VIEWS 注册表按域拆出；键名=routes.json 的 view 名）。

export const viewsImages = {
  'images': {
    render(d) { pageWarn(d); renderImages(rowsOf(d['/images'])); }
  },
  'imageDetail': {
    render(d, params) { pageWarn(d); renderImageDetail(d['/images/{name}'], d['/vm-image-profiles'], params); }
  },
};
