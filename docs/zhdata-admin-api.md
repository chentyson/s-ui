# zhdata 外置核心 × s-ui 面板 管理契约（P0）

版本：草案 v1（2026-11-20）· 适用 zhdata v0.15.x / s-ui v1.6.3-ZY4+
参与方：zhdata core（admin API 新增）、s-ui 后端（external 入站管理）、s-ui 前端（zhdata 表单/凭据）。

## 0. 总原则

- zhdata server 是**独立进程**（systemd 管理，s-ui 不 fork、不管理其生命周期）。
- s-ui 是**唯一权威数据源**（客户、凭据、限额都在 s-ui DB），通过 admin API 单向推送。
- 现有文件/sqlite users 热加载（txt/json/s-ui.db，20s mtime）**保留不动**；admin API 是叠加的第三个用户源。

## 1. zhdata core 侧：admin API

启动参数（CLI 或 config.json 字段同名）：

| 参数 | 说明 |
|---|---|
| `-admin 127.0.0.1:6070` | 管理面监听地址（空=关闭）。必须默认只绑 loopback |
| `-admin-token <token>` | 可选 Bearer token；设置后所有 /v1/* 必须带 `Authorization: Bearer <token>` |

JSON 端点（全部 `Content-Type: application/json`）：

### 1.1 `GET /v1/version`
```json
{"version": "0.15.1"}
```

### 1.2 `PUT /v1/users` — 全量替换 API 用户集
请求：
```json
{"users":[{"uuid":"abc...","max_devices":2}, {"uuid":"def...","max_devices":0}]}
```
语义：
- 替换的是 **"API 用户集"**；与文件/sqlite 来源 **取并集**（保持 c498 现有 users.txt 用法不破坏）。
- `max_devices`：0=不限；策略合并规则：**API 优先于文件**（覆盖同名 uuid）。
- 空数组 `{"users":[]}` = 清空 API 用户集（不影响文件源）。
- 返回 `{"ok":true,"keys":N}`；zhdata 在内存原子上换表，正在会话按现有 reject/kick 语义处理（被移除的 uuid 新连接拒绝；已有会话不强制断，除非另行 kick）。

### 1.3 `GET /v1/stats` — 按用户统计（累计自进程启动）
```json
{
  "ts": 1732100000,
  "sessions_active": 137,
  "users": [
    {"uuid":"abc...","devices":2,"up":102400,"down":988172}
  ]
}
```
- `devices` = 当前活跃 device_id 数（与 max_devices 同一计数）。
- `up/down` = 该 uuid 隧道字节累计（pumpUp/pumpDown 计数器）。s-ui 侧按周期采样求增量，语义与面板 SessionTracker flush 一致。

### 1.4 `POST /v1/users/kick` — 切断指定用户全部会话
```json
{"uuid":"abc..."}
```
返回 `{"kicked":N}`。uuid 为空字符串 = 踢所有 API/文件用户中被点名者（P0 只要求按 uuid 踢）。

### 1.5 `POST /v1/reload`（可选 P1）— 强制立即重载文件用户源。

## 2. s-ui 侧：zhdata 入站（external type）

面板新建入站类型 `zhdata`，Options（存 inbounds.options，与 zhdata server 配置字段同名，便于将来生成 `-c config.json`）：

```json
{
  "type": "zhdata",
  "tag": "zhdata-in",
  "listen": ":49888,:49889",        // 逗号分隔端口列表（zhdata server 语义）
  "backend": "direct",              // direct | socks 地址
  "max_sessions": 4800,
  "decoy": "",
  "admin_addr": "127.0.0.1:6070",   // 该入站对应的 zhdata 进程管理口
  "admin_token": ""
}
```

后端规则：
- `service` 层把 `zhdata` 记为 **external type**：不进 sing-box 全量 config（GetAllConfig 过滤），不走 corePtr.AddInbound；保存/编辑/删除时改为 admin API 推送 + 立即刷新用户集。
- `hasUser("zhdata")=true`：用户凭据继续取 `clients.config.$.zhdata`（对象形状见 §3）。
- users 推送触发点 = 现有 addUsers/UpdateInboundsUsers/client enable-disable 全部路径：把面板全部启用客户（uuid + max_devices=面板设备数限额）`PUT /v1/users`。
- 统计：新 cron（15s）`GET /v1/stats` → 按 uuid 增量累加进 clients.up/down（与 stats.go flush 合并语义）→ DepleteClients 超额自动 disable → 用户从 zhdata users 推送中消失。
- Kick：面板 Kick 用户时并列调 `POST /v1/users/kick`。

## 3. 客户凭据形状（clients.config.zhdata）

```json
{"name": "alice", "uuid": "<uuid>"}
```
- migration（对齐 1_6_3.go 写法）：存量客户复制其 vless uuid（没有则新造）填入 `config.zhdata.uuid`。
- 前端 randomConfigs 生成同样形状；Client.vue 里 zhdata 块显示/刷新 uuid。
- 链接：`zhdata://<uuid>@<host>:<port>[,<host2>:<port2>]#<remark>`（已在核心接口文档 §3.2 定案）。

## 4. 面板内全局总设备数（与 zhdata 无关的独立改进）

- 现状：usersession.Registry 每入站一份，devlimit 按入站各自生效；GetDeviceCounts 仅展示。
- 目标：TryBind 的计数改为**跨入站共享**（全局 user→活跃设备 计数；source key 加 inbound tag 前缀防撞）。所有 sui 内嵌入站（vless/hysteria2/trojan/anytls/ss/...）合计不超过客户设备数限额。
- zhdata 的 devices 由 zhdata core 自己按 uuid 限（面板推送 max_devices）；跨面板+zhdata 的全局设备数精确合并留 P1（租约式）。

## 5. 验收（c498 联测）

1. systemd 起 zhdata-tunnel（`-admin 127.0.0.1:6070`），面板建 zhdata 入站（端口 49888-49951、backend direct）。
2. 面板加客户 → 20s 内 zhdata 日志 users 生效；zhenhanvn 用 `zhdata://` 链接连上。
3. 该客户再订一个 hysteria2 入站：面板设备数限额 2 → 两协议合计同时只允许 2 台（面板内嵌协议精确；zhdata 侧按推送 max_devices 独立限）。
4. 面板流量列 = hysteria2 流量 + zhdata 流量之和；15s 刷新；超额 DepleteClients 后 zhdata 新连接被拒。
5. 面板 Kick → zhdata 会话被切（日志出现 reject）。
