// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// tailscale.go 负责 Tailscale Android 客户端的应用初始化、日志系统、平台信息获取等核心功能，涵盖与 Android/ChromeOS 平台适配、日志采集、系统策略注册等关键环节。
package libtailscale

import (
	"context"       // 用于管理后端生命周期和异步任务
	"log"           // 标准日志库，部分日志会重定向到远程
	"net/http"      // 远程日志上传使用 HTTP 协议
	"path/filepath" // 日志文件路径拼接
	"runtime/debug" // panic 时打印堆栈
	"time"          // 日志刷新、定时任务

	"tailscale.com/health"            // 健康状态跟踪
	"tailscale.com/logpolicy"         // 日志策略配置
	"tailscale.com/logtail"           // 日志采集与上传
	"tailscale.com/logtail/filch"     // 日志本地缓冲
	"tailscale.com/net/netmon"        // 网络接口监控
	"tailscale.com/types/logger"      // 日志接口类型
	"tailscale.com/types/logid"       // 日志唯一标识
	"tailscale.com/util/clientmetric" // 客户端指标采集
	"tailscale.com/util/syspolicy"    // 系统策略注册
)

// defaultMTU 默认 MTU，1280 兼容 IPv6，参考 wgengine/userspace.go。
const defaultMTU = 1280 // minimalMTU from wgengine/userspace.go

// 应用内持久化配置键，分别用于日志ID、登录方式、自定义登录服务器。
const (
	logPrefKey               = "privatelogid"
	loginMethodPrefKey       = "loginmethod"
	customLoginServerPrefKey = "customloginserver"
)

// newApp 创建并初始化 App 实例，完成数据目录、文件根目录、上下文、状态存储、策略注册、网络接口监控等初始化。
// dataDir: 应用数据目录，存储本地状态。
// directFileRoot: 直连文件根目录。
// appCtx: 平台相关上下文，封装 Android 侧接口。
// 返回 Application 接口实例。
func newApp(dataDir, directFileRoot string, appCtx AppContext) Application {
	// 构造 App 结构体，初始化关键字段。
	a := &App{
		directFileRoot:   directFileRoot,         // 文件根目录
		dataDir:          dataDir,                // 数据目录
		appCtx:           appCtx,                 // 平台上下文
		backendRestartCh: make(chan struct{}, 1), // 后端重启信号通道
	}
	// ready 用于同步后端和前端初始化，Add(2) 表示需等待两个事件。
	a.ready.Add(2)

	// 初始化状态存储，封装 Android 侧持久化。
	a.store = newStateStore(a.appCtx)
	// 注册系统策略处理器，适配企业策略。
	a.policyStore = &syspolicyHandler{a: a}
	// 注册网络接口获取器，便于 netmon 监控网络变化。
	netmon.RegisterInterfaceGetter(a.getInterfaces)
	// 注册系统策略处理器到全局。
	syspolicy.RegisterHandler(a.policyStore)
	// 启动文件操作变更监听，便于同步文件状态。
	go a.watchFileOpsChanges()

	// 启动后端主循环，负责核心业务逻辑。
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic in runBackend %s: %s", p, debug.Stack())
				panic(p)
			}
		}()

		ctx := context.Background()
		if err := a.runBackend(ctx); err != nil {
			fatalErr(err)
		}
	}()

	return a
}

// fatalErr 处理致命错误，当前仅打印日志，后续可扩展为 UI 提示。
func fatalErr(err error) {
	// TODO: expose in UI.
	log.Printf("fatal error: %v", err)
}

// osVersion 返回 Android 系统版本号，若未集成 Google Play 服务则追加 " [nogoogle]"。
// 设计说明：通过 appCtx 调用 Android 侧实现，panic 仅用于开发期调试。
func (a *App) osVersion() string {
	version, err := a.appCtx.GetOSVersion()
	if err != nil {
		panic(err)
	}
	return version
}

// modelName 返回设备制造商+型号，便于日志和问题定位。
// 设计说明：通过 appCtx 调用 Android 侧实现。
func (a *App) modelName() string {
	model, err := a.appCtx.GetModelName()
	if err != nil {
		panic(err)
	}
	return model
}

// isChromeOS 判断当前设备是否为 ChromeOS。
// 设计说明：部分网络/DNS 行为在 ChromeOS 下需特殊处理。
func (a *App) isChromeOS() bool {
	isChromeOS, err := a.appCtx.IsChromeOS()
	if err != nil {
		panic(err)
	}
	return isChromeOS
}

// setupLogs 初始化远程日志系统，配置日志采集、缓冲、上传、健康监控等。
// logDir: 日志文件目录。
// logID: 日志唯一标识。
// logf: 日志回调函数。
// health: 健康状态跟踪器。
func (b *backend) setupLogs(logDir string, logID logid.PrivateID, logf logger.Logf, health *health.Tracker) {
	// 必须先初始化 netMon，否则日志采集依赖网络状态。
	if b.netMon == nil {
		panic("netMon must be created prior to SetupLogs")
	}
	// 配置远程日志上传的 HTTP 传输层，支持健康监控和网络切换。
	transport := logpolicy.NewLogtailTransport(logtail.DefaultHost, b.netMon, health, log.Printf)

	// 构造日志采集配置，包含唯一标识、缓冲、压缩、指标采集等。
	logcfg := logtail.Config{
		Collection:          logtail.CollectionNode,
		PrivateID:           logID,
		Stderr:              log.Writer(),
		MetricsDelta:        clientmetric.EncodeLogTailMetricsDelta,
		IncludeProcID:       true,
		IncludeProcSequence: true,
		HTTPC:               &http.Client{Transport: transport},
		CompressLogs:        true,
	}
	// 日志刷新延迟，2 分钟批量上传，兼顾实时性与流量消耗。
	logcfg.FlushDelayFn = func() time.Duration { return 2 * time.Minute }

	// 配置本地日志缓冲，filch 支持断网时日志持久化。
	filchOpts := filch.Options{
		ReplaceStderr: true,
	}

	var filchErr error
	if logDir != "" {
		logPath := filepath.Join(logDir, "ipn.log.")
		logcfg.Buffer, filchErr = filch.New(logPath, filchOpts)
	}

	// 创建远程日志 Logger，所有 log.Printf 均重定向到远程。
	b.logger = logtail.NewLogger(logcfg, logf)

	// 设置全局日志输出，便于调试和问题定位。
	log.SetFlags(0)
	log.SetOutput(b.logger)

	log.Printf("goSetupLogs: success")

	if logDir == "" {
		log.Printf("SetupLogs: no logDir, storing logs in memory")
	}
	if filchErr != nil {
		log.Printf("SetupLogs: filch setup failed: %v", filchErr)
	}

	// 启动日志通道监听，onLog 为全局日志通道。
	go func() {
		for {
			select {
			case logstr := <-onLog:
				b.logger.Logf(logstr)
			}
		}
	}()
}

// Close 关闭 App，当前无端口转发相关逻辑，预留接口。
func (a *App) Close() {
	// 已无端口转发相关逻辑
}
