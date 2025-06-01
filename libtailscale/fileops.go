// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// libtailscale 包为 Android 平台 Tailscale 客户端提供文件操作相关的适配。
package libtailscale

import (
	"fmt"
	"io"
)

// AndroidFileOps 实现 ShareFileHelper 接口，封装 Android 侧 SAF 文件操作。
type AndroidFileOps struct {
	// helper 持有 Android 侧实现的 ShareFileHelper 实例。
	helper ShareFileHelper
}

// NewAndroidFileOps 创建 AndroidFileOps 实例。
// helper: Android 侧 ShareFileHelper 实例。
// 返回 *AndroidFileOps。
func NewAndroidFileOps(helper ShareFileHelper) *AndroidFileOps {
	// 直接封装 helper
	return &AndroidFileOps{helper: helper}
}

// OpenFileURI 获取文件的 SAF URI。
// filename: 文件名。
// 返回 SAF URI 字符串。
func (ops *AndroidFileOps) OpenFileURI(filename string) string {
	// 调用 Android 侧 helper 获取 URI
	return ops.helper.OpenFileURI(filename)
}

// OpenFileWriter 打开文件写入流。
// filename: 文件名。
// 返回 io.WriteCloser、SAF URI、错误。
func (ops *AndroidFileOps) OpenFileWriter(filename string) (io.WriteCloser, string, error) {
	// 获取文件 URI
	uri := ops.helper.OpenFileURI(filename)
	// 获取写入流
	outputStream := ops.helper.OpenFileWriter(filename)
	if outputStream == nil {
		// 打开失败，返回错误
		return nil, uri, fmt.Errorf("failed to open SAF output stream for %s", filename)
	}
	// 返回写入流和 URI
	return outputStream, uri, nil
}

// RenamePartialFile 重命名部分文件。
// partialUri: 原始部分文件 URI。
// targetDirUri: 目标目录 URI。
// targetName: 目标文件名。
// 返回新文件 URI 和错误。
func (ops *AndroidFileOps) RenamePartialFile(partialUri, targetDirUri, targetName string) (string, error) {
	// 调用 Android 侧 helper 重命名
	newURI := ops.helper.RenamePartialFile(partialUri, targetDirUri, targetName)
	if newURI == "" {
		// 重命名失败
		return "", fmt.Errorf("failed to rename partial file via SAF")
	}
	// 返回新 URI
	return newURI, nil
}
