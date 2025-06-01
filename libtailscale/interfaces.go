// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// libtailscale 包为 Android 平台 Tailscale 客户端提供核心接口与类型定义。
package libtailscale

import (
	"log"

	_ "golang.org/x/mobile/bind" // 用于 gomobile 绑定，实际不直接引用
)

// Start 启动应用，存储状态到 dataDir，并使用 appCtx。
// dataDir: 数据目录。
// directFileRoot: SAF 文件根目录。
// appCtx: Android App 上下文。
// 返回 Application 实例。
func Start(dataDir, directFileRoot string, appCtx AppContext) Application {
	// 直接调用内部 start 方法，返回 Application 实例
	return start(dataDir, directFileRoot, appCtx)
}

// AppContext 提供应用运行的上下文，所有方法均由 Android 侧实现。
type AppContext interface {
	// Log 记录日志。
	// tag: 日志标签。
	// logLine: 日志内容。
	Log(tag, logLine string)

	// EncryptToPref 加密存储键值对。
	// key: 键。
	// value: 值。
	EncryptToPref(key, value string) error

	// DecryptFromPref 解密读取键值。
	// key: 键。
	// 返回值和错误。
	DecryptFromPref(key string) (string, error)

	// GetOSVersion 获取 Android 版本。
	GetOSVersion() (string, error)

	// GetModelName 获取设备型号。
	GetModelName() (string, error)

	// GetInstallSource 获取安装来源。
	GetInstallSource() string

	// ShouldUseGoogleDNSFallback 是否使用 Google DNS 作为兜底。
	ShouldUseGoogleDNSFallback() bool

	// IsChromeOS 是否为 ChromeOS 设备。
	IsChromeOS() (bool, error)

	// GetInterfacesAsString 获取所有网络接口字符串。
	GetInterfacesAsString() (string, error)

	// GetPlatformDNSConfig 获取当前 DNS 配置字符串。
	GetPlatformDNSConfig() string

	// GetSyspolicyStringValue 获取系统策略字符串值。
	GetSyspolicyStringValue(key string) (string, error)

	// GetSyspolicyBooleanValue 获取系统策略布尔值。
	GetSyspolicyBooleanValue(key string) (bool, error)

	// GetSyspolicyStringArrayJSONValue 获取系统策略字符串数组（JSON）。
	GetSyspolicyStringArrayJSONValue(key string) (string, error)
}

// IPNService 对应 Java 侧的 IPNService。
type IPNService interface {
	// ID 返回唯一实例 ID。
	ID() string

	// Protect 保护指定 fd 不被 VPN 捕获。
	Protect(fd int32) bool

	// NewBuilder 创建新的 VPNServiceBuilder。
	NewBuilder() VPNServiceBuilder

	// Close 关闭服务。
	Close()

	// DisconnectVPN 断开 VPN。
	DisconnectVPN()

	// UpdateVpnStatus 更新 VPN 状态。
	UpdateVpnStatus(bool)
}

// VPNServiceBuilder 对应 Android 的 VpnService.Builder。
type VPNServiceBuilder interface {
	// SetMTU 设置 MTU。
	SetMTU(int32) error
	// AddDNSServer 添加 DNS 服务器。
	AddDNSServer(string) error
	// AddSearchDomain 添加搜索域。
	AddSearchDomain(string) error
	// AddRoute 添加路由。
	AddRoute(string, int32) error
	// ExcludeRoute 排除路由。
	ExcludeRoute(string, int32) error
	// AddAddress 添加地址。
	AddAddress(string, int32) error
	// Establish 建立 VPN，返回 ParcelFileDescriptor。
	Establish() (ParcelFileDescriptor, error)
}

// ParcelFileDescriptor 对应 Android 的 ParcelFileDescriptor。
type ParcelFileDescriptor interface {
	// Detach 分离 fd。
	Detach() (int32, error)
}

// Application 封装运行中的 Tailscale 应用。
type Application interface {
	// CallLocalAPI 调用本地 API。
	CallLocalAPI(timeoutMillis int, method, endpoint string, body InputStream) (LocalAPIResponse, error)
	// CallLocalAPIMultipart 调用本地 API（multipart）。
	CallLocalAPIMultipart(timeoutMillis int, method, endpoint string, parts FileParts) (LocalAPIResponse, error)
	// NotifyPolicyChanged 通知策略变更。
	NotifyPolicyChanged()
	// WatchNotifications 订阅通知。
	WatchNotifications(mask int, cb NotificationCallback) NotificationManager
}

// FileParts 表示多个文件分片。
type FileParts interface {
	// Len 返回分片数量。
	Len() int32
	// Get 获取指定分片。
	Get(int32) *FilePart
}

// FilePart 表示单个 multipart 文件。
type FilePart struct {
	ContentLength int64       // 文件长度
	Filename      string      // 文件名
	Body          InputStream // 文件流
	ContentType   string      // 可选 MIME 类型
}

// LocalAPIResponse 本地 API 响应。
type LocalAPIResponse interface {
	StatusCode() int
	BodyBytes() ([]byte, error)
	BodyInputStream() InputStream
}

// NotificationCallback 通知回调。
type NotificationCallback interface {
	OnNotify([]byte) error
}

// NotificationManager 通知管理器。
type NotificationManager interface {
	Stop()
}

// InputStream 适配 Java InputStream。
type InputStream interface {
	Read() ([]byte, error)
	Close() error
}

// OutputStream 适配 Java OutputStream。
type OutputStream interface {
	Write([]byte) (int, error)
	Close() error
}

// ShareFileHelper 对应 Kotlin 侧 ShareFileHelper。
type ShareFileHelper interface {
	// OpenFileWriter 打开写入流。
	OpenFileWriter(fileName string) OutputStream
	// OpenFileURI 打开文件并返回 SAF URI。
	OpenFileURI(filename string) string
	// RenamePartialFile 重命名部分文件，返回新 URI。
	RenamePartialFile(partialUri string, targetDirUri string, targetName string) string
}

// RequestVPN 通知 Go 层有新的 VPN 服务需要处理。
// service: IPNService 实例。
func RequestVPN(service IPNService) {
	// 发送 IPNService 实例到全局 onVPNRequested 通道，通知 Go 层有新的 VPN 服务需要处理
	onVPNRequested <- service
}

// ServiceDisconnect 通知 Go 层 VPN 服务已断开。
// service: IPNService 实例。
func ServiceDisconnect(service IPNService) {
	// 发送 IPNService 实例到全局 onDisconnect 通道，通知 Go 层 VPN 服务已断开
	onDisconnect <- service
}

// SendLog 发送日志到全局日志通道。
// logstr: 日志内容字节数组。
func SendLog(logstr []byte) {
	// 非阻塞发送日志到 onLog 通道，通道满则丢弃并打印警告
	select {
	case onLog <- string(logstr):
		// 成功发送日志
	default:
		// 通道已满，日志未发送，打印警告
		log.Printf("Log %v not sent", logstr) // 这里 logstr 直接打印字节数组
	}
}

// SetShareFileHelper 设置全局 ShareFileHelper 实例。
// fileHelper: ShareFileHelper 实例。
func SetShareFileHelper(fileHelper ShareFileHelper) {
	// 如果通道已有旧值，先清空，保证只保留最新 helper
	select {
	case <-onShareFileHelper:
		// 已清空旧值
	default:
		// 通道本为空，无需处理
	}
	// 尝试发送新 helper，若通道仍满则强制清空再发送
	select {
	case onShareFileHelper <- fileHelper:
		// 发送成功
	default:
		// 通道仍满，强制清空再发送
		<-onShareFileHelper
		onShareFileHelper <- fileHelper
	}
}

// SetDirectFileRoot 设置全局 directFileRoot 路径。
// filePath: SAF 根路径。
func SetDirectFileRoot(filePath string) {
	// 直接发送 SAF 根路径到 onFilePath 通道，供后端监听
	onFilePath <- filePath
}
