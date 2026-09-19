# 每用户带宽限速（group 即带宽）—— 修改说明

> 定制内容，非上游功能。基线上游：`alireza0/s-ui` **v1.6.3**；本仓库在其上叠加两个公司补丁：
> `devlimit`（设备数限制，commit `c91dea8`）+ **`bwlimit`（本说明，commit `5c894ab`）**。

## 1. 需求与背景

**需求**：给 s-ui 的**每个用户（客户端账号）**加带宽限制，而不是限制整个入站；设置要简单，直接写 `1.5M` / `2M` 这种值。

**为什么不能直接用 sing-box 的字段**（2026-09 查证 sing-box v1.14.1 源码）：

| 事实 | 出处 |
|---|---|
| sing-box **完全没有**限速字段（`rate_limit` / `per_connection_rate_limit` 全仓 grep 无） | `option/` 全目录 |
| hysteria / hysteria2 的 `up_mbps` / `down_mbps` 是**入站级**且为 `int`（填 `1.5` 会让核心 unmarshal 失败、核心起不来） | `option/hysteria2.go:17,18,208,209`、`option/hysteria.go` |
| `SendBPS = up_mbps × 125000`（`constant.MbpsToBps`），所以整数 Mbps 的粒度就是 125000 B/s | `protocol/hysteria2/inbound.go:183` |
| hysteria 的 brutual CC 是 **per-connection** 的：即使按用户下发 `up_mbps`，用户开 N 条连接就 N 倍带宽 —— **达不到"每用户封顶"** | `sing-quic/hysteria2/service.go` ServeHTTP |
| 但 s-ui **自己 vendor 了全部协议 inbound**，每条在连接入口都已拿到「用户名 + net.Conn」 | `core/protocol/{vless,vmess,trojan,snell,shadowsocks,hysteria,hysteria2,tuic,anytls}/inbound.go` |

**结论**：在 s-ui 自有 inbound 层做"每用户共享令牌桶"是唯一能真正**按用户聚合封顶**、且**不必 fork sing-box / sing-quic** 的方案。这也是本次采用的方案。

## 2. 业务语义（对操作者的约定）

客户端的 **`group`（分组）字段就是带宽设置**：

| group 值 | 行为 |
|---|---|
| 空 | 不限速 |
| `1M` / `1.5M` / `2M` / `500K` / `1G` | 该账号**上传**和**下载**都限到这个值（同值） |
| 裸数字 `2` | 等同 `2M`（毫比特） |
| 带单位词 `500Kbps` / `2Mbit` | 同上（大小写不敏感，可选带 `B`） |
| 其他任何非空值（`vip`、`special`、`H28`…） | **无法识别 → 立即按 `1M` 限速**；并在**下次保存该客户端时把 group 改写成 `1M`**（面板里能看到改后的值） |

设计意图：group 既有的分组含义被带宽取代；**非空即受限**，所以一个写错的标签不会意外给出无限速线路。

## 3. 实现

### 3.1 新增包 `core/bwlimit`（照 `core/devlimit` 的形状）

| 文件 | 职责 |
|---|---|
| `group.go` | 解析 group → 带宽（kbit/s）。`ParseKbit` 认 `1M/1.5M/500K/2/1Mbps/2Mbit`；`Group()` 给出该 group 代表的限速（不可识别 → 默认 1M）；`CanonicalGroup()` 给出"应当存成什么"（不可识别 → `1M`） |
| `bwlimit.go` | 进程内注册表 `map[用户名]{Up,Down}` + **每用户每方向一个共享 `*rate.Limiter`**；`Reload()` 热更新（已建立的连接也立即改道/取消），`Get()`/`Configured()`，`Wrap()` |
| `conn.go` | `limitedConn`：包一层 net.Conn，**读=上行、写=下行**，按已搬运字节数扣桶；`Close()` 取消等待，`CloseWrite/CloseRead` 透传 |

要点：
- **共享桶**是"每用户封顶"的关键：同一账号的 10 条连接共用一个桶，不会 10 倍带宽。
- 桶容量（burst）= `max(1 秒流量, 64KB)`；`WaitN` 超过 burst 会报错，所以内部按 burst 分段等待。
- 单位换算与 sing-box 一致：`kbit/s × 125 = 字节/秒`（1.5M = 1500 kbit = 187500 B/s）。
- 无 group 的账号**完全不包装**（零开销）。

### 3.2 九个协议入站各挂一行钩子

在"用户身份已解析、尚未交给路由"的位置插入 `conn = bwlimit.Wrap(用户名, conn)`：

| 协议 | 位置 |
|---|---|
| vless / vmess / trojan | `newConnectionEx` 中 `metadata.User = user` 之后 |
| shadowsocks（multi） | `newConnection` 中 `sessions.Untrack` 之后、`tracker` 之前 |
| snell | `NewConnectionEx` 中 `users` 分支内（用户名为数组下标解析出的名字） |
| hysteria / tuic / anytls | `NewConnectionEx` 中 `sessions.Bind` 之后 |
| hysteria2 | `NewConnectionEx` 中设备限制检查（devlimit）之后 |

> Multiplex（mux）**载体连接也会被限速**——否则走 mux 的流量会绕过策略。

### 3.3 配置来源与热更新

| 时机 | 动作 | 代码 |
|---|---|---|
| 核心启动 / 重建配置 | `bwlimit.Reload(loadGroupLimits())` | `service/config.go` `GetConfig()` |
| 面板**保存**任意对象（提交成功后） | `ReloadGroupLimits()`（一行日志：`bwlimit: N client(s) capped by group`） | `service/config.go` `Save()` 的 commit 之后 |
| 保存客户端时 | 不可识别的 group 改写成 `1M`（含批量新增/批量编辑） | `service/client.go` `normalizeGroup()` |

- 读库用原始 SQL（`SELECT name, "group" FROM clients`，`group` 是关键字必须加引号），查不到只 warning 不上报失败；无 DB 时直接返回"无人受限"。
- **不需要重启节点、也不需要重启核心**：保存后即时生效。

### 3.4 前端

- 客户端编辑框的「组」加了提示文案 `client.groupHint`（中/繁/英：`分组即带宽：1.5M / 2M / 500K，留空不限速`）。
- 没有新增字段/页面；`group` 原本就在编辑框和列表里，操作习惯不变。

## 4. 已知边界（重要）

1. **只覆盖 TCP 连接**：UDP（`NewPacketConnectionEx` 路径，如 hysteria2 的 UDP 中继）**尚未限速**（第二期）。TCP 是流量大头。
2. **用户态"软"限速**：桶容量 ≈ 1 秒流量（最小 64KB），测量窗口内的初始突发会滑过去，长期均值即设定值。实测比设定值高 10~25%。
3. **每台出口节点各自计数**：同一账号连多台节点会各拿一份配额（无中心化账本）。
4. **与入站自带带宽取 min**：hysteria2 入站的 `up_mbps/down_mbps` 若填得比用户档位低，brutal 会先卡住，此时用户桶不起作用（建议入站这两项留高或留 0）。
5. **非空即受限**：老节点上已有的非带宽分组会被直接按 `1M` 卡住 —— 部署前**必须**做分组审计（见 5.1）。

## 5. 运维

### 5.1 部署前必做：分组审计

```sql
SELECT "group", COUNT(*) FROM clients GROUP BY 1 ORDER BY 2 DESC;
```
- 该限速的：改成 `1.5M` / `2M` 这类值
- 不该限速的：**清空**
- 面板里改更直观（客户端编辑框，「组」下面有提示）

### 5.2 生效确认

```bash
journalctl -u s-ui --no-pager | grep bwlimit
#   bwlimit: N client(s) capped by group                            ← 限速表已刷新
#   bwlimit: client xxx group vip is not a bandwidth, storing 1M    ← 分组被改写
```

### 5.3 实机验证（真流量，无需客户端软件）

仓库脚本 `scripts/bwtest.py`（python3，纯标准库）：用 trojan 隧道 + 进程内 loopback 大文件服务，客户端/s-ui/目标全在同一台机器 → 排除跨境带宽与磁盘干扰，**唯一瓶颈就是限速器**。

```bash
# 测试入站：type=trojan, listen=127.0.0.1:38543；测试账号：bwtest-cap(group=1.5M) / bwtest-free(group=1000M)
python3 bwtest.py 38543 <cap密码> 1 6     # 单连接 ≈ 187 KB/s
python3 bwtest.py 38543 <cap密码> 3 6     # 3 并发：总量仍 ≈187 KB/s ← 聚合的关键证据
python3 bwtest.py 38543 <free密码> 1 5    # 对照：应跑到 loopback 上限
```

**2026-09-19 在 H28（107.173.159.175）实测**：

| 场景 | 结果 |
|---|---|
| 不限速账号（group=1000M）单连接 | 1196 Mbps |
| group=1.5M 单连接 | 224 KB/s = 1.83 Mbps |
| group=1.5M **3 连接并发** | 201 KB/s = 1.65 Mbps（**总量仍≈1.5M**） |
| group=1.5M 单连接复测 | 191 KB/s = 1.56 Mbps |

### 5.4 单元/闭环测试

```bash
go test ./core/bwlimit/ ./service/
```
覆盖：解析各种写法、不可识别按默认限速、双向限速实测（net.Pipe）、多连接共享桶、热更新（改小/取消）、关闭时释放等待、并发安全、DB → 生效闭环、缺表容错。

## 6. 构建与升级（保持"一条命令出标准件"）

```bash
# 上游新版本（如 v1.7.0）→ 自动 clone + 打两个补丁 + 构建前端 + 静态编译 + 6 点校验
bash ~/.hermes/skills/node-ops/sui-1.6.3-fleet-upgrade/scripts/sui-build-patched.sh v1.7.0
```

- 补丁由本仓库提交区间生成（新文件必须先 `git add -N`，否则 `git diff` 不带它们）：
  `git diff 13abbdc c91dea8` = devlimit；`git diff c91dea8 <bwlimit-commit>` = bwlimit；合起来 = 一个补丁
- ⚠️ **`frontend/` 是上游 `alireza0/s-ui-frontend` 子模块**（不是我们的 fork）：前端改动（提示文案 + i18n）**不随本仓库走**，单独以 `bwlimit-frontend.patch` 形式保存在升级技能里、在 `frontend/` 内 `git apply`；构建脚本已自动处理
- 二进制硬性要求（坑已踩过）：**静态链接 + `CGO_ENABLED=1` + `with_acme`**；缺 `with_acme` 的构建会在面板保存设置后让核心死循环
- 升级后校验：静态链接 / `ACME is not included in this build` 串为 0 / `core/devlimit` >0 / `core/bwlimit` >0 / `go-sqlite3 requires cgo` 串为 0 / 前端 `groupHint` 已嵌入

## 7. 改动清单（相对上游 v1.6.3）

| 文件 | 变化 |
|---|---|
| `core/bwlimit/bwlimit.go` `conn.go` `group.go` | **新增**（注册表 / 连接包装 / 组解析） |
| `core/bwlimit/bwlimit_test.go` | **新增**（10 个用例） |
| `service/bwlimit_test.go` | **新增**（DB → 生效闭环、缺表容错） |
| `service/client_group_test.go` | **新增**（保存时的分组规范化） |
| `service/config.go` | 读分组、热更新、保存后刷新、一行日志 |
| `service/client.go` | `normalizeGroup()`（新增/编辑/批量三处调用） |
| `core/protocol/{vless,vmess,trojan,snell,shadowsocks,hysteria,hysteria2,tuic,anytls}/inbound.go` | 各 1~2 行钩子 + import |
| `go.mod` | `golang.org/x/time` 提为直接依赖（令牌桶） |
| `frontend/src/layouts/modals/Client.vue` | 「组」字段加提示 |
| `frontend/src/locales/{en,zhcn,zhtw}.ts` | `client.groupHint` 文案 |

合计约 200 行新代码 + 24 行钩子，**未改动 sing-box / sing-quic 任何依赖**。

## 8. 回滚

```bash
systemctl stop s-ui
cp -a /usr/local/s-ui/backup-<ts>/sui.bin.old /usr/local/s-ui/sui
cp -a /usr/local/s-ui/backup-<ts>/s-ui.db     /usr/local/s-ui/db/
systemctl start s-ui
```
（限速是纯运行时行为，DB 无 schema 变更 —— 除 `devs` 列由 devlimit 补丁需要外；回滚二进制后 group 里的带宽值只是普通标签，不产生副作用。）
