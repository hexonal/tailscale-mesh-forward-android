// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package libtailscale

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"tailscale.com/drive/driveimpl"
	_ "tailscale.com/feature/condregister"
	"tailscale.com/feature/taildrop"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnauth"
	"tailscale.com/ipn/ipnlocal"
	"tailscale.com/ipn/localapi"
	"tailscale.com/logtail"
	"tailscale.com/net/dns"
	"tailscale.com/net/netmon"
	"tailscale.com/net/netns"
	"tailscale.com/net/tsdial"
	"tailscale.com/paths"
	"tailscale.com/tsd"
	"tailscale.com/types/logger"
	"tailscale.com/types/logid"
	"tailscale.com/types/netmap"
	"tailscale.com/util/eventbus"
	"tailscale.com/wgengine"
	"tailscale.com/wgengine/netstack"
	"tailscale.com/wgengine/router"
)

type App struct {
	dataDir string

	// passes along SAF file information for the taildrop manager
	directFileRoot  string
	shareFileHelper ShareFileHelper

	// appCtx is a global reference to the com.tailscale.ipn.App instance.
	appCtx AppContext

	store             *stateStore
	policyStore       *syspolicyHandler
	logIDPublicAtomic atomic.Pointer[logid.PublicID]

	localAPIHandler http.Handler
	backend         *ipnlocal.LocalBackend
	ready           sync.WaitGroup
	backendMu       sync.Mutex

	backendRestartCh chan struct{}
}

// start 启动 Tailscale 应用，初始化日志、环境变量，并返回 Application 实例。
// dataDir: 数据目录路径。
// directFileRoot: SAF 文件根目录。
// appCtx: Android App 上下文。
// 返回 Application 实例。
func start(dataDir, directFileRoot string, appCtx AppContext) Application {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in Start %s: %s", p, debug.Stack())
			panic(p)
		}
	}()

	// 初始化日志系统
	initLogging(appCtx)
	// 设置 XDG_CACHE_HOME 环境变量，保证 os.UserCacheDir 可用
	if _, exists := os.LookupEnv("XDG_CACHE_HOME"); !exists {
		cachePath := filepath.Join(dataDir, "cache")
		os.Setenv("XDG_CACHE_HOME", cachePath)
	}
	// 设置 XDG_CONFIG_HOME 环境变量，保证 os.UserConfigDir 可用
	if _, exists := os.LookupEnv("XDG_CONFIG_HOME"); !exists {
		cfgPath := filepath.Join(dataDir, "config")
		os.Setenv("XDG_CONFIG_HOME", cfgPath)
	}
	// 设置 HOME 环境变量，保证 os.UserHomeDir 可用
	if _, exists := os.LookupEnv("HOME"); !exists {
		os.Setenv("HOME", dataDir)
	}

	// 创建并返回 App 实例
	return newApp(dataDir, directFileRoot, appCtx)
}

type backend struct {
	engine     wgengine.Engine
	backend    *ipnlocal.LocalBackend
	sys        *tsd.System
	devices    *multiTUN
	settings   settingsFunc
	lastCfg    *router.Config
	lastDNSCfg *dns.OSConfig
	netMon     *netmon.Monitor

	logIDPublic logid.PublicID
	logger      *logtail.Logger

	bus *eventbus.Bus

	// avoidEmptyDNS controls whether to use fallback nameservers
	// when no nameservers are provided by Tailscale.
	avoidEmptyDNS bool

	appCtx AppContext
}

type settingsFunc func(*router.Config, *dns.OSConfig) error

// runBackend 持续运行后端主循环，监听重启信号并重启后端。
// ctx: 上下文，用于取消操作。
// 返回错误信息（如有）。
func (a *App) runBackend(ctx context.Context) error {
	for {
		// 启动一次后端主循环
		err := a.runBackendOnce(ctx)
		if err != nil {
			log.Printf("runBackendOnce error: %v", err)
		}

		// 等待重启信号
		<-a.backendRestartCh
	}
}

// runBackendOnce 启动一次后端服务，处理 VPN、通知、代理等主循环。
// ctx: 上下文。
// 返回错误信息（如有）。
func (a *App) runBackendOnce(ctx context.Context) error {
	log.Printf("runBackendOnce: start")
	// 检查是否有重启信号提前到达
	select {
	case <-a.backendRestartCh:
		log.Printf("runBackendOnce: received backendRestartCh before start")
	default:
	}

	// 设置全局共享目录
	paths.AppSharedDir.Store(a.dataDir)
	// 设置主机信息
	hostinfo.SetOSVersion(a.osVersion())
	hostinfo.SetPackage(a.appCtx.GetInstallSource())
	deviceModel := a.modelName()
	if a.isChromeOS() {
		deviceModel = "ChromeOS: " + deviceModel
	}
	hostinfo.SetDeviceModel(deviceModel)

	// 定义配置对结构体
	type configPair struct {
		rcfg *router.Config
		dcfg *dns.OSConfig
	}
	// 配置通道
	configs := make(chan configPair)
	// 配置错误通道
	configErrs := make(chan error)
	log.Printf("runBackendOnce: before newBackend")
	// 创建后端实例
	b, err := a.newBackend(a.dataDir, a.appCtx, a.store, func(rcfg *router.Config, dcfg *dns.OSConfig) error {
		if rcfg == nil {
			return nil
		}
		// 发送配置到 configs 通道
		configs <- configPair{rcfg, dcfg}
		// 等待配置处理结果
		return <-configErrs
	})
	if err != nil {
		log.Printf("runBackendOnce: newBackend error: %v", err)
		return err
	}
	// 存储日志公钥
	a.logIDPublicAtomic.Store(&b.logIDPublic)
	// 绑定后端
	a.backend = b.backend
	// 退出时关闭 TUN 设备
	defer b.CloseTUNs()

	// 创建本地 API 处理器
	h := localapi.NewHandler(ipnauth.Self, b.backend, log.Printf, *a.logIDPublicAtomic.Load())
	h.PermitRead = true
	h.PermitWrite = true
	a.localAPIHandler = h

	// 标记 ready 完成
	a.ready.Done()

	// ChromeOS 兼容 DNS
	b.avoidEmptyDNS = a.isChromeOS()

	// 定义主循环变量
	var (
		cfg        configPair
		state      ipn.State
		networkMap *netmap.NetworkMap
	)

	// 状态通道
	stateCh := make(chan ipn.State)
	// 网络映射通道
	netmapCh := make(chan *netmap.NetworkMap)
	log.Printf("runBackendOnce: starting WatchNotifications goroutine")
	// 启动通知监听协程
	go b.backend.WatchNotifications(ctx, ipn.NotifyInitialNetMap|ipn.NotifyInitialPrefs|ipn.NotifyInitialState, func() {}, func(notify *ipn.Notify) bool {
		if notify.State != nil {
			stateCh <- *notify.State
		}
		if notify.NetMap != nil {
			netmapCh <- notify.NetMap
		}
		if notify.BrowseToURL != nil && *notify.BrowseToURL != "" {
			log.Printf("[TEST-FLINK] 【DEBUG】收到 authURL: %s", *notify.BrowseToURL)
		}
		return true
	})

	log.Printf("runBackendOnce: entering main select loop")
	for {
		select {
		case s := <-stateCh:
			// 收到状态变更
			log.Printf("r[TEST-FLINK] unBackendOnce: received stateCh: %v", s)
			state = s
			// VPN 启动后，配置有变化则更新 TUN
			if state >= ipn.Starting && vpnService.service != nil && b.isConfigNonNilAndDifferent(cfg.rcfg, cfg.dcfg) {
				log.Printf("[TEST-FLINK] runBackendOnce: updating TUN after stateCh")
				if err := b.updateTUN(cfg.rcfg, cfg.dcfg); err != nil {
					if errors.Is(err, errMultipleUsers) {
						log.Printf("[TEST-FLINK] runBackendOnce: multiple users error: %v", err)
					}
					a.closeVpnService(err, b)
				}
			}
		case n := <-netmapCh:
			// 收到网络映射变更
			log.Printf("[TEST-FLINK] runBackendOnce: received netmapCh, networkMap: %+v", n)
			networkMap = n
		case c := <-configs:
			// 收到新配置
			log.Printf("[TEST-FLINK] runBackendOnce: received configs")
			cfg = c
			if vpnService.service == nil || !b.isConfigNonNilAndDifferent(cfg.rcfg, cfg.dcfg) {
				log.Printf("[TEST-FLINK] runBackendOnce: config not changed or vpnService nil")
				configErrs <- nil
				break
			}
			log.Printf("[TEST-FLINK] runBackendOnce: updating TUN after configs")
			configErrs <- b.updateTUN(cfg.rcfg, cfg.dcfg)
		case s := <-onVPNRequested:
			// 收到 VPN 启动请求
			log.Printf("[TEST-FLINK] runBackendOnce: received onVPNRequested")
			if vpnService.service != nil && vpnService.service.ID() == s.ID() {
				log.Printf("runBackendOnce: vpnService already set, skipping")
				break
			}
			// 设置 Android Protect 回调
			netns.SetAndroidProtectFunc(func(fd int) error {
				if !s.Protect(int32(fd)) {
					log.Printf("[TEST-FLINK] [unexpected] VpnService.protect(%d) returned false", fd)
				}
				return nil
			})
			log.Printf("[TEST-FLINK] onVPNRequested: rebind required")
			b.backend.DebugRebind()
			vpnService.service = s
			if networkMap != nil {
				log.Printf("[TEST-FLINK] onVPNRequested: networkMap present")
				// TODO: 这里可扩展
			}
			if state >= ipn.Starting && b.isConfigNonNilAndDifferent(cfg.rcfg, cfg.dcfg) {
				log.Printf("[TEST-FLINK] onVPNRequested: updating TUN after VPN requested")
				if err := b.updateTUN(cfg.rcfg, cfg.dcfg); err != nil {
					a.closeVpnService(err, b)
				}
			}
		case s := <-onDisconnect:
			// 收到 VPN 断开请求
			log.Printf("[TEST-FLINK] runBackendOnce: received onDisconnect")
			b.CloseTUNs()
			if vpnService.service != nil && vpnService.service.ID() == s.ID() {
				log.Printf("[TEST-FLINK] runBackendOnce: disconnecting vpnService")
				netns.SetAndroidProtectFunc(nil)
				vpnService.service = nil
			}
			// 停止代理服务
			log.Printf("[TEST-FLINK] runBackendOnce: stopping proxyService")
			stopProxyService()
		case i := <-onDNSConfigChanged:
			// 收到 DNS 配置变更
			log.Printf("[TEST-FLINK] runBackendOnce: received onDNSConfigChanged: %s", i)
			go b.NetworkChanged(i)
		}
	}
}

// newBackend 创建并初始化 backend 实例，配置网络、日志、TUN 设备等。
// dataDir: 数据目录。
// appCtx: Android App 上下文。
// store: 状态存储。
// settings: 路由和 DNS 配置回调。
// 返回 backend 实例和错误信息。
func (a *App) newBackend(dataDir string, appCtx AppContext, store *stateStore,
	settings settingsFunc) (*backend, error) {

	sys := new(tsd.System)
	sys.Set(store)

	logf := logger.RusagePrefixLog(log.Printf)
	b := &backend{
		devices:  newTUNDevices(),
		settings: settings,
		appCtx:   appCtx,
		bus:      eventbus.New(),
	}

	var logID logid.PrivateID
	logID.UnmarshalText([]byte("dead0000dead0000dead0000dead0000dead0000dead0000dead0000dead0000"))
	storedLogID, err := store.read(logPrefKey)
	// In all failure cases we ignore any errors and continue with the dead value above.
	if err != nil || storedLogID == nil {
		// Read failed or there was no previous log id.
		newLogID, err := logid.NewPrivateID()
		if err == nil {
			logID = newLogID
			enc, err := newLogID.MarshalText()
			if err == nil {
				store.write(logPrefKey, enc)
			}
		}
	} else {
		logID.UnmarshalText([]byte(storedLogID))
	}

	netMon, err := netmon.New(b.bus, logf)
	if err != nil {
		log.Printf("netmon.New: %w", err)
	}
	b.netMon = netMon
	b.setupLogs(dataDir, logID, logf, sys.HealthTracker())
	dialer := new(tsdial.Dialer)
	vf := &VPNFacade{
		SetBoth:           b.setCfg,
		GetBaseConfigFunc: b.getDNSBaseConfig,
	}
	engine, err := wgengine.NewUserspaceEngine(logf, wgengine.Config{
		Tun:            b.devices,
		Router:         vf,
		DNS:            vf,
		ReconfigureVPN: vf.ReconfigureVPN,
		Dialer:         dialer,
		SetSubsystem:   sys.Set,
		NetMon:         b.netMon,
		HealthTracker:  sys.HealthTracker(),
		Metrics:        sys.UserMetricsRegistry(),
		DriveForLocal:  driveimpl.NewFileSystemForLocal(logf),
	})
	if err != nil {
		return nil, fmt.Errorf("[TEST-FLINK] runBackend: NewUserspaceEngine: %v", err)
	}
	sys.Set(engine)
	b.logIDPublic = logID.Public()
	ns, err := netstack.Create(logf, sys.Tun.Get(), engine, sys.MagicSock.Get(), dialer, sys.DNSManager.Get(), sys.ProxyMapper())
	if err != nil {
		return nil, fmt.Errorf("netstack.Create: %w", err)
	}
	sys.Set(ns)
	ns.ProcessLocalIPs = false // let Android kernel handle it; VpnBuilder sets this up
	ns.ProcessSubnets = true   // for Android-being-an-exit-node support
	sys.NetstackRouter.Set(true)
	if w, ok := sys.Tun.GetOK(); ok {
		w.Start()
	}
	lb, err := ipnlocal.NewLocalBackend(logf, logID.Public(), sys, 0)
	if ext, ok := ipnlocal.GetExt[*taildrop.Extension](lb); ok {
		ext.SetFileOps(NewAndroidFileOps(a.shareFileHelper))
		ext.SetDirectFileRoot(a.directFileRoot)
	}

	if err := ns.Start(lb); err != nil {
		return nil, fmt.Errorf("startNetstack: %w", err)
	}
	if b.logger != nil {
		lb.SetLogFlusher(b.logger.StartFlush)
	}
	b.engine = engine
	b.backend = lb
	b.sys = sys
	go func() {
		// 直接设置自定义 Headscale 服务器
		customHeadscaleURL := "https://headscal.myjl.top" // TODO: 替换为你的 Headscale 地址

		var opts ipn.Options
		log.Printf("[TEST-FLINK] Starting with custom Headscale server: %s", customHeadscaleURL)
		prefs := ipn.NewPrefs()
		prefs.ControlURL = customHeadscaleURL
		prefs.WantRunning = true
		opts.UpdatePrefs = prefs

		err := lb.Start(opts)
		if err != nil {
			log.Printf("[TEST-FLINK] Failed to start LocalBackend, panicking: %s", err)
			panic(err)
		}
		a.ready.Done()
	}()
	return b, nil
}

// watchFileOpsChanges 监听文件操作相关的全局通道，动态更新 directFileRoot 和 shareFileHelper。
func (a *App) watchFileOpsChanges() {
	for {
		select {
		case newPath := <-onFilePath:
			log.Printf("Got new directFileRoot")
			a.directFileRoot = newPath
			a.backendRestartCh <- struct{}{}
		case helper := <-onShareFileHelper:
			log.Printf("Got shareFIleHelper")
			a.shareFileHelper = helper
			a.backendRestartCh <- struct{}{}
		}
	}
}

// isConfigNonNilAndDifferent 判断路由和 DNS 配置是否有变化且不为 nil。
// rcfg: 路由配置。
// dcfg: DNS 配置。
// 返回 true 表示有变化。
func (b *backend) isConfigNonNilAndDifferent(rcfg *router.Config, dcfg *dns.OSConfig) bool {
	if reflect.DeepEqual(rcfg, b.lastCfg) && reflect.DeepEqual(dcfg, b.lastDNSCfg) {
		b.logger.Logf("isConfigNonNilAndDifferent: no change to Routes or DNS, ignore")
		return false
	}
	return rcfg != nil
}

// closeVpnService 关闭 VPN 服务，清理配置并断开连接。
// err: 关闭原因错误。
// b: backend 实例。
func (a *App) closeVpnService(err error, b *backend) {
	log.Printf("[TEST-FLINK] VPN update failed: %v", err)

	mp := new(ipn.MaskedPrefs)
	mp.WantRunning = false
	mp.WantRunningSet = true

	if _, localApiErr := a.EditPrefs(*mp); localApiErr != nil {
		log.Printf("[TEST-FLINK] localapi edit prefs error %v", localApiErr)
	}

	b.lastCfg = nil
	b.CloseTUNs()

	vpnService.service.DisconnectVPN()
	vpnService.service = nil
}