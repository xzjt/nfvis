// 部署向导（Cisco VM Life Cycle > Deploy 的对应物；Web 控制台重构 2026-10）。
//
// 四步生成 VM/容器部署候选：
//   ① 类型（VM / 容器）→ ② 镜像与资源 → ③ 网络映射（含静态拓扑预览）→ ④ 摘要确认。
//
// 口径（与计划/spec 一致）：
//   · 只写候选不代提交——POST /virtual-machine-functions / POST /container-functions
//     都是 mutateCandidate 的纯候选写入（无 X-NFVIS-Auto-Commit 头），提交是「配置」页
//     或 CLI 的事；成功文案必须指向那里。
//   · 客户端只做提示级预校验（必填/数字/名称形状），权威校验在服务端（400 的 detail[]
//     由 api() 逐条并入错误文案，这里如实显示）。
//   · 拓扑预览是静态 SVG 示意（连线 = 网卡 → 交换机），不可拖拽——Cisco 向导的拖拽
//     交互在此降级为表单选择 + 只读预览。
//   · 「写入部署候选」经 uiConfirm 低危档（确认文案里写明「只写候选」）。

import { $, el, dash, fill, uiConfirm } from '../core/dom.js';
import { api, rowsOf, pageWarn, apiErrText } from '../core/api.js';
import { bytes } from '../core/format.js';

let depKind = '';        // 'vm' | 'ct'（Step 1 选定）
let depStep = 1;
let depData = null;      // { images, profiles, vss, pools, hp }（render 时从路由端点取齐）

function depMsg(text, isErr) {
  const m = $('dep-msg');
  m.textContent = text || '';
  m.classList.toggle('error', !!isErr);
}

function depShow(step) {
  depStep = step;
  for (let i = 1; i <= 4; i++) $('dep-step-' + i).hidden = i !== step;
  for (const b of $('dep-steps').querySelectorAll('button')) {
    const n = Number(b.dataset.step);
    b.style.opacity = n <= step ? '1' : '.5';
  }
  $('dep-step-note').textContent = '第 ' + step + ' / 4 步';
  if (step === 3) depDrawTopo();
  if (step === 4) depFillSummary();
}

function depRequireKind() {
  if (depKind) return true;
  depMsg('请先在第 ① 步选择部署类型。', true);
  return false;
}

function depFillImages() {
  const sel = $('dep-image');
  sel.textContent = '';
  for (const img of depData.images) {
    sel.appendChild(el('option', {
      value: img.name,
      text: img.name + (img.size_bytes ? '（' + bytes(img.size_bytes) + '）' : ''),
    }));
  }
  depUpdateImageNote();
}

function depUpdateImageNote() {
  const sel = $('dep-image');
  const prof = depData.profiles.find((p) => p.image === sel.value);
  $('dep-image-note').textContent = prof
    ? '镜像画像：machine=' + dash(prof.machine) + ' firmware=' + dash(prof.firmware) +
      ' console=' + dash(prof.console) +
      (prof.extra_args && prof.extra_args.length ? ' 等（详情见镜像页）' : '')
    : '该镜像没有启动画像（按缺省画像启动）。';
}

function depFillPools() {
  const lines = [];
  const hp = depData.hp;
  // 字段口径 = GET /system/hugepages 的池段（page_size/free；-1 = 取不到，如实显示）。
  if (hp && Array.isArray(hp.pools)) {
    for (const p of hp.pools) {
      const freeText = p.free != null && p.free >= 0 ? p.free + ' 页' : '取不到';
      lines.push(p.page_size + ' 池空闲页 ' + freeText + '（VNF 可起页数以此为准）');
    }
  }
  $('dep-pool-note').textContent = lines.length
    ? '资源池现状：' + lines.join('；') + '——起 VNF 前确认空闲量足够。'
    : '资源池读数取不到，起 VNF 前请到「系统 · 资源池」页核对。';
}

function depNicRows() {
  const rows = [];
  for (const tr of $('dep-nic-table').querySelectorAll('tbody tr')) {
    const nicIn = tr.querySelector('input');
    const sels = tr.querySelectorAll('select');
    if (!nicIn || sels.length < 2) continue;
    rows.push({ nic: nicIn.value.trim(), type: sels[0].value, vs: sels[1].value });
  }
  return rows.filter((r) => r.nic || r.vs);
}

function depCollect() {
  const name = $('dep-name').value.trim();
  const image = $('dep-image').value;
  const vcpu = Number($('dep-vcpu').value);
  const mem = Number($('dep-mem').value);
  const nics = depNicRows().map((r) => ({ name: r.nic, type: r.type, virtual_switch: r.vs }));
  if (depKind === 'vm') {
    const out = {
      name, image,
      vcpu: { count: vcpu },
      memory: { size_mb: mem },
      interfaces: nics,
      autostart: $('dep-autostart').checked,
    };
    if ($('dep-serial').checked) out.serial_console = true;
    if ($('dep-disk-mode').value === 'image') out.disks = [{ name: 'disk0', image }];
    else out.disks = [{ name: 'disk0', size_gb: Number($('dep-disk-size').value) || 8 }];
    return out;
  }
  const out = { name, image, interfaces: nics };
  if (vcpu >= 1) out.vcpu = vcpu;
  if (mem >= 128) out.memory_mb = mem;
  const cmd = $('dep-cmd').value.trim();
  if (cmd) out.command = cmd;
  const rp = $('dep-restart').value;
  if (rp) out.restart_policy = rp;
  return out;
}

// 提示级预校验：只拦明显缺漏，规则全集在服务端。
function depPrecheck(body) {
  const errs = [];
  if (!body.name) errs.push('名称必填');
  else if (!/^[a-zA-Z][a-zA-Z0-9_-]*$/.test(body.name)) {
    errs.push('名称以字母开头，只含字母/数字/-/_（与命令行一致）');
  }
  if (!body.image) errs.push('请选择镜像（「镜像库」页可导入）');
  if (depKind === 'vm') {
    if (!(body.vcpu.count >= 1)) errs.push('vCPU 至少 1');
    if (!(body.memory.size_mb >= 128)) errs.push('内存至少 128 MB');
  }
  return errs;
}

function depAddNicRow() {
  const tbody = $('dep-nic-table').querySelector('tbody');
  const tr = document.createElement('tr');
  const nic = el('input', { placeholder: 'eth0' });
  const type = el('select');
  const types = depKind === 'vm' ? ['vhost-user', 'sriov-vf'] : ['memif'];
  for (const t of types) type.appendChild(el('option', { value: t, text: t }));
  const vs = el('select');
  vs.appendChild(el('option', { value: '', text: '（未挂接）' }));
  for (const s of depData.vss) vs.appendChild(el('option', { value: s.name, text: s.name }));
  const del = el('button', { type: 'button', class: 'ghost small', text: '删' });
  del.addEventListener('click', () => tr.remove());
  for (const c of [nic, type, vs, del]) {
    const td = document.createElement('td');
    td.appendChild(c);
    tr.appendChild(td);
  }
  tbody.appendChild(tr);
}

// 静态拓扑预览：左侧新节点，右侧每个挂接交换机一个图标，连线 = 网卡 → 交换机。
function depDrawTopo() {
  const svg = $('dep-topo');
  svg.textContent = '';
  const NS = 'http://www.w3.org/2000/svg';
  const node = (tag, attrs) => {
    const n = document.createElementNS(NS, tag);
    for (const k in attrs) n.setAttribute(k, attrs[k]);
    return n;
  };
  const nics = depNicRows();
  svg.appendChild(node('rect', { x: 20, y: 60, width: 130, height: 60, rx: 8,
    fill: '#e6f7fd', stroke: '#049fd9' }));
  const label = node('text', { x: 85, y: 95, 'text-anchor': 'middle', fill: '#1a2b3c', 'font-size': 13 });
  label.textContent = ($('dep-name').value || '新部署') + '（' + (depKind === 'vm' ? 'VM' : '容器') + '）';
  svg.appendChild(label);
  const vsNames = [...new Set(nics.filter((n) => n.vs).map((n) => n.vs))];
  vsNames.forEach((vsName, i) => {
    const y = 25 + i * 45;
    svg.appendChild(node('rect', { x: 340, y, width: 160, height: 34, rx: 6,
      fill: '#f8fafc', stroke: '#d9dee5' }));
    const t = node('text', { x: 420, y: y + 22, 'text-anchor': 'middle', fill: '#1a2b3c', 'font-size': 12 });
    t.textContent = '虚拟交换机 ' + vsName;
    svg.appendChild(t);
    svg.appendChild(node('line', { x1: 150, y1: 90, x2: 340, y2: y + 17,
      stroke: '#049fd9', 'stroke-width': 1.5 }));
    const attached = nics.filter((n) => n.vs === vsName).map((n) => n.nic || 'nic');
    const edge = node('text', { x: 245, y: (90 + y + 17) / 2 - 4, fill: '#5f7183', 'font-size': 11 });
    edge.textContent = attached.join(', ');
    svg.appendChild(edge);
  });
  if (!vsNames.length) {
    const t = node('text', { x: 260, y: 95, 'text-anchor': 'middle', fill: '#5f7183', 'font-size': 12 });
    t.textContent = '（未挂接任何交换机——部署后无业务网络）';
    svg.appendChild(t);
  }
}

function depFillSummary() {
  const body = depCollect();
  const pairs = [
    ['类型', depKind === 'vm' ? '虚拟机（VM）' : '容器'],
    ['名称', body.name],
    ['镜像', body.image],
  ];
  if (depKind === 'vm') {
    pairs.push(
      ['vCPU', body.vcpu.count],
      ['内存 (MB)', body.memory.size_mb],
      ['磁盘', body.disks[0].image ? '从镜像 ' + body.disks[0].image + ' 克隆' : '空盘 ' + body.disks[0].size_gb + ' GB'],
      ['自动启动', body.autostart ? '是' : '否'],
      ['串口控制台', body.serial_console ? '启用' : '禁用']);
  } else {
    pairs.push(
      ['vCPU', body.vcpu != null ? body.vcpu : '（缺省）'],
      ['内存 (MB)', body.memory_mb != null ? body.memory_mb : '（缺省）'],
      ['命令', body.command || '（镜像缺省）'],
      ['重启策略', body.restart_policy || '（缺省）']);
  }
  pairs.push(['网卡', body.interfaces.length
    ? body.interfaces.map((n) => n.name + ' → ' + n.type + ' @ ' + (n.virtual_switch || '未挂接')).join('；')
    : '（无）']);
  fill($('dep-summary'), pairs);
}

async function depSubmit() {
  const body = depCollect();
  const errs = depPrecheck(body);
  if (errs.length) {
    depMsg('请先修正：' + errs.join('；'), true);
    return;
  }
  const what = depKind === 'vm' ? '虚拟机 ' + body.name : '容器 ' + body.name;
  if (!(await uiConfirm('部署 ' + what, {
    tier: 'low',
    paragraphs: ['把 ' + what + ' 的部署声明写入候选配置？本操作只写候选，不会立即创建实例' +
      '——请到「配置」页复核后提交生效（与命令行同一管线）。'],
    cli: (depKind === 'vm' ? 'set virtual-machine-functions ' : 'set container-functions ') + body.name + ' …',
  }))) return;
  try {
    const r = await api(depKind === 'vm' ? '/virtual-machine-functions' : '/container-functions', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const out = $('dep-result');
    out.hidden = false;
    out.textContent = '已写入部署候选（revision ' + (r && r.revision != null ? r.revision : '—') + '）。\n' +
      '下一步：到「配置」页复核并提交；提交后实例才会创建' +
      (depKind === 'vm' && body.autostart ? '并自动启动。' : '。');
    depMsg('部署候选已写入。', false);
  } catch (e) {
    depMsg('写入失败：' + apiErrText(e, '请按提示修正后重试；名称冲突时换一个名字。'), true);
  }
}

export const viewsDeploy = {
  'deploy': {
    async render(d) {
      pageWarn(d);
      depKind = '';
      depData = {
        images: rowsOf(d['/images']),
        profiles: rowsOf(d['/vm-image-profiles']),
        vss: rowsOf(d['/virtual-switches']),
        pools: d['/resource-pools'] || null,
        hp: d['/system/hugepages'] || null,
      };
      depFillImages();
      depFillPools();
      $('dep-nic-table').querySelector('tbody').textContent = '';
      $('dep-result').hidden = true;
      $('dep-vm-only').hidden = false;
      $('dep-ct-only').hidden = true;
      depMsg('', false);
      depShow(1);
    },
  },
};

// ---- 接线（模块求值期执行；type=module 在 DOM 解析后运行，元素必已存在）----
$('dep-type-vm').addEventListener('click', () => {
  depKind = 'vm';
  $('dep-vm-only').hidden = false;
  $('dep-ct-only').hidden = true;
  depMsg('已选：虚拟机。继续第 ② 步。', false);
  depShow(2);
});
$('dep-type-ct').addEventListener('click', () => {
  depKind = 'ct';
  $('dep-vm-only').hidden = true;
  $('dep-ct-only').hidden = false;
  depMsg('已选：容器。继续第 ② 步。', false);
  depShow(2);
});
$('dep-next-2').addEventListener('click', () => { if (depRequireKind()) depShow(3); });
$('dep-back-3').addEventListener('click', () => depShow(2));
$('dep-next-3').addEventListener('click', () => depShow(4));
$('dep-back-4').addEventListener('click', () => depShow(3));
$('dep-add-nic').addEventListener('click', () => depAddNicRow());
$('dep-image').addEventListener('change', depUpdateImageNote);
$('dep-submit').addEventListener('click', depSubmit);
