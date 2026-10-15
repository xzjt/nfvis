// 部署向导（Cisco VM Life Cycle > Deploy 的对应物）：分步表单在 Task 6 落地，
// 先占位保住路由↔视图映射（ui_routes_test 守护 #/deploy 的 view 必须在 VIEWS 里）。

import { pageWarn } from '../core/api.js';

export const viewsDeploy = {
  'deploy': {
    render(d) { pageWarn(d); },
  },
};
