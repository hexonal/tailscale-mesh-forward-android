// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// store.go 负责 Tailscale Android 客户端的本地持久化存储，封装与 Android EncryptedSharedPreferences 的交互，支持状态、字符串、布尔值等多种类型安全存取，兼容加密与平台安全特性。
package libtailscale

import (
	"encoding/base64" // 用于二进制数据与字符串互转，便于安全存储

	"tailscale.com/ipn" // 状态键与错误定义
)

// stateStore 封装 Android 侧加密持久化存储接口，负责所有本地状态的安全读写。
// 设计说明：通过 AppContext 间接调用 Android EncryptedSharedPreferences，保证数据加密与隔离。
type stateStore struct {
	// appCtx 是全局 Android 应用上下文，负责实际的存储操作。
	appCtx AppContext
}

// newStateStore 创建 stateStore 实例，注入平台上下文。
func newStateStore(appCtx AppContext) *stateStore {
	return &stateStore{
		appCtx: appCtx,
	}
}

// prefKeyFor 生成状态键的持久化前缀，避免与其他存储冲突。
func prefKeyFor(id ipn.StateKey) string {
	return "statestore-" + string(id)
}

// ReadString 读取字符串类型的持久化数据，若不存在返回默认值。
// key: 存储键。
// def: 默认值。
// 返回：实际读取到的字符串和错误。
func (s *stateStore) ReadString(key string, def string) (string, error) {
	// 读取原始数据，可能为 nil
	data, err := s.read(key)
	if err != nil {
		return def, err
	}
	if data == nil {
		return def, nil
	}
	return string(data), nil
}

// WriteString 写入字符串类型的持久化数据。
// key: 存储键。
// val: 待写入字符串。
func (s *stateStore) WriteString(key string, val string) error {
	return s.write(key, []byte(val))
}

// ReadBool 读取布尔类型的持久化数据，若不存在返回默认值。
// key: 存储键。
// def: 默认值。
// 返回：实际读取到的布尔值和错误。
func (s *stateStore) ReadBool(key string, def bool) (bool, error) {
	data, err := s.read(key)
	if err != nil {
		return def, err
	}
	if data == nil {
		return def, nil
	}
	return string(data) == "true", nil
}

// WriteBool 写入布尔类型的持久化数据。
// key: 存储键。
// val: 待写入布尔值。
func (s *stateStore) WriteBool(key string, val bool) error {
	data := []byte("false")
	if val {
		data = []byte("true")
	}
	return s.write(key, data)
}

// ReadState 读取二进制状态数据，常用于 Tailscale 状态同步。
// id: 状态键。
// 返回：二进制数据和错误。
func (s *stateStore) ReadState(id ipn.StateKey) ([]byte, error) {
	state, err := s.read(prefKeyFor(id))
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, ipn.ErrStateNotExist
	}
	return state, nil
}

// WriteState 写入二进制状态数据。
// id: 状态键。
// bs: 待写入的二进制数据。
func (s *stateStore) WriteState(id ipn.StateKey, bs []byte) error {
	prefKey := prefKeyFor(id)
	return s.write(prefKey, bs)
}

// read 从加密存储读取数据，自动解码 base64。
// key: 存储键。
// 返回：原始二进制数据和错误。
func (s *stateStore) read(key string) ([]byte, error) {
	// 调用 Android 侧解密接口，返回 base64 字符串
	b64, err := s.appCtx.DecryptFromPref(key)
	if err != nil {
		return nil, err
	}
	if b64 == "" {
		return nil, nil
	}
	// 解码 base64，恢复原始数据
	return base64.RawStdEncoding.DecodeString(b64)
}

// write 写入加密存储，自动编码为 base64。
// key: 存储键。
// value: 原始二进制数据。
func (s *stateStore) write(key string, value []byte) error {
	// 编码为 base64，便于字符串安全存储
	bs64 := base64.RawStdEncoding.EncodeToString(value)
	return s.appCtx.EncryptToPref(key, bs64)
}
