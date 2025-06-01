// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// vpnfacade.go 负责 Tailscale Android 客户端的 VPN 配置适配，封装 wgengine.Router 和 dns.OSConfigurator 双接口，支持动态路由/DNS 配置、MTU 管理、线程安全、平台兼容性等。
// 设计说明：通过 VPNFacade 统一管理 TUN 设备的网络与 DNS 配置，便于后端与平台解耦。
package libtailscale

import (
	"sync" // 互斥锁，保证多线程环境下配置一致性

	"tailscale.com/net/dns"         // DNS 配置接口与类型
	"tailscale.com/wgengine/router" // 路由配置接口与类型
)

var (
	_ router.Router      = (*VPNFacade)(nil) // 编译期接口断言，保证实现 router.Router
	_ dns.OSConfigurator = (*VPNFacade)(nil) // 编译期接口断言，保证实现 dns.OSConfigurator
)

// VPNFacade 同时实现 wgengine.Router 和 dns.OSConfigurator，
// 用于统一管理 TUN 设备的路由与 DNS 配置，支持动态重配置。
// 设计说明：SetBoth 由后端注入，便于平台自定义配置下发。
type VPNFacade struct {
	SetBoth func(rcfg *router.Config, dcfg *dns.OSConfig) error // 路由与 DNS 配置下发回调

	// GetBaseConfigFunc 可选，返回当前 DNS 配置，便于状态同步。
	// 若为 nil，GetBaseConfig 返回不支持错误。
	GetBaseConfigFunc func() (dns.OSConfig, error)

	// InitialMTU 指定 TUN 设备初始化时的 MTU，0 表示使用默认值。
	// 仅在 TUN 创建后首次生效，后续忽略。
	InitialMTU uint32

	mu        sync.Mutex     // 保护以下所有字段，保证多线程安全
	didSetMTU bool           // 是否已设置过 MTU，防止重复下发
	rcfg      *router.Config // 最近一次下发的路由配置
	dcfg      *dns.OSConfig  // 最近一次下发的 DNS 配置
}

// Up 实现 wgengine.Router 接口，Android 平台无需额外初始化，直接返回 nil。
func (vf *VPNFacade) Up() error {
	return nil // TODO: 检查所有调用方是否确实无需初始化
}

// Set 实现 wgengine.Router 接口，设置路由配置，支持 MTU 首次下发。
// rcfg: 路由配置。
func (vf *VPNFacade) Set(rcfg *router.Config) error {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	// 若配置未变，直接返回，避免重复下发
	if vf.rcfg.Equal(rcfg) {
		return nil
	}
	// 首次设置时下发 MTU
	if vf.didSetMTU == false {
		vf.didSetMTU = true
		rcfg.NewMTU = int(vf.InitialMTU)
	}
	vf.rcfg = rcfg
	return nil
}

// UpdateMagicsockPort 实现 wgengine.Router 接口，Android 端无需关心 UDP 端口，直接返回 nil。
func (vf *VPNFacade) UpdateMagicsockPort(_ uint16, _ string) error {
	return nil
}

// SetDNS 实现 dns.OSConfigurator 接口，设置 DNS 配置。
// dcfg: DNS 配置。
func (vf *VPNFacade) SetDNS(dcfg dns.OSConfig) error {
	vf.mu.Lock()
	defer vf.mu.Unlock()
	// 若配置未变，直接返回
	if vf.dcfg != nil && vf.dcfg.Equal(dcfg) {
		return nil
	}
	vf.dcfg = &dcfg
	return nil
}

// SupportsSplitDNS 实现 dns.OSConfigurator 接口，Android 端不支持 Split DNS，返回 false。
func (vf *VPNFacade) SupportsSplitDNS() bool {
	return false
}

// GetBaseConfig 实现 dns.OSConfigurator 接口，返回当前 DNS 配置。
// 若未实现 GetBaseConfigFunc，返回不支持错误。
func (vf *VPNFacade) GetBaseConfig() (dns.OSConfig, error) {
	if vf.GetBaseConfigFunc == nil {
		return dns.OSConfig{}, dns.ErrGetBaseConfigNotSupported
	}
	return vf.GetBaseConfigFunc()
}

// Close 实现 wgengine.Router 和 dns.OSConfigurator 接口，关闭时清空配置。
func (vf *VPNFacade) Close() error {
	return vf.SetBoth(nil, nil) // TODO: 检查此行为是否合理
}

// ReconfigureVPN 由后端调用，触发路由与 DNS 的重新下发。
// 设计说明：加锁保证配置一致性。
func (vf *VPNFacade) ReconfigureVPN() error {
	vf.mu.Lock()
	defer vf.mu.Unlock()

	return vf.SetBoth(vf.rcfg, vf.dcfg)
}
