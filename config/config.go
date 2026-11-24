package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed version
var version string

//go:embed name
var name string

type LogLevel string

const (
	Debug LogLevel = "debug"
	Info  LogLevel = "info"
	Warn  LogLevel = "warn"
	Error LogLevel = "error"
)

// VersionSuffix 是自研改动的代际后缀：官方基线版本号（config/version）保持不动，
// 面板上显示成 <基线>-<后缀>，例如 1.6.3-ZY3。每做一次自研改动就 +1；
// 换官方基线（如 1.6.3 → 1.7.0）时从 ZY1 重新计数。
//
//	ZY1  连接时设备数限制（devlimit，core/devlimit）
//	ZY2  每用户带宽限速（bwlimit，按 client.group）+ client devs/email 字段 + 在线设备数/拒绝日志
//	ZY3  bwlimit 半关闭修复（core/bwlimit/conn.go：QUIC 类协议 stream 被整条关闭 → 官方 hysteria2/tuic 客户端 EOF）
//	ZY4  订阅端口新增查询接口 GET /sub/query?key=*** 按 uuid/password 匹配用户，
//	     返回到期日/流量限额/当前用量/devs/email/enable（sub/queryService.go）
//	ZY5  zhdata external 入站管理（service/zhdata.go、cronjob/zhdataStatsJob.go、
//	     migration 1.6.4 补 clients.config.zhdata）+ 面板内跨入站全局总设备数
//	     （core/usersession 全局设备表，TryBind 跨入站合计）
//	ZY6  zhdata 出站（内嵌 box 注册 core/protocol/zhdata，依赖 zhdata v0.15.2 库化）+
//	     zhdata 入站 transparent relay（options.upstream → PUT /v1/relay）
//	ZY7  zhdata 健康可见性：admin 调用即健康采样 → GET api/zhdataHealth（并挂在
//	     api/load 上），前端入站卡片 chip + 顶栏告警 chip；PUT /v1/users 附带
//	     expiry/quota/used（隧道内套餐查询走面板值）；对离开用户集的客户自动 kick；
//	     resetTraffic 后补推送；fetchUsers 跳过无该协议凭据的客户（NULL 不再炸掉配置）
//	ZY8  migration 1.6.5：把 clients.links 里 ZY5 时代生成的 zhdata 链接刷成当前形态
//	     （只写端口池第一个端口，remark 走 JoinRemark）——该列只在保存客户/入站时重写，
//	     所以升级后「用户编辑」弹窗里的链接与二维码仍显示旧的整端口池 URI
//	ZY9  PUT /v1/users 附带 up_kbps/down_kbps（zhdata >= 0.15.6）：client.group 的
//	     带宽限速不再只作用于 sing-box 入站，zhdata 隧道进程与 relay 也按同一组
//	     2M/500K 限速（此前核心没有按用户限速，面板填的组对 zhdata 完全无效）
//	ZY10 面板托管 zhdata 进程的启动参数（service/zhdata_unit.go）：保存 zhdata 入站时
//	     按 listen_ports/backend/decoy/max_sessions 渲染 /etc/systemd/system/zhdata.service，
//	     缺失就创建并 enable，ExecStart 变了就 daemon-reload + restart（参数没变不动进程），
//	     并在启动前跑核心 `-init-view` 建用户视图；发现 drop-in 里有 ExecStart 覆盖、
//	     二进制缺失、admin_addr 上已有别人的进程等情况只在入站卡片上告警，不再静默
//	     （C455 现场：界面把端口改成 45501-45564，进程还听 49888-49919，客户端连不上）
//	ZY11 面板托管 zhdata 核心本体（service/zhdata_core.go）：GET api/zhdataCore 给出当前版本/最新
//	     版本/是否需要更新与资产名，POST api/zhdataCore/update 从 GitHub chentyson/zhdata 的 latest
//	     release 下载 zhdata-<os>-<arch>.zip（资产带 digest 就校验 sha256）→ 解压 zhdata/zhdata(.exe)
//	     → 临时文件 + chmod 0755 + 备份旧文件后原子替换到「面板目录/zhdata/」→ 对每个受托管 unit
//	     跑 -init-view 再 systemctl restart。SUI_ZHDATA_PATH 指向面板外的核心时优先级最高且不更新它；
//	     ZY10 时代的 /opt/zhdata/zhdata-tunnel-linux-amd64 仍可用，但核心状态里标 external=true，
//	     界面提示迁移；两处都没有 ⇒ installed=false，保存入站时的告警直接指向面板内的下载入口。
//	     service/zhdata_unit.go 同时改成**每个 zhdata 入站一个 unit**：admin_addr 为默认 127.0.0.1:6070
//	     的入站沿用 zhdata.service（ZY10 现场不动），其余按 tag 生成 zhdata-<tag>.service（小写、
//	     [^a-z0-9._-]→-、撞名加序号）；ZY10 的谨慎全部保留（ExecStart 没变不 restart、unit 不存在但
//	     admin_addr 上已有进程应答就只告警不写、drop-in 覆盖告警、没有 systemctl/容器告警），每次同步
//	     顺带删掉面板写过但已无对应入站的 unit（判定：Description 以 "zhdata-tunnel server" 开头），
//	     删 zhdata 入站时 stop + disable + 删 unit 文件 + daemon-reload（失败只告警不阻塞删除）
const VersionSuffix = "ZY11"

// GetVersion 返回面板版本号：官方基线 + 自研后缀。
// -v、启动日志、面板仪表盘（appVersion）都用它。
func GetVersion() string {
	return GetBaseVersion() + "-" + VersionSuffix
}

// GetBaseVersion 返回官方基线版本号（不含自研后缀），供需要与上游做版本比较的地方使用。
// 注意：cmd/migration 用 parseVersion 比较，已自动忽略 "-" 之后的后缀，所以数据库里
// 存带后缀的版本也是安全的。
func GetBaseVersion() string {
	return strings.TrimSpace(version)
}

func GetName() string {
	return strings.TrimSpace(name)
}

func GetLogLevel() LogLevel {
	if IsDebug() {
		return Debug
	}
	logLevel := os.Getenv("SUI_LOG_LEVEL")
	if logLevel == "" {
		return Info
	}
	return LogLevel(logLevel)
}

func IsDebug() bool {
	return os.Getenv("SUI_DEBUG") == "true"
}

func GetDBFolderPath() string {
	dbFolderPath := os.Getenv("SUI_DB_FOLDER")
	if dbFolderPath == "" {
		dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			// Cross-platform fallback path
			if runtime.GOOS == "windows" {
				return "C:\\Program Files\\s-ui\\db"
			}
			return "/usr/local/s-ui/db"
		}
		dbFolderPath = filepath.Join(dir, "db")
	}
	return dbFolderPath
}

func GetDBPath() string {
	return fmt.Sprintf("%s/%s.db", GetDBFolderPath(), GetName())
}
