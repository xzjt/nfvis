// NFViS 控制台前端（聚合入口；Web 控制台重构 2026-10）。
//
// 无外部依赖、无构建步骤：改完直接刷新页面即可，产物随二进制内嵌。
// 取数一律走同源 REST 接口并带 Bearer token。
//
// 三件事各归其位：路由表是数据（routes.json，与 Go 守护共用同一份真源）、页面是容器
// （index.html 里每个路由一个 .page 段）、视图是代码（本文件的 VIEWS 注册表，按域拆在
// js/views/ 各文件里，这里聚合）。共享件在 js/core/，启动接线在 js/boot.js。
//
// 数据来源只用**对外**的 REST 端点（/cli/execute 是 x-internal，前端不碰）。

import { softLoad } from './js/core/api.js';
import { setPollRoute, showGlobalError } from './js/core/state.js';
import { pageWarn } from './js/core/api.js';

import { viewsOverview } from './js/views/overview.js';
import { viewsSystem } from './js/views/system.js';
import { viewsConfig } from './js/views/config.js';
import { viewsVms } from './js/views/vms.js';
import { viewsVmDetail } from './js/views/vmDetail.js';
import { viewsContainers } from './js/views/containers.js';
import { viewsContainerDetail } from './js/views/containerDetail.js';
import { viewsImages } from './js/views/images.js';
import { viewsNetwork } from './js/views/network.js';
import { viewsSwitches } from './js/views/switches.js';
import { viewsLldp } from './js/views/lldp.js';
import { viewsOps } from './js/views/ops.js';
import { viewsMonitor } from './js/views/monitor.js';
import { viewsDeploy } from './js/views/deploy.js';

import { boot, reload } from './js/boot.js';

export const VIEWS = {
  ...viewsDeploy,
  ...viewsOverview,
  ...viewsSystem,
  ...viewsConfig,
  ...viewsVms,
  ...viewsVmDetail,
  ...viewsContainers,
  ...viewsContainerDetail,
  ...viewsImages,
  ...viewsNetwork,
  ...viewsSwitches,
  ...viewsLldp,
  ...viewsOps,
  ...viewsMonitor,
};
// router.js 消费的取数助手由 core 提供（re-export 保持既有 import 面）。
import { el } from './js/core/dom.js';
export { softLoad, setPollRoute, showGlobalError, reload, el };

boot();
