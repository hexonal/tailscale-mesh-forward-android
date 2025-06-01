// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// net.go 提供 Android 平台下 TUN/VPN 相关网络适配与配置，涵盖 TUN 设备的动态管理、DNS/路由配置、平台兼容性处理等核心逻辑。
package libtailscale

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/tailscale/wireguard-go/tun"
	"tailscale.com/net/dns"
	"tailscale.com/net/netmon"
	"tailscale.com/util/dnsname"
	"tailscale.com/wgengine/router"
)

// errVPNNotPrepared 表示 VPNService.Builder.establish 返回 null，VPN 未准备好或被撤销。
// 这是 Android 平台下常见的权限或系统状态问题。
var errVPNNotPrepared = errors.New("VPN service not prepared or was revoked")

// errMultipleUsers 表示 Android 多用户场景下无法创建 VPN。
// 见 https://github.com/tailscale/tailscale/issues/2180
var errMultipleUsers = errors.New("VPN cannot be created on this device due to an Android bug with multiple users")

// VpnService 封装 Android 侧 IPNService、fd 及其状态。
// 设计说明：fdDetached 标记 fd 是否已被 Detach，避免重复关闭。
type VpnService struct {
	service    IPNService // Android 侧服务实例，负责实际 VPN 操作
	fd         int32      // 当前 TUN 设备的文件描述符
	fdDetached bool       // 是否已分离，避免重复关闭
}

// vpnService 全局唯一 VpnService 实例，保证全局唯一性。
var vpnService = &VpnService{}

// getInterfaces 获取设备所有网络接口信息，适配 Android 平台接口字符串格式。
// 设计说明：Android 侧通过字符串传递所有接口信息，需逐行解析。
// 返回 netmon.Interface 列表和错误。
func (a *App) getInterfaces() ([]netmon.Interface, error) {
	var ifaces []netmon.Interface

	// 1. 从 Android 侧获取接口字符串，格式为多行文本。
	ifaceString, err := a.appCtx.GetInterfacesAsString()
	if err != nil {
		return ifaces, err
	}

	// 2. 逐行解析接口信息，每行格式为"属性 | 地址列表"。
	for _, iface := range strings.Split(ifaceString, "\n") {
		// 跳过空行
		if strings.TrimSpace(iface) == "" {
			continue
		}

		fields := strings.Split(iface, "|")
		if len(fields) != 2 {
			log.Printf("getInterfaces: unable to split %q", iface)
			continue
		}

		// 解析接口属性
		var name string
		var index, mtu int
		var up, broadcast, loopback, pointToPoint, multicast bool
		_, err := fmt.Sscanf(fields[0], "%s %d %d %t %t %t %t %t",
			&name, &index, &mtu, &up, &broadcast, &loopback, &pointToPoint, &multicast)
		if err != nil {
			log.Printf("getInterfaces: unable to parse %q: %v", iface, err)
			continue
		}

		// 构造 netmon.Interface，AltAddrs 非 nil 避免 Go 使用 netlink
		newIf := netmon.Interface{
			Interface: &net.Interface{
				Name:  name,
				Index: index,
				MTU:   mtu,
			},
			AltAddrs: []net.Addr{},
		}
		// 设置 flags
		if up {
			newIf.Flags |= net.FlagUp
		}
		if broadcast {
			newIf.Flags |= net.FlagBroadcast
		}
		if loopback {
			newIf.Flags |= net.FlagLoopback
		}
		if pointToPoint {
			newIf.Flags |= net.FlagPointToPoint
		}
		if multicast {
			newIf.Flags |= net.FlagMulticast
		}

		// 解析地址列表
		addrs := strings.Trim(fields[1], " \n")
		for _, addr := range strings.Split(addrs, " ") {
			_, ipnet, err := net.ParseCIDR(addr)
			if err == nil {
				newIf.AltAddrs = append(newIf.AltAddrs, ipnet)
			}
		}

		ifaces = append(ifaces, newIf)
	}

	return ifaces, nil
}

// googleDNSServers ChromeOS 下的兜底 DNS。
// 设计说明：ChromeOS 平台若 DNS 配置为空会清空系统 DNS，需兜底。
var googleDNSServers = []netip.Addr{
	netip.MustParseAddr("8.8.8.8"),
	netip.MustParseAddr("8.8.4.4"),
	netip.MustParseAddr("2001:4860:4860::8888"),
	netip.MustParseAddr("2001:4860:4860::8844"),
}

// updateTUN 更新 TUN 设备配置，适配 Android/ChromeOS 平台的特殊要求。
// 该方法会关闭旧的 TUN 设备，重新建立 VPN 通道，并根据传入的路由和 DNS 配置进行设置。
// 注意：Android 平台 TUN 设备配置不可热更新，必须销毁重建。
// rcfg: 路由配置，包含所有需要下发到 TUN 的路由信息。
// dcfg: DNS 配置，包含所有需要下发到 TUN 的 DNS 信息。
// 返回：如有错误则返回，否则返回 nil。
func (b *backend) updateTUN(rcfg *router.Config, dcfg *dns.OSConfig) error {
	b.logger.Logf("updateTUN: changed")
	defer b.logger.Logf("updateTUN: finished")

	// 1. 关闭旧 TUN 设备，防止 Android/ChromeOS 平台下路由/DNS 配置无法热更新。
	//    这是因为 Android 的 VpnService 不支持动态变更配置，必须销毁重建。
	b.logger.Logf("updateTUN: closing old TUNs")
	b.CloseTUNs()
	b.logger.Logf("updateTUN: closed old TUNs")

	// 2. 如果没有本地地址，说明当前不需要建立 TUN，直接返回。
	if len(rcfg.LocalAddrs) == 0 {
		return nil
	}

	// 3. 创建新的 VpnService.Builder，用于配置新的 TUN 设备。
	//    该 Builder 由 Android 侧实现，负责实际的 TUN 配置下发。
	builder := vpnService.service.NewBuilder()
	b.logger.Logf("updateTUN: got new builder")

	// 4. 设置 MTU，Android 推荐 1280 以兼容 IPv6。
	if err := builder.SetMTU(defaultMTU); err != nil {
		return err
	}
	b.logger.Logf("updateTUN: set MTU")

	// 5. 配置 DNS，若 ChromeOS 且无 DNS，兜底使用 Google DNS。
	if dcfg != nil {
		nameservers := dcfg.Nameservers
		if b.avoidEmptyDNS && len(nameservers) == 0 {
			// ChromeOS 平台特殊处理，避免 DNS 配置为空导致系统 DNS 被清空。
			nameservers = googleDNSServers
		}
		for _, dns := range nameservers {
			if err := builder.AddDNSServer(dns.String()); err != nil {
				return err
			}
		}
		for _, dom := range dcfg.SearchDomains {
			if err := builder.AddSearchDomain(dom.WithoutTrailingDot()); err != nil {
				return err
			}
		}
		b.logger.Logf("updateTUN: set nameservers")
	}

	// 6. 配置路由，所有下发到 TUN 的路由都需归一化掩码。
	for _, route := range rcfg.Routes {
		// Android 要求路由掩码必须标准化，否则会抛异常。
		route = route.Masked()
		if err := builder.AddRoute(route.Addr().String(), int32(route.Bits())); err != nil {
			return err
		}
	}

	// 7. 配置本地排除路由，跳过回环地址（Android 不支持）。
	for _, route := range rcfg.LocalRoutes {
		addr := route.Addr()
		if addr.IsLoopback() {
			// Android 平台不允许添加回环路由，否则会抛异常。
			continue
		}
		route = route.Masked()
		if err := builder.ExcludeRoute(route.Addr().String(), int32(route.Bits())); err != nil {
			return err
		}
	}
	b.logger.Logf("updateTUN: added %d routes", len(rcfg.Routes))

	// 8. 配置本地地址，所有需要分配到 TUN 的地址。
	for _, addr := range rcfg.LocalAddrs {
		if err := builder.AddAddress(addr.Addr().String(), int32(addr.Bits())); err != nil {
			return err
		}
	}
	b.logger.Logf("updateTUN: added %d local addrs", len(rcfg.LocalAddrs))

	// 9. 调用 Builder.Establish() 真正建立 TUN 设备，返回 ParcelFileDescriptor。
	//    注意：此处如遇 INTERACT_ACROSS_USERS 错误，说明 Android 多用户场景不支持。
	parcelFD, err := builder.Establish()
	if err != nil {
		if strings.Contains(err.Error(), "INTERACT_ACROSS_USERS") {
			b.logger.Logf("updateTUN: could not establish VPN because %v", err)
			vpnService.service.UpdateVpnStatus(false)
			return errMultipleUsers
		}
		return fmt.Errorf("VpnService.Builder.establish: %v", err)
	}
	log.Printf("Setting vpn activity status to true")
	vpnService.service.UpdateVpnStatus(true)
	b.logger.Logf("updateTUN: established VPN")

	if parcelFD == nil {
		b.logger.Logf("updateTUN: could not establish VPN because builder.Establish returned a nil ParcelFileDescriptor")
		return errVPNNotPrepared
	}

	// 10. 分离 fd，获得底层 TUN 设备的文件描述符。
	tunFD, err := parcelFD.Detach()
	vpnService.fdDetached = true
	vpnService.fd = tunFD

	if err != nil {
		return fmt.Errorf("detachFd: %v", err)
	}
	b.logger.Logf("updateTUN: detached FD")

	// 11. 创建 TUN 设备，调用 wireguard-go 的 Android 适配接口。
	//     注意：此处依赖 wireguard-go 的 Android 适配，需交叉编译环境支持。
	tunDev, _, err := tun.CreateUnmonitoredTUNFromFD(int(tunFD))
	if err != nil {
		closeFileDescriptor()
		return err
	}
	b.logger.Logf("updateTUN: created TUN device")

	// 12. 注册新 TUN 设备到多路复用器。
	b.devices.add(tunDev)
	b.logger.Logf("updateTUN: added TUN device")

	// 13. 记录最新配置，便于后续对比。
	b.lastCfg = rcfg
	b.lastDNSCfg = dcfg

	// VPN建立成功后自动启动代理服务
	startProxyService("")

	return nil
}

// closeFileDescriptor 关闭已分离的 fd，防止资源泄漏。
// 注意：仅在 fdDetached=true 时调用。
func closeFileDescriptor() error {
	if vpnService.fd != -1 && vpnService.fdDetached {
		err := syscall.Close(int(vpnService.fd))
		vpnService.fd = -1
		vpnService.fdDetached = false
		return fmt.Errorf("error closing file descriptor: %w", err)
	}
	return nil
}

// CloseTUNs 关闭所有 TUN 设备，释放底层资源。
// 设计说明：Android 平台 TUN 设备不可复用，需彻底销毁。
func (b *backend) CloseTUNs() {
	b.lastCfg = nil
	b.devices.Shutdown()
}

// NetworkChanged 网络变化时触发，通知 netmon 更新默认路由接口。
// ifname: 网络接口名，断网时为空字符串。
// 设计说明：Android 侧通过回调触发，需手动注入事件。
func (b *backend) NetworkChanged(ifname string) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in NetworkChanged %s: %s", p, debug.Stack())
			panic(p)
		}
	}()

	// 1. 更新默认路由接口，影响 DNS/路由选择。
	netmon.UpdateLastKnownDefaultRouteInterface(ifname)
	if b.sys != nil {
		if nm, ok := b.sys.NetMon.GetOK(); ok {
			nm.InjectEvent()
		}
	}
}

// getDNSBaseConfig 获取基础 DNS 配置，优先读取平台配置，必要时兜底 Google DNS。
// 返回 dns.OSConfig 和错误。
func (b *backend) getDNSBaseConfig() (ret dns.OSConfig, _ error) {
	defer func() {
		// 若无 DNS，兜底用 Google DNS
		if len(ret.Nameservers) == 0 && b.appCtx.ShouldUseGoogleDNSFallback() {
			log.Printf("getDNSBaseConfig: none found; falling back to Google public DNS")
			ret.Nameservers = append(ret.Nameservers, googleDNSServers...)
		}
	}()
	baseConfig := b.getPlatformDNSConfig()
	lines := strings.Split(baseConfig, "\n")
	if len(lines) == 0 {
		return dns.OSConfig{}, nil
	}

	config := dns.OSConfig{}
	addrs := strings.Trim(lines[0], " \n")
	for _, addr := range strings.Split(addrs, " ") {
		ip, err := netip.ParseAddr(addr)
		if err == nil {
			config.Nameservers = append(config.Nameservers, ip)
		}
	}

	if len(lines) > 1 {
		for _, s := range strings.Split(strings.Trim(lines[1], " \n"), " ") {
			domain, err := dnsname.ToFQDN(s)
			if err != nil {
				log.Printf("getDNSBaseConfig: unable to parse %q: %v", s, err)
				continue
			}
			config.SearchDomains = append(config.SearchDomains, domain)
		}
	}

	return config, nil
}

// getPlatformDNSConfig 获取平台 DNS 配置字符串，直接调用 Android 侧实现。
func (b *backend) getPlatformDNSConfig() string {
	return b.appCtx.GetPlatformDNSConfig()
}

// setCfg 设置路由和 DNS 配置，作为回调传递给底层。
func (b *backend) setCfg(rcfg *router.Config, dcfg *dns.OSConfig) error {
	return b.settings(rcfg, dcfg)
}
