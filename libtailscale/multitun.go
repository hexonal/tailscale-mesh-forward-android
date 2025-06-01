// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// multitun.go 实现 Android 平台多 TUN 设备适配，支持动态切换 TUN。
package libtailscale

import (
	"log"
	"os"
	"runtime/debug"

	"github.com/tailscale/wireguard-go/tun"
)

// multiTUN 实现 tun.Device 接口，支持多个底层 TUN 设备。
type multiTUN struct {
	// devices 用于添加新设备。
	devices chan tun.Device
	// events 汇总所有活跃设备的事件。
	events chan tun.Event

	close    chan struct{}
	closeErr chan error

	reads        chan ioRequest
	writes       chan ioRequest
	mtus         chan chan mtuReply
	names        chan chan nameReply
	shutdowns    chan struct{}
	shutdownDone chan struct{}
}

// tunDevice 封装单个 tun.Device。
type tunDevice struct {
	dev tun.Device
	// close 关闭设备
	close chan struct{}
	// closeDone 关闭完成通知
	closeDone chan error
	// readDone 读协程完成通知
	readDone chan struct{}
}

type ioRequest struct {
	data   [][]byte
	sizes  []int
	offset int
	reply  chan<- ioReply
}

type ioReply struct {
	count int
	err   error
}

type mtuReply struct {
	mtu int
	err error
}

type nameReply struct {
	name string
	err  error
}

// newTUNDevices 创建 multiTUN 实例。
func newTUNDevices() *multiTUN {
	d := &multiTUN{
		devices:      make(chan tun.Device),
		events:       make(chan tun.Event),
		close:        make(chan struct{}),
		closeErr:     make(chan error),
		reads:        make(chan ioRequest),
		writes:       make(chan ioRequest),
		mtus:         make(chan chan mtuReply),
		names:        make(chan chan nameReply),
		shutdowns:    make(chan struct{}),
		shutdownDone: make(chan struct{}),
	}
	// 启动主循环
	go d.run()
	return d
}

// run 主循环，管理设备切换、事件分发等。
func (d *multiTUN) run() {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in multiTUN.run %s: %s", p, debug.Stack())
			panic(p)
		}
	}()

	var devices []*tunDevice
	// readDone 当前读取设备的 readDone 通道
	var readDone chan struct{}
	// runDone 当前写入设备的 closeDone 通道
	var runDone chan error
	for {
		select {
		case <-readDone:
			// 最老设备 EOF，切换下一个
			n := copy(devices, devices[1:])
			devices = devices[:n]
			if len(devices) > 0 {
				dev := devices[0]
				readDone = dev.readDone
				go d.readFrom(dev)
			}
		case <-runDone:
			// 写入设备完成，切换下一个
			if len(devices) > 0 {
				dev := devices[len(devices)-1]
				runDone = dev.closeDone
				go d.runDevice(dev)
			}
		case <-d.shutdowns:
			// 关闭所有设备
			for _, dev := range devices {
				close(dev.close)
				<-dev.closeDone
				<-dev.readDone
			}
			devices = nil
			d.shutdownDone <- struct{}{}
		case <-d.close:
			// 关闭并返回错误
			var derr error
			for _, dev := range devices {
				if err := <-dev.closeDone; err != nil {
					derr = err
				}
			}
			d.closeErr <- derr
			return
		case dev := <-d.devices:
			// 添加新设备
			if len(devices) > 0 {
				prev := devices[len(devices)-1]
				close(prev.close)
			}
			wrap := &tunDevice{
				dev:       dev,
				close:     make(chan struct{}),
				closeDone: make(chan error),
				readDone:  make(chan struct{}, 1),
			}
			if len(devices) == 0 {
				readDone = wrap.readDone
				go d.readFrom(wrap)
				runDone = wrap.closeDone
				go d.runDevice(wrap)
			}
			devices = append(devices, wrap)
		case m := <-d.mtus:
			r := mtuReply{mtu: defaultMTU}
			if len(devices) > 0 {
				dev := devices[len(devices)-1]
				r.mtu, r.err = dev.dev.MTU()
			}
			m <- r
		case n := <-d.names:
			var r nameReply
			if len(devices) > 0 {
				dev := devices[len(devices)-1]
				r.name, r.err = dev.dev.Name()
			}
			n <- r
		}
	}
}

// readFrom 读取数据。
func (d *multiTUN) readFrom(dev *tunDevice) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in multiTUN.readFrom %s: %s", p, debug.Stack())
			panic(p)
		}
	}()

	defer func() {
		dev.readDone <- struct{}{}
	}()
	for {
		select {
		case r := <-d.reads:
			n, err := dev.dev.Read(r.data, r.sizes, r.offset)
			stop := false
			if err != nil {
				select {
				case <-dev.close:
					stop = true
					err = nil
				default:
				}
			}
			r.reply <- ioReply{n, err}
			if stop {
				return
			}
		case <-d.close:
			return
		}
	}
}

// runDevice 处理设备写入和事件。
func (d *multiTUN) runDevice(dev *tunDevice) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in multiTUN.runDevice %s: %s", p, debug.Stack())
			panic(p)
		}
	}()

	defer func() {
		dev.closeDone <- dev.dev.Close()
	}()
	// 事件分发协程
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic in multiTUN.readFrom.events %s: %s", p, debug.Stack())
				panic(p)
			}
		}()
		for {
			select {
			case e := <-dev.dev.Events():
				d.events <- e
			case <-dev.close:
				return
			}
		}
	}()
	for {
		select {
		case w := <-d.writes:
			n, err := dev.dev.Write(w.data, w.offset)
			w.reply <- ioReply{n, err}
		case <-dev.close:
			// 设备关闭
			return
		case <-d.close:
			// 多设备关闭
			return
		}
	}
}

// add 添加新 tun 设备。
func (d *multiTUN) add(dev tun.Device) {
	d.devices <- dev
}

// File 返回底层文件（Android 不支持）。
func (d *multiTUN) File() *os.File {
	panic("not available on Android")
}

// Read 读取数据。
func (d *multiTUN) Read(data [][]byte, sizes []int, offset int) (int, error) {
	r := make(chan ioReply)
	d.reads <- ioRequest{data, sizes, offset, r}
	rep := <-r
	return rep.count, rep.err
}

// Write 写入数据。
func (d *multiTUN) Write(data [][]byte, offset int) (int, error) {
	r := make(chan ioReply)
	d.writes <- ioRequest{data, nil, offset, r}
	rep := <-r
	return rep.count, rep.err
}

// MTU 获取 MTU。
func (d *multiTUN) MTU() (int, error) {
	r := make(chan mtuReply)
	d.mtus <- r
	rep := <-r
	return rep.mtu, rep.err
}

// Name 获取设备名。
func (d *multiTUN) Name() (string, error) {
	r := make(chan nameReply)
	d.names <- r
	rep := <-r
	return rep.name, rep.err
}

// Events 获取事件通道。
func (d *multiTUN) Events() <-chan tun.Event {
	return d.events
}

// Shutdown 关闭所有设备。
func (d *multiTUN) Shutdown() {
	d.shutdowns <- struct{}{}
	<-d.shutdownDone
}

// Close 关闭 multiTUN。
func (d *multiTUN) Close() error {
	close(d.close)
	return <-d.closeErr
}

// BatchSize 返回批处理大小（Android 固定为 1）。
func (d *multiTUN) BatchSize() int {
	return 1
}
