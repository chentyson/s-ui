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
const VersionSuffix = "ZY8"

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
