// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// syspolicy_handler.go 负责 Tailscale Android 客户端的系统策略（Syspolicy）适配，实现与 Android RestrictionsManager 的集成，支持策略读取、变更回调、类型安全转换等，兼容企业/设备管理场景。
// 设计说明：通过 AppContext 间接调用 Android 侧策略接口，保证平台兼容性与安全性。
package libtailscale

import (
	"encoding/json" // 用于解析策略数组类型
	"errors"        // 错误处理
	"sync"          // 读写锁，保证回调并发安全

	"tailscale.com/util/set"       // 回调句柄集合
	"tailscale.com/util/syspolicy" // 策略接口与错误定义
)

// syspolicyHandler 是 Android 版 Tailscale 的系统策略处理器，
// 支持通过 Android RestrictionsManager 读取企业/设备策略，
// 并为主网络模块提供统一的策略读取接口。
type syspolicyHandler struct {
	a   *App                  // 应用实例，便于访问 appCtx
	mu  sync.RWMutex          // 读写锁，保护回调集合
	cbs set.HandleSet[func()] // 策略变更回调集合，支持多回调并发注册
}

// ReadString 读取字符串类型的策略值。
// key: 策略键。
// 返回：策略值和错误。
func (h *syspolicyHandler) ReadString(key string) (string, error) {
	if key == "" {
		return "", syspolicy.ErrNoSuchKey
	}
	retVal, err := h.a.appCtx.GetSyspolicyStringValue(key)
	return retVal, translateHandlerError(err)
}

// ReadBoolean 读取布尔类型的策略值。
// key: 策略键。
// 返回：策略值和错误。
func (h *syspolicyHandler) ReadBoolean(key string) (bool, error) {
	if key == "" {
		return false, syspolicy.ErrNoSuchKey
	}
	retVal, err := h.a.appCtx.GetSyspolicyBooleanValue(key)
	return retVal, translateHandlerError(err)
}

// ReadUInt64 读取 uint64 类型的策略值。
// 目前 Android 端未实现，直接返回错误。
func (h *syspolicyHandler) ReadUInt64(key string) (uint64, error) {
	if key == "" {
		return 0, syspolicy.ErrNoSuchKey
	}
	// 目前无 uint64 策略，返回未实现错误
	return 0, errors.New("ReadUInt64 is not implemented on Android")
}

// ReadStringArray 读取字符串数组类型的策略值，支持 JSON 解码。
// key: 策略键。
// 返回：字符串数组和错误。
func (h *syspolicyHandler) ReadStringArray(key string) ([]string, error) {
	if key == "" {
		return nil, syspolicy.ErrNoSuchKey
	}
	retVal, err := h.a.appCtx.GetSyspolicyStringArrayJSONValue(key)
	if err := translateHandlerError(err); err != nil {
		return nil, err
	}
	if retVal == "" {
		return nil, syspolicy.ErrNoSuchKey
	}
	var arr []string
	jsonErr := json.Unmarshal([]byte(retVal), &arr)
	if jsonErr != nil {
		return nil, jsonErr
	}
	return arr, err
}

// RegisterChangeCallback 注册策略变更回调，支持多回调并发注册。
// cb: 回调函数。
// 返回：注销函数和错误。
func (h *syspolicyHandler) RegisterChangeCallback(cb func()) (unregister func(), err error) {
	h.mu.Lock()
	handle := h.cbs.Add(cb)
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.cbs, handle)
		h.mu.Unlock()
	}, nil
}

// notifyChanged 通知所有已注册回调，异步触发，避免阻塞主线程。
func (h *syspolicyHandler) notifyChanged() {
	h.mu.RLock()
	for _, cb := range h.cbs {
		go cb()
	}
	h.mu.RUnlock()
}

// translateHandlerError 统一处理策略接口错误，兼容平台差异。
func translateHandlerError(err error) error {
	if err != nil && !errors.Is(err, syspolicy.ErrNoSuchKey) && err.Error() == syspolicy.ErrNoSuchKey.Error() {
		return syspolicy.ErrNoSuchKey
	}
	return err // may be nil or non-nil
}
