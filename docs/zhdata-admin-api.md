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
{"users":[{"uuid":"abc...","max_devices":2,"up_kbps":2000,"down_kbps":2000}, {"uuid":"def...","max_devices":0}]}
```
语义：
- 替换的是 **"API 用户集"**；与文件/sqlite 来源 **取并集**（保持 c498 现有 users.txt 用法不破坏）。
- `max_devices`：0=不限；策略合并规则：**API 优先于文件**（覆盖同名 uuid）。
- `up_kbps`/`down_kbps`（**zhdata ≥0.15.6 增量字段**）：该用户上下行带宽上限，单位 kbit/s（1000 进制）。面板取值 = `bwlimit.Group(clients."group")`，也就是 hy2/vless/trojan 入站已经在用的那套「组即带宽」语义（空组=不限；读不出带宽的组=DefaultKbit=1000，不给白名单）。单值写法 `speed_limit_kbps` 等价于两向同值（核心也支持）。0=不限，且**与 `max_devices` 同样的整体覆盖语义**：条目不带 ⇒ 抹掉文件/sqlite 源里的限速（所以面板每次都全量带值）。限速在核心按 **uuid 聚合**（跨该用户所有隧道会话与流），zhdata 客户端 `-conns 32` 不会变成 32 倍额度；改值当场生效，不用重连或重启进程。详见核心接口文档 §3.4.1。
- `expiry/quota/used`（**zhdata ≥0.15.4 增量字段**，面板取自 clients.expiry / volume / up+down）：核心把它作为该 uuid 的套餐记录，用于隧道内套餐查询（客户端在隧道里请求 `query.zhdata`）。三个字段**全 0 = 面板没有该信息**，此时核心**保留**文件/sqlite 源里的同名记录，不抹掉。
- 空数组 `{"users":[]}` = 清空 API 用户集（不影响文件源）。
- 返回 `{"ok":true,"keys":N}`；zhdata 在内存原子上换表，正在会话按现有 reject/kick 语义处理（被移除的 uuid 新连接拒绝；已有会话不强制断，除非另行 kick）。

### 1.3 `GET /v1/stats` — 按用户统计（累计自进程启动）
```json
{
  "ts": 1732100000,
  "sessions_active": 137,
  "keys_total": 3,
  "user_sources": [
    {"path":"/opt/zhdata/users.txt","keys":1,"last_error":"","at":1795350000},
    {"path":"/etc/s-ui/private/s-ui.db","keys":0,"last_error":"no such file or directory","at":1795350000}
  ],
  "users": [
    {"uuid":"abc...","devices":2,"up":102400,"down":988172,"up_kbps":2000,"down_kbps":2000}
  ]
}
```
- `devices` = 当前活跃 device_id 数（与 max_devices 同一计数）。
- `up/down` = 该 uuid 隧道字节累计（pumpUp/pumpDown 计数器）。s-ui 侧按周期采样求增量，语义与面板 SessionTracker flush 一致。
- `up_kbps`/`down_kbps`（**zhdata ≥0.15.6 增量字段**）= 核心当前对该用户生效的带宽上限（kbit/s；0 或字段缺失=不限）。这是面板核对「我填的 2M 到底传没传到核心」的只读回显，不必再靠测速反推；限速的计费点与这里的 `up/down` 是同一处。
- `keys_total` / `user_sources`（**zhdata ≥0.15.4 增量字段**，老核心不返回）：有效 key 集大小；以及 `-users` 每个源「最后一次成功加载贡献几个 key」「最后一次尝试的时间与错误」。这是把「用户库路径写错」这类只进 journald 的告警变成面板可见的唯一途径（面板 §2.5）。

### 1.4 `POST /v1/users/kick` — 切断指定用户全部会话
```json
{"uuid":"abc..."}
```
返回 `{"kicked":N}`。uuid 为空字符串 = 踢所有 API/文件用户中被点名者（P0 只要求按 uuid 踢）。

### 1.5 `POST /v1/reload`（可选 P1）— 强制立即重载文件用户源。
返回 `{"ok":true,"keys":N,"user_sources":[…同 §1.3…]}`（`user_sources` 为 zhdata ≥0.15.4 增量字段），运维一行 curl 就能核对用户库配置。

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
- 套餐字段：users 推送除 uuid+max_devices 外带上 `expiry=clients.expiry`、`quota=clients.volume`、`used=clients.up+down`（zhdata ≥0.15.4 用它们在隧道内回答套餐查询，见核心接口文档 §3.8）。因此 `resetTraffic`（清零 up/down、重新启用客户）也要补一次推送，否则核心最多滞后 1 分钟。
- 离开集合自动 kick：面板对比前后两次推送的 uuid 集，对被删除/禁用/换凭据的客户自动 `POST /v1/users/kick`（核心只对**新**连接做 key miss，不断已有隧道）。首次推送（面板刚启动）不踢任何人；入站删除时清掉该 tag 的前后快照。
- **组即限速也要推到 zhdata**（**s-ui v1.6.3-ZY9+ / zhdata ≥0.15.6**）：`clients."group"` 读出的带宽（`bwlimit.Group`，与 hy2/vless/trojan 入站同一个函数）作为 `up_kbps`/`down_kbps` 随同推送。此前 ZY2 的限速只挂在 sing-box 入站的 `bwlimit.Wrap` 上，zhdata 隧道由独立进程（`zhdata.service`/`zhdata-relay.service`）终结，面板填的组对 zhdata 用户完全无效（客户测速不降）。推送点与触发时机不变（保存客户/入站、enable/disable、resetTraffic 都推），所以改组名/改限速当场生效，无需重启核心。relay 与终点进程都会收到同一份表：串联的令牌桶**不叠加**，稳态速率仍是那个值。旧核心忽略这两个字段（不影响其它字段），旧面板不带 ⇒ 该用户不限速。

### 2.4 新节点部署（**v1.6.3-ZY11 起面板同时负责核心本体与 unit**）

ZY10 之前 `service/` 里没有任何 `exec.Command`：面板只做记账 + admin 推送 + 健康探测，进程由运维装 systemd。
代价在 C455 现形：界面上把入站端口改成 45501-45564 之后，`zhdata.service` 的 `-listen` 还停在 49888-49919，
455xx 上根本没有 TCP 监听 ⇒ 客户端「无法连接」而入站卡片还是绿的。
ZY10 把 unit 交给了面板，但核心二进制仍在面板之外（运维手工 `install -m 755 … /opt/zhdata/…`），
于是节点会悄悄跑着旧核心而界面毫无察觉，新装的面板连一个可指的二进制都没有。
**ZY11 把这两半都收进面板**：核心从 GitHub release 下载到自己的部署目录，**每个 zhdata 入站一个 unit**。

**核心（`service/zhdata_core.go`）**

- 面板托管路径 = `<s-ui 可执行文件所在目录>/zhdata/zhdata(.exe)`（与 `config.GetDBFolderPath` 同一个「部署目录」概念）。
- 版本源：公开仓库 `chentyson/zhdata` 的 latest release（`GET https://api.github.com/repos/chentyson/zhdata/releases/latest`），
  资产名 `zhdata-<os>-<arch>.zip`（`linux`/`windows`/`macos` × `64`/`arm64`；映射由 `zhdataAssetNameFor` 做）。
  资产 JSON 里的 `digest: "sha256:…"` 存在就校验，不存在就跳过（不假装校验过）。
- 当前版本 = 执行 `<binary> -version`（zhdata 打印 `zhdata 0.15.8`，解析不出 ⇒ `unknown`）；结果按「文件 size+mtime」缓存，
  安装后立刻清缓存 ⇒ 界面刷新就看见新版本，面板 10s 轮询也不会反复 fork 进程。
- `GET api/zhdataCore` → `{installed, path, external, current_version, latest_version, need_update, download_asset, units, error}`：
  `latest_version` 在查询失败时为 `null`（原因在 `error`）；`need_update` = 未装 / 版本解析不出 / 比最新旧；
  `external=true` 表示面板找到但不拥有这个二进制（旧 `/opt/zhdata` 安装或 `SUI_ZHDATA_PATH`），界面据此提示迁移。
- `POST api/zhdataCore/update`（两段路径，body `{}`，下载超时 120s）→ 下载 → 校验 sha256 → 解压 `zhdata/zhdata(.exe)` →
  临时文件 + `chmod 0755` → 旧文件改名成 `zhdata(.exe).bak` 后原子替换 → 清版本缓存 →
  对每个受托管 unit 先 `<binary> -init-view <面板 db>` 再 `systemctl restart`（**即使 ExecStart 没变也 restart**：跑着的还是旧进程）。
  成功返回 `{from, to, path, external, restarted:[unit…]}`，失败只返回错误且**不动**现有核心（不重启、不换文件）。
- 解析顺序：`SUI_ZHDATA_PATH`（最高，面板不更新它，更新请求直接报错让运维取消）→ 面板托管路径 → 旧 `/opt/zhdata/zhdata-tunnel-linux-amd64`。
  三处都没有 ⇒ `installed=false`，保存 zhdata 入站时卡片告警直接指向面板内的下载入口。

**每入站一个 unit（`service/zhdata_unit.go`）**

- 托管**所有** options 可解析的 zhdata 入站（不再要求 `admin_addr` 等于默认值）。命名：`admin_addr` 为默认 `127.0.0.1:6070` 的入站
  沿用 **`zhdata.service`**（ZY10 现场不动，含 `ExecStart` 逐字不变就不重启的兼容），其余按 tag 生成
  **`zhdata-<tag>.service`**（小写、`[^a-z0-9._-]`→`-`、超长截断、撞名加 `-2`/`-3`；按 tag 排序分配，结果确定）。
- ZY10 的谨慎逐条保留：unit 文本与渲染结果一致 ⇒ 什么都不做；`ExecStart` 变了 ⇒ 写文件（0644）+ `daemon-reload` + `-init-view` + `restart`（新建时还 `enable`）；
  unit 不存在但 `admin_addr` 上已有进程应答 ⇒ 只告警不写（不抢端口）；drop-in 里有 `ExecStart` 覆盖 ⇒ 告警；没有 systemctl / 容器 ⇒ 告警；非 linux ⇒ 不碰 systemd。
- **孤儿清理**：每次保存 zhdata 入站/客户时，删除「面板写过」（`Description` 以 `Description=zhdata-tunnel server` 开头）但当前没有入站对应的 unit
  （`stop` + `disable` + 删文件 + `daemon-reload`）。运维自己写的 `zhdata-*.service`（描述不是这个前缀）不会被碰，例如 C498 的 `zhdata-relay.service`/6071。
- **删 zhdata 入站** = `systemctl stop` + `disable` + 删 unit 文件 + `daemon-reload`（按 unit 文件里的 `Description=(<tag>)` 认单元，tag 改过名也找得回）；失败只告警不阻塞删除。

四步部署（ZY11 版）：

1. 面板升到 ZY11 → 备份 `db/s-ui.db*` → 换 `sui` → **`./sui migrate`**（ZY5 的 1.6.4 给存量客户补 `config.zhdata.uuid`，没这一步视图里是 0 行）→ 起服务。
2. 装核心：面板「入站」页的 **zhdata 核心** 处点更新即可（装到 `<部署目录>/zhdata/zhdata`），不需要再手工 scp。
   核心 ≥**0.15.7** 自带 `-init-view`（建 `zhdata_users` 视图；CentOS 7 的 sqlite3 没有 JSON1，语句放进核心，见核心接口文档 §3.5.1），面板在每次 restart 前自己跑一次。
   旧现场照旧可用：`/opt/zhdata/zhdata-tunnel-linux-amd64` 还在就继续用它（`external=true`），点一次更新就迁到面板托管路径并把 unit 的 `ExecStart` 换过去。
3. 面板里建 zhdata 入站：`listen_ports` 写端口池、`admin_addr` 填 `127.0.0.1:6070`（第二个入站填别的端口）、`backend`/`max_sessions`/`decoy` 按需
   ⇒ 保存后面板生成对应 unit 并 `enable --now`，日志出现 `zhdata unit: 已生成并启用 /etc/systemd/system/zhdata-xxx.service（入站 <tag>）`。
4. 核验：卡片 chip 显示 `core <版本> · N users pushed`；`systemctl list-units 'zhdata*'` 与入站一一对应；`systemctl cat` 的 `-listen` 与入站端口池一致；`GET /v1/stats` 回显 `up_kbps/down_kbps`。

改端口池/改 `max_sessions`/改 `backend` ⇒ 在界面上保存一次入站即可，面板只重写并重启**那个**入站的进程（别的入站不受影响，隧道断几秒）；
**重启后有个 ≤60s 的「不限速窗口」**（视图不带速率，速率靠下一次 `PUT /v1/users`），测限速前先等核心日志出现 `admin API users replaced: … bandwidth-limited`。

两个直接写 DB 的坑（都踩过）：

- `GET api/inbounds`（不带 `id`）是**摘要**（`id/type/tag/tls_id/listen/listen_port`），整行回灌 `POST api/save` 会把 zhdata 的 `listen_ports/backend/admin_addr/max_sessions` 清光。编辑必须走 `GET api/inbounds?id=<id>`（`MarshalFull`，带全字段），与 UI 一致。
- 直接写 `inbounds.options` 要写 **bytes**（blob）。写成 TEXT 会让面板每次列入站都报 `sql: Scan error … unsupported Scan`，zhdata 推送与统计全停。

独立核心 vs 内嵌进面板的取舍见 `docs/zhdata-embedded-vs-standalone.md`。

### 2.5 健康可见性（面板 UI 能看到核心异常）

所有 admin 调用同时是健康采样（`service/zhdata.go` 的按 tag 记录：可达性、核心版本、推送数 vs 面板客户数、核心 key 数、每个 `-users` 源的 key 数与最后一次错误）：

- `GET api/zhdataHealth` → 每个 zhdata 入站一条；同一份数据挂在 `api/load` 的 `zhdataHealth` 字段上（搭 10s 轮询，不额外发请求）。版本探测有 5 分钟节流，且刚失败的进程不再探（避免面板轮询被 5s 超时拖住）。
- 前端：入站卡片上 zhdata 一条状态 chip（正常=版本+推送数；异常=红色，tooltip 逐条列出 warning、`admin_addr`、最后一次错误、每个用户源 `path → keys`（带该源错误）与 `pushed/panel/core` 数量对比）；顶栏复用 maintenance chip 做全局「zhdata: K/N 异常」。
- warning 代码：`down`（进程不可达）、`user_source`（某用户源读取失败，例如 sqlite 路径写错）、`key_mismatch`（核心 key 数 < 面板客户数）、`bad_options` / `no_admin_addr`（入站本身配错）。

## 3. 客户凭据形状（clients.config.zhdata）

```json
{"name": "alice", "uuid": "<uuid>"}
```
- migration（对齐 1_6_3.go 写法）：存量客户复制其 vless uuid（没有则新造）填入 `config.zhdata.uuid`。
- 前端 randomConfigs 生成同样形状；Client.vue 里 zhdata 块显示/刷新 uuid。
- 链接：`zhdata://<uuid>@<host>:<port>[,<host2>:<port2>]#<remark>`（已在核心接口文档 §3.2 定案）。
  面板只写端点池的**第一个（最小）端口**，也不写 `conns=`/`portspan=` 参数（用客户端默认值）：
  - 端点池由**客户端**展开：zhenhanvn 的 zhdata 节点默认 `autoports=1`、`portspan=32`、`conns=32`
    （`ServiceLib/Models/Entities/ZhDataExtraItem.cs`），单端点会展开成 `port … port+31`，
    再按 `slot i → servers[i%N]`（zhdata `client.go`）选端点——slot0→49888、slot1→49889…
    服务器不需要也不会自己递增端口，它只在 `-listen` 列出的每个端口上各起一个 listener。
  - 所以**入站端口池必须从最小端口起连续**：面板写最小端口 + 客户端连续展开正好对上；
    端口池跳号会让一部分 slot 连到服务器没有监听的端口。
  - slot 上限是 64（握手字段 `maxSlots = 64`，zhdata `session.go`），`portspan`/`conns` 超过 64 对单个客户端无意义。
  - 需要偏离默认值时用链接 query：`conns=` / `spread=` / `dlconns=` / `migrate=` / `slotbase=` / `autoports=0` / `portspan=`。
    导入时若端点数正好等于 `portspan` 且连续，GUI 会折叠回单端点（`ZhDataFmt.ExpandAutoPorts`）。
  - 64 端口全列会让 URI 变成几百字节、客户端导入吃力。多端口（多台服务器）留给手工拼接。
- 存量链接刷新（ZY8 / migration `1_6_5.go`）：`clients.links` 只在保存客户或其入站时重写，
  所以 ZY5 那版生成器写出的整端口池 URI 会一直留在「用户编辑」弹窗、复制按钮和二维码里。
  migration 1.6.5 在启动时把 `zhdata://` 的 local 条目按当前生成器重算（其他协议的链接一字节都不动），
  幂等：已是当前形态的条目不改。入站被删掉、入站有多个 addrs、客户没有 zhdata 凭据这三种情况一律跳过。

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
6. 面板改客户到期日/流量包 → 客户端在隧道里查套餐立即（保存后推送）反映，无需等 1 分钟。
7. 把 zhdata 的 `-users` 指到一个不存在的 sqlite 路径 → 面板顶栏出现 zhdata 异常 chip，入站卡片 tooltip 里能看到「该用户源读取失败」而不必 ssh 看 journald。
8. 面板禁用/删除客户 → 该客户的 zhdata 隧道被切（自动 kick），新连接被拒。
9. 客户组填 `2M` → `curl -H "Authorization: Bearer <token>" http://127.0.0.1:6070/v1/stats | jq '.users[]|{uuid,up_kbps,down_kbps}'` 显示 `2000/2000`；该客户 zhenhanvn 测速上/下行都被压到 ~2Mbps（含 `-conns 32` 的多隧道），改组名为 `10M` 保存后**不重连**再测即变 ~10Mbps；组清空则不限速。同一客户走 hysteria2 入站时行为一致（同一个限速函数）。

## 6. zhdata 入站 transparent relay（zhdata→zhdata 透明转发，s-ui v1.6.3-ZY6+ / zhdata core ≥0.15.2）

zhdata 入站可把流量**字节级直通**转发给上游 zhdata（级联出口：VPS-A 面板侧鉴权/统计/限额，上游 zhdata-B 出网）。

- 入站 Options 新增字段：`"upstream": ["host:port", ...]`（字符串数组，host:port 列表；空/缺省 = 普通 direct 后端）。
- zhdata core 管理面（与 /v1/users 同一 -admin/-admin-token 鉴权）：
  - `PUT /v1/relay` 请求 `{"upstream":["10.0.0.1:49888"],"enabled":true}`（两字段可单独给；只给 enabled 保留现有 upstream；`enabled=true` 且列表为空 → 400；地址非法 → 400）。返回 `{"ok":true,"upstream":N,"enabled":true}`。
  - `GET /v1/relay` 返回 `{"enabled":bool,"upstream":[...],"pipes":N}`（pipes=当前在转发的管道数）。
  - 面板发禁用：`{"upstream":[],"enabled":false}`（清空+关闭；`enabled=false` 只影响新连接，已有管道走原 upstream 至自然结束）。
- 语义：
  - **字节直通**：下游会话到 zhdata 后不再解隧道，原样（含握手/帧）转发到 upstream（多 upstream 按连接轮询/粘性，P0 任一可达即可）。
  - **控制面保留在本地进程**：users 鉴权（PUT /v1/users）、stats 计数（GET /v1/stats，relay 会话照常计 up/down/devices）、kick（POST /v1/users/kick = 断开整条管道）全部仍由面板对接的这级 zhdata 执行；uuid/device_id 用下游客户端自己的。
  - zhdata 核心 <0.15.2 时对 `/v1/relay` 返回 404，面板只 warning。
- s-ui 侧：`upstream` 是纯入站 options，不拼内嵌 box；推送点与 users 完全相同（ConfigService.Save 提交后 obj∈{clients,inbounds} + deplete 后），逐 zhdata 入站 `PUT /v1/relay`。

## 7. zhdata 出站（内嵌 sing-box box 的出站，s-ui v1.6.3-ZY6+）

面板内嵌 sing-box 需要把其它入站（vless/hy2/...）的流量经 zhdata 通道送出去时，用内嵌 box 的 zhdata 出站（区别于 §2 的 external 入站进程）。

- 依赖：Go module `github.com/chentyson/zhdata`（v0.15.2 起库化）。开发期 `go.mod`：
  `require github.com/chentyson/zhdata v0.0.0` + `replace github.com/chentyson/zhdata => ../v2rayn/zhdata-src`。
- 注册：`core/register.go` 无条件 `outbound.Register[zhdata.ZhdataOutboundOptions](registry, "zhdata", ...)`；包在 `core/protocol/zhdata`。
- Options JSON（s-ui 出站编辑器 / config.json 出站条目）：
```json
{
  "type": "zhdata",
  "tag": "zhdata-out",
  "server": "1.2.3.4",
  "server_port": 49888,
  "uuid": "<出站 uuid>",
  "conns": 16,
  "spread": 8,
  "connect_timeout": "5s"
}
```
  - `uuid`：出站自己的凭据（与面板客户无关；不需要 users 推送——它是客户端侧）。
  - `device_id` 不出现在配置里：出站实例懒加载 `zhdata.NewClient` 时 DeviceID=nil（本机随机、进程存续期稳定）。
  - `conns/spread` 透传 zhdata ClientConfig；0=核心默认。`connect_timeout`（badoption.Duration）作用于 DialContext（调用方已有 deadline 时不覆盖）。
  - `DLPool` 面板侧固定传 **-1**（auto：conns/2 条下载专用会话），与 standalone CLI/zhenhanvn 默认行为一致；不设则 0=共享池，下载吞吐路径不同。`SlotBase/Train` 不设=0（关），与 CLI 默认一致。
  - 懒加载：首个 Dial 才建 `zhdata.Client`（Servers=[server:port]）；出站 Close 释放。
- **UDP 不支持**：zhdata 无 PacketConn；Network() 只声明 tcp，ListenPacket 返回明确错误 `zhdata outbound: udp not supported`。
- 库语义（zhdata v0.15.2，已核对）：`DialContext` 等服务端 tOpenAck 才返回；返回的 net.Conn 另实现 `CloseWrite() error`（半关闭透传）；host 可为域名（服务端解析）；`Conns<=0→16`、`Spread<=0→8`；库客户端不监听本地端口，无端口冲突。

## 8. zhdata 入站连接列表（GET /v1/sessions，s-ui v1.6.3-ZY6+ / zhdata core 同步添加）

面板"连接列表"弹窗（Sessions.vue，resource=inbound/user）读内嵌 SessionTracker；zhdata 是 external 进程，核心看不到，改由 admin 面拉取（同一 -admin/-admin-token 鉴权，5s 超时）。

- `GET /v1/sessions[?limit=n]` → `{"ts":<unix s>,"total":N,"sessions":[{"uuid","device"(32hex,""=旧 zhv4/5/6 握手),"slot":int,"remote":"ip:port","started_at":<unix s>,"relay":bool}]}`。
  - `limit`：默认 50，有效区间 10<n<100（面板用 100）；按 started_at 取最近 n 条，`total`=未截断总数（响应无 total 视为未截断）。
  - `started_at`/`ts` 均为 **unix 秒**（与面板 SessionInfo.createdAt、tracker `Unix()` 同单位，不换算）。
  - 条目 = **隧道会话**（uuid+device 的一条 slot），不是每条应用流一条；relay 管道也计入（`relay:true`）；空 device 合法（去重键必须含 uuid）。
  - 无每会话流量计数（uuid 累计在 §1.3 /v1/stats）。
- 面板映射（service/zhdata.go `zhdataSessions`，对齐 core.SessionInfo/Sessions.vue 字段）：
  - `id = "<入站tag>:<uuid>:<device>:<slot>"`；`inbound` = 入站 tag；`user` = uuid 反查面板客户 name（查不到显示 uuid）；
  - `network = "zhdata"`（`relay:true` 的条目把 `rule` 置 `"relay"` 以在 tooltip 显示）；`source` = remote；`destination` = ""（隧道会话无目的地，流由对端解析）；`created_at` = started_at（**秒**，与 tracker 快照 `CreatedAt.Unix()` 同单位）；`up/down` = 0。
  - 拉取失败/超时：warning + 该入站按空列表处理，不缓存（弹窗自带 5s 自动刷新）。
- 查询路径（service/stats.go GetSessions）：
  - `resource=inbound` 且 tag 是 zhdata 入站 → 只查该进程；
  - `resource=user` 且客户有 config.zhdata.uuid → 遍历全部 zhdata 入站拉取、按 uuid 过滤、与内嵌会话合并去重（按 created_at 倒序）。
- Close All（StatsService.CloseUserSessions）：先 `POST /v1/users/kick` 全部 zhdata 入站（与内嵌 core 是否在跑无关；kick 语义=断会话/管道，客户端会重连除非已 disable，见 §1.4），再走原有内嵌 CloseByUser/KickUserSessions，行为不变。
- 在线徽章数字（onlines JSON 新增 `"inboundCounts": {<入站tag>: <设备数>}`，GetOnlines 实时组装）：
  - zhdata 入站 = 该入站 `GET /v1/stats` 各 uuid devices 求和（与 online overlay 同一采集、同一 staleness 门，15s 采样，失联即从 map 消失；空闲入站也在 map 中值为 0）；
  - 内嵌入站 = usersession 全局设备表按 `tag\x1fsource` 前缀聚合（`GlobalDeviceCountsByTag`，与 TryBind/GetDeviceCounts 同源）；
  - 两侧 tag 不冲突，直接合并；前端按此字段显示数字（缺字段则回落旧"在线点"逻辑）。
