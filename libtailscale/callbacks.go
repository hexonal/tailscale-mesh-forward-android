// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// libtailscale 包为 Android 平台 Tailscale 客户端提供 Go 层核心逻辑。
package libtailscale

import (
	"sync"
)

var (
	// onVPNRequested 用于接收 VPN 连接请求的全局通道，类型为 IPNService。
	onVPNRequested = make(chan IPNService)
	// onDisconnect 用于接收 VPN 断开请求的全局通道，类型为 IPNService。
	onDisconnect = make(chan IPNService)

	// onGoogleToken 用于接收 Google ID token 的全局通道。
	onGoogleToken = make(chan string)

	// onDNSConfigChanged 用于通知网络变化、需要更新 DNS 配置的全局通道，内容为接口名。
	onDNSConfigChanged = make(chan string, 1)

	// onLog 用于接收 Android 日志的全局通道。
	onLog = make(chan string, 10)

	// onShareFileHelper 用于接收 ShareFileHelper 实例的全局通道，便于文件接收。
	onShareFileHelper = make(chan ShareFileHelper, 1)

	// onFilePath 用于接收 Taildrop SAF 路径的全局通道。
	onFilePath = make(chan string)
)

// OnDNSConfigChanged 通知 Go 层网络发生变化，需要更新 DNS 配置。
// ifname: 网络接口名，断网时为空字符串。
func OnDNSConfigChanged(ifname string) {
	// 非阻塞发送接口名到 onDNSConfigChanged 通道，通道满则丢弃
	select {
	case onDNSConfigChanged <- ifname:
		// 成功发送
	default:
		// 通道已满，丢弃本次变更
	}
}

// android 结构体用于存储全局 Android App 上下文。
var android struct {
	// mu 保护结构体所有字段的互斥锁。
	mu sync.Mutex

	// appCtx 全局 Android App context。
	appCtx AppContext
}
