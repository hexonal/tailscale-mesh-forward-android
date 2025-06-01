// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// libtailscale 包为 Android 平台 Tailscale 客户端提供本地 API 调用适配。
package libtailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn"
)

// CallLocalAPI 用于从 Kotlin 调用本地 API。
// timeoutMillis: 超时时间（毫秒）。
// method: HTTP 方法。
// endpoint: API 路径。
// body: 请求体。
// 返回 LocalAPIResponse 和错误。
func (app *App) CallLocalAPI(timeoutMillis int, method, endpoint string, body InputStream) (LocalAPIResponse, error) {
	// 适配 InputStream 并调用底层实现
	return app.callLocalAPI(timeoutMillis, method, endpoint, nil, adaptInputStream(body))
}

// CallLocalAPIMultipart 支持 multipart/form-data 上传。
// timeoutMillis: 超时时间。
// method: HTTP 方法。
// endpoint: API 路径。
// parts: 文件分片。
// 返回 LocalAPIResponse 和错误。
func (app *App) CallLocalAPIMultipart(timeoutMillis int, method, endpoint string, parts FileParts) (LocalAPIResponse, error) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in CallLocalAPIMultipart %s: %s", p, debug.Stack())
			panic(p)
		}
	}()

	// 创建管道用于 multipart 写入
	r, w := io.Pipe()
	defer r.Close()

	mw := multipart.NewWriter(w)
	header := make(http.Header)
	header.Set("Content-Type", mw.FormDataContentType())
	resultCh := make(chan interface{})
	// 启动协程异步调用 API
	go func() {
		resp, err := app.callLocalAPI(timeoutMillis, method, endpoint, header, r)
		if err != nil {
			resultCh <- err
		} else {
			resultCh <- resp
		}
	}()

	// 写入所有分片
	go func() {
		for i := int32(0); i < parts.Len(); i++ {
			part := parts.Get(i)
			contentType := "application/octet-stream"
			if part.ContentType != "" {
				contentType = part.ContentType
			}
			header := make(textproto.MIMEHeader, 3)
			header.Set("Content-Disposition",
				fmt.Sprintf(`form-data; name="%s"; filename="%s"`,
					escapeQuotes("file"), escapeQuotes(part.Filename)))
			header.Set("Content-Type", contentType)
			header.Set("Content-Length", strconv.FormatInt(part.ContentLength, 10))
			p, err := mw.CreatePart(header)
			if err != nil {
				resultCh <- fmt.Errorf("CreatePart: %w", err)
				return
			}
			_, err = io.Copy(p, adaptInputStream(part.Body))
			if err != nil {
				resultCh <- fmt.Errorf("Copy: %w", err)
				return
			}
		}

		err := mw.Close()
		if err != nil {
			resultCh <- fmt.Errorf("Close MultipartWriter: %w", err)
		}
		err = w.Close()
		if err != nil {
			resultCh <- fmt.Errorf("Close Writer: %w", err)
		}
	}()

	// 等待结果
	result := <-resultCh
	switch t := result.(type) {
	case LocalAPIResponse:
		return t, nil
	case error:
		return nil, t
	default:
		panic("unexpected result type, this shouldn't happen")
	}
}

// NotifyPolicyChanged 通知策略变更。
func (app *App) NotifyPolicyChanged() {
	app.policyStore.notifyChanged()
}

// EditPrefs 编辑用户偏好。
// prefs: 掩码偏好。
// 返回 LocalAPIResponse 和错误。
func (app *App) EditPrefs(prefs ipn.MaskedPrefs) (LocalAPIResponse, error) {
	// 使用管道异步编码 JSON
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		enc := json.NewEncoder(w)
		if err := enc.Encode(prefs); err != nil {
			log.Printf("Error encoding preferences: %v", err)
		}
	}()
	return app.callLocalAPI(30000, "PATCH", "prefs", nil, r)
}

// callLocalAPI 实现本地 API 调用的底层逻辑。
// timeoutMillis: 超时时间。
// method: HTTP 方法。
// endpoint: API 路径。
// header: HTTP 头。
// body: 请求体。
// 返回 LocalAPIResponse 和错误。
func (app *App) callLocalAPI(timeoutMillis int, method, endpoint string, header http.Header, body io.ReadCloser) (LocalAPIResponse, error) {
	defer func() {
		if p := recover(); p != nil {
			log.Printf("panic in callLocalAPI %s: %s", p, debug.Stack())
			panic(p)
		}
	}()

	// 等待后端就绪
	app.ready.Wait()

	// 设置超时上下文
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(uint64(timeoutMillis)*uint64(time.Millisecond)))
	defer cancel()

	if body != nil {
		defer body.Close()
	}

	// 构造 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	maps.Copy(req.Header, header)
	if err != nil {
		return nil, fmt.Errorf("error creating new request for %s: %w", endpoint, err)
	}
	// 设置管道用于响应体
	deadline, _ := ctx.Deadline()
	pipeReader, pipeWriter := net.Pipe()
	pipeReader.SetDeadline(deadline)
	pipeWriter.SetDeadline(deadline)

	// 构造响应对象
	resp := &Response{
		headers:          http.Header{},
		status:           http.StatusOK,
		bodyReader:       pipeReader,
		bodyWriter:       pipeWriter,
		startWritingBody: make(chan interface{}),
	}

	// 启动协程处理本地 API
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Printf("panic in CallLocalAPI.ServeHTTP %s: %s", p, debug.Stack())
				panic(p)
			}
		}()

		defer pipeWriter.Close()
		app.localAPIHandler.ServeHTTP(resp, req)
		resp.Flush()
	}()

	// 等待响应体可用或超时
	select {
	case <-resp.startWritingBody:
		return resp, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("timeout for %s", endpoint)
	}
}

// Response 表示本地 API 响应。
type Response struct {
	headers              http.Header      // 响应头
	status               int              // 状态码
	bodyWriter           net.Conn         // 写端
	bodyReader           net.Conn         // 读端
	startWritingBody     chan interface{} // 通知通道
	startWritingBodyOnce sync.Once        // 保证只关闭一次
}

// Header 获取响应头。
func (r *Response) Header() http.Header {
	return r.headers
}

// Write 写入响应体。
func (r *Response) Write(data []byte) (int, error) {
	r.Flush()
	if r.status == 0 {
		r.WriteHeader(http.StatusOK)
	}
	return r.bodyWriter.Write(data)
}

// WriteHeader 写入状态码。
func (r *Response) WriteHeader(statusCode int) {
	r.status = statusCode
}

// Body 获取响应体读端。
func (r *Response) Body() net.Conn {
	return r.bodyReader
}

// BodyBytes 读取全部响应体。
func (r *Response) BodyBytes() ([]byte, error) {
	return io.ReadAll(r.bodyReader)
}

// BodyInputStream 获取响应体流（未实现）。
func (r *Response) BodyInputStream() InputStream {
	return nil
}

// StatusCode 获取状态码。
func (r *Response) StatusCode() int {
	return r.status
}

// Flush 通知响应体可用。
func (r *Response) Flush() {
	r.startWritingBodyOnce.Do(func() {
		close(r.startWritingBody)
	})
}

// adaptInputStream 适配 Java InputStream 为 io.ReadCloser。
// in: InputStream。
// 返回 io.ReadCloser。
func adaptInputStream(in InputStream) io.ReadCloser {
	if in == nil {
		return nil
	}
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		for {
			b, err := in.Read()
			if err != nil {
				log.Printf("error reading from inputstream: %s", err)
			}
			if b == nil {
				return
			}
			w.Write(b)
		}
	}()
	return r
}

// quoteEscaper 用于转义引号。
var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, `\\"`)

// escapeQuotes 转义字符串中的引号。
func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}
