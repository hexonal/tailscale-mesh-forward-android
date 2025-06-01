// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// notifier.go 负责 Tailscale Android 客户端的通知监听与分发，封装通知回调、取消管理等，适配 Android 平台的异步事件处理需求。
package libtailscale

import (
	"context"       // 用于通知监听的上下文管理，实现取消与超时
	"encoding/json" // 通知序列化为 JSON，便于跨语言传递
	"log"           // 日志输出，便于调试和异常追踪
	"runtime/debug" // panic 时打印堆栈，便于定位问题

	"tailscale.com/ipn" // 通知结构体与选项定义
)

// WatchNotifications 启动通知监听，异步接收并分发 Tailscale 后端的通知。
// mask: 通知掩码，指定感兴趣的通知类型。
// cb: 通知回调接口，负责处理每条通知。
// 返回 NotificationManager，可用于后续取消监听。
func (app *App) WatchNotifications(mask int, cb NotificationCallback) NotificationManager {
	// 等待 App 初始化完成，确保后端已就绪。
	app.ready.Wait()

	// 创建可取消的上下文，便于后续主动停止监听。
	ctx, cancel := context.WithCancel(context.Background())
	// 启动后端通知监听，采用 goroutine 异步处理，避免阻塞主线程。
	go app.backend.WatchNotifications(ctx, ipn.NotifyWatchOpt(mask), func() {}, func(notify *ipn.Notify) bool {
		// 捕获 panic，防止回调异常导致 goroutine 泄漏。
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic in WatchNotifications %s: %s", p, debug.Stack())
				panic(p)
			}
		}()

		// 将通知结构体序列化为 JSON，便于 Android 侧或其他语言处理。
		b, err := json.Marshal(notify)
		if err != nil {
			log.Printf("error: WatchNotifications: marshal notify: %s", err)
			return true // 返回 true 继续监听，避免因单条错误中断整体监听
		}
		// 调用回调接口处理通知，若处理失败记录日志。
		err = cb.OnNotify(b)
		if err != nil {
			log.Printf("error: WatchNotifications: OnNotify: %s", err)
			return true // 返回 true 继续监听
		}
		return true // 始终返回 true，保持监听活跃
	})
	// 返回通知管理器，封装取消函数。
	return &notificationManager{cancel}
}

// notificationManager 封装通知监听的取消逻辑，便于外部主动停止监听。
type notificationManager struct {
	cancel func() // 取消函数，调用后终止监听 goroutine
}

// Stop 主动停止通知监听，释放资源。
func (nm *notificationManager) Stop() {
	nm.cancel()
}
