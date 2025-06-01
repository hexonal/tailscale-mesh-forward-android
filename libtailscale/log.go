// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Gratefully borrowed from Gio UI https://gioui.org/ under MIT license
// log.go 提供 Android 平台下的日志重定向与适配。
package libtailscale

/*
#cgo LDFLAGS: -llog

#include <stdlib.h>
#include <android/log.h>
*/
import "C"

import (
	"bufio"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"unsafe"
)

// logLineLimit 日志单行最大长度（含换行），与 android/log.h 保持一致。
const logLineLimit = 1024

// ID 当前进程名。
var ID = filepath.Base(os.Args[0])

// logTag 用于 Android 日志的 tag。
var logTag = C.CString(ID)

// initLogging 初始化日志系统，将 Go 日志重定向到 Android logcat。
// appCtx: Android App 上下文。
func initLogging(appCtx AppContext) {
	// Android logcat 已包含时间戳，去除 Go 日志时间。
	log.SetFlags(log.Flags() &^ log.LstdFlags)
	// 设置日志输出为 androidLogWriter
	log.SetOutput(&androidLogWriter{
		appCtx: appCtx,
	})

	// 重定向 stdout 和 stderr 到 Android 日志
	logFd(os.Stdout.Fd())
	logFd(os.Stderr.Fd())
}

// androidLogWriter 实现 io.Writer，将日志写入 Android logcat。
type androidLogWriter struct {
	appCtx AppContext
}

// Write 实现 io.Writer，将数据分段写入 Android 日志。
func (w *androidLogWriter) Write(data []byte) (int, error) {
	n := 0
	for len(data) > 0 {
		msg := data
		// 截断超长日志
		if len(msg) > logLineLimit {
			msg = msg[:logLineLimit]
		}
		w.appCtx.Log(ID, string(msg))
		n += len(msg)
		data = data[len(msg):]
	}
	return n, nil
}

// logFd 重定向指定 fd 到 Android 日志。
// fd: 文件描述符。
func logFd(fd uintptr) {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	// 复制 w 的 fd 到目标 fd
	if err := syscall.Dup3(int(w.Fd()), int(fd), syscall.O_CLOEXEC); err != nil {
		panic(err)
	}
	// 启动协程读取日志并写入 Android logcat
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic in logFd %s: %s", p, debug.Stack())
				panic(p)
			}
		}()

		lineBuf := bufio.NewReaderSize(r, logLineLimit)
		// buf 用于传递给 C，包含结尾的 '\0'
		buf := make([]byte, lineBuf.Size()+1)
		cbuf := (*C.char)(unsafe.Pointer(&buf[0]))
		for {
			line, _, err := lineBuf.ReadLine()
			if err != nil {
				break
			}
			copy(buf, line)
			buf[len(line)] = 0
			C.__android_log_write(C.ANDROID_LOG_INFO, logTag, cbuf)
		}
		// 防止 w 被 GC 回收导致 fd 被关闭
		runtime.KeepAlive(w)
	}()
}
