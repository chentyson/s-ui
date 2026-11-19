# s-ui 用户查询接口（ZY4）

> 版本：`1.6.3-ZY4`
> 模块：`sub/queryService.go`
> 路由：`GET /sub/query`

## 概述

在订阅端口（subPort）上新增的公开查询接口。通过用户的 **uuid** 或 **password** 匹配到对应客户端，返回其到期日、流量限额、当前用量等信息。

无需登录面板，无需订阅 token，直接 HTTP GET 即可。

## 请求

```
GET {subPath}query?key=***
```

| 参数 | 位置 | 类型 | 必填 | 说明 |
|------|------|------|------|------|
| `key` | query | string | 是 | 用户的 uuid 或 password（见下方匹配规则） |

### 匹配规则

`key` 会与该节点下**所有客户端**的 config 中每个协议条目的 `uuid` 和 `password` 字段逐一比对（大小写敏感）。命中任意一条即返回该客户端。

覆盖的协议：

| 协议 | 匹配字段 |
|------|----------|
| VLESS | `uuid` |
| VMess | `uuid` |
| TUIC | `uuid` + `password` |
| Shadowsocks | `password` |
| Shadowsocks-2022 | `password` |
| Trojan | `password` |
| AnyTLS | `password` |
| Hysteria / Hysteria2 | `password` |
| Snell | `userkey`（不匹配，仅 uuid/password） |
| Naive / Socks / HTTP / Mixed | `password` |

> 一个客户端通常有多条协议条目，uuid 和 password 可能相同也可能不同。任意一条命中即返回。

## 响应

### 成功（HTTP 200）

```json
{
  "code": 200,
  "msg": "ok",
  "data": {
    "name": "C49801",
    "expiry": 1823903999,
    "expiry_str": "2027-10-19 03:29:59",
    "unlimited_time": false,
    "volume": 0,
    "volume_str": "0.00B",
    "unlimited_traffic": true,
    "used": 21940183575,
    "used_str": "20.43GB",
    "up": 1958886253,
    "down": 19981297322,
    "devs": 0,
    "email": "1716677@qq.com",
    "enable": true
  }
}
```

### 未找到（HTTP 404）

```json
{
  "code": 404,
  "msg": "not found"
}
```

### 缺少 key（HTTP 400）

```json
{
  "code": 400,
  "msg": "key is required"
}
```

### 内部错误（HTTP 500）

```json
{
  "code": 500,
  "msg": "internal error"
}
```

## 字段说明

| 字段 | 类型 | 说明 |
|------|------|------|
| `name` | string | 客户端名称（即订阅 ID） |
| `expiry` | int64 | 到期时间戳（Unix 秒），`0` 表示永不过期 |
| `expiry_str` | string | 到期时间可读格式 `YYYY-MM-DD HH:MM:SS`（面板时区），永不过期时为空 |
| `unlimited_time` | bool | `true` = 永不过期（expiry ≤ 0） |
| `volume` | int64 | 流量限额（字节），`0` 表示不限量 |
| `volume_str` | string | 流量限额可读格式（如 `10.00GB`），不限量时为 `0.00B` |
| `unlimited_traffic` | bool | `true` = 流量不限量（volume ≤ 0） |
| `used` | int64 | 已用流量（字节）= up + down |
| `used_str` | string | 已用流量可读格式（如 `20.43GB`） |
| `up` | int64 | 上传流量（字节） |
| `down` | int64 | 下载流量（字节） |
| `devs` | int | 设备数限制，`0` 表示不限制 |
| `email` | string | 客户端绑定的邮箱 |
| `enable` | bool | 客户端是否启用 |

## 调用示例

### 按 VLESS uuid 查询

```bash
curl "http://154.12.53.217:62444/62444/query?key=***"
```

### 按 Hysteria2 password 查询

```bash
curl "http://154.12.53.217:62444/62444/query?key=***"
```

### 按 Shadowsocks password 查询

```bash
curl "http://154.12.53.217:62444/62444/query?key=***"
```

## 注意事项

1. **端口**：接口运行在订阅端口（subPort），与订阅链接同一端口。若面板配置了 `subDomain`（域名校验）或 `subCertFile`/`subKeyFile`（HTTPS），查询接口同样继承这些约束。
2. **无鉴权**：接口本身不做身份验证，`key` 即凭证。知道 uuid 或 password 即可查询。
3. **单节点**：只查询当前节点的客户端，不跨节点。
4. **性能**：每次请求遍历该节点所有客户端并解析 config JSON。节点客户端数量通常 < 100，性能无压力。
5. **时区**：`expiry_str` 使用面板设置的时区（`GetTimeLocation`），非 UTC。

## 改动文件

| 文件 | 说明 |
|------|------|
| `sub/queryService.go` | 新增：QueryService、QueryResult、匹配逻辑、流量格式化 |
| `sub/subHandler.go` | 注册 `GET /query` 路由 + handler |
| `config/config.go` | VersionSuffix `ZY3` → `ZY4` |
