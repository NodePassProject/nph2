// Package nph2 实现了基于HTTP/2协议的高性能、可靠的网络流管理系统
package nph2

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

const (
	defaultMinCap           = 1
	defaultMaxCap           = 1
	defaultMinIvl           = 1 * time.Second
	defaultMaxIvl           = 1 * time.Second
	idReadTimeout           = 1 * time.Minute
	idRetryInterval         = 50 * time.Millisecond
	acceptRetryInterval     = 50 * time.Millisecond
	intervalAdjustStep      = 100 * time.Millisecond
	capacityAdjustLowRatio  = 0.2
	capacityAdjustHighRatio = 0.8
	intervalLowThreshold    = 0.2
	intervalHighThreshold   = 0.8
)

// Pool HTTP/2流池结构体，用于管理HTTP/2流
type Pool struct {
	streams      sync.Map                          // 存储流的映射表
	idChan       chan string                       // 可用流ID通道
	tlsCode      string                            // TLS安全模式代码
	hostname     string                            // 主机名
	clientIP     string                            // 客户端IP
	tlsConfig    *tls.Config                       // TLS配置
	addrResolver func() (string, error)            // 地址解析器
	listenAddr   string                            // 监听地址
	baseListener net.Listener                      // 基础TCP监听器
	h2Conn       atomic.Pointer[net.Conn]          // HTTP/2底层连接
	h2Client     atomic.Pointer[*http2.ClientConn] // HTTP/2客户端连接
	h2Server     *http2.Server                     // HTTP/2服务器
	listener     atomic.Pointer[net.Listener]      // 监听器
	first        atomic.Bool                       // 首次标志
	errCount     atomic.Int32                      // 错误计数
	capacity     atomic.Int32                      // 当前容量
	minCap       int                               // 最小容量
	maxCap       int                               // 最大容量
	interval     atomic.Int64                      // 流创建间隔
	minIvl       time.Duration                     // 最小间隔
	maxIvl       time.Duration                     // 最大间隔
	keepAlive    time.Duration                     // 保活间隔
	ctx          context.Context                   // 上下文
	cancel       context.CancelFunc                // 取消函数
}

// StreamConn 将HTTP/2流包装为net.Conn接口
type StreamConn struct {
	io.ReadWriteCloser
	conn       net.Conn
	localAddr  net.Addr
	remoteAddr net.Addr
}

// LocalAddr 返回本地地址
func (s *StreamConn) LocalAddr() net.Addr {
	return s.localAddr
}

// RemoteAddr 返回远程地址
func (s *StreamConn) RemoteAddr() net.Addr {
	return s.remoteAddr
}

// SetDeadline 设置读写截止时间
func (s *StreamConn) SetDeadline(t time.Time) error {
	// HTTP/2流不支持单独的deadline，使用底层连接的
	if tc, ok := s.conn.(interface{ SetDeadline(time.Time) error }); ok {
		return tc.SetDeadline(t)
	}
	return nil
}

// SetReadDeadline 设置读取截止时间
func (s *StreamConn) SetReadDeadline(t time.Time) error {
	if tc, ok := s.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		return tc.SetReadDeadline(t)
	}
	return nil
}

// SetWriteDeadline 设置写入截止时间
func (s *StreamConn) SetWriteDeadline(t time.Time) error {
	if tc, ok := s.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return tc.SetWriteDeadline(t)
	}
	return nil
}

// ConnectionState 返回TLS状态
func (s *StreamConn) ConnectionState() tls.ConnectionState {
	if tc, ok := s.conn.(*tls.Conn); ok {
		return tc.ConnectionState()
	}
	return tls.ConnectionState{}
}

// buildTLSConfig 构建TLS配置
func buildTLSConfig(tlsCode, hostname string) *tls.Config {
	switch tlsCode {
	case "2":
		// 使用验证证书（安全模式）
		return &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         hostname,
			NextProtos:         []string{"h2"},
			MinVersion:         tls.VersionTLS13,
		}
	default:
		// 使用自签名证书（不验证）
		return &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
			MinVersion:         tls.VersionTLS13,
		}
	}
}

// NewClientPool 创建新的客户端HTTP/2池
func NewClientPool(
	minCap, maxCap int,
	minIvl, maxIvl time.Duration,
	keepAlive time.Duration,
	tlsCode string,
	hostname string,
	addrResolver func() (string, error),
) *Pool {
	if minCap <= 0 {
		minCap = defaultMinCap
	}
	if maxCap <= 0 {
		maxCap = defaultMaxCap
	}
	if minCap > maxCap {
		minCap, maxCap = maxCap, minCap
	}

	if minIvl <= 0 {
		minIvl = defaultMinIvl
	}
	if maxIvl <= 0 {
		maxIvl = defaultMaxIvl
	}
	if minIvl > maxIvl {
		minIvl, maxIvl = maxIvl, minIvl
	}

	pool := &Pool{
		streams:      sync.Map{},
		idChan:       make(chan string, maxCap),
		tlsCode:      tlsCode,
		hostname:     hostname,
		addrResolver: addrResolver,
		minCap:       minCap,
		maxCap:       maxCap,
		minIvl:       minIvl,
		maxIvl:       maxIvl,
		keepAlive:    keepAlive,
	}
	pool.capacity.Store(int32(minCap))
	pool.interval.Store(int64(minIvl))
	pool.ctx, pool.cancel = context.WithCancel(context.Background())
	return pool
}

// NewServerPool 创建新的服务端HTTP/2池
func NewServerPool(
	maxCap int,
	clientIP string,
	tlsConfig *tls.Config,
	baseListener net.Listener,
	keepAlive time.Duration,
) *Pool {
	if maxCap <= 0 {
		maxCap = defaultMaxCap
	}
	if baseListener == nil {
		return nil
	}
	if tlsConfig == nil {
		return nil
	}

	tlsConfig = tlsConfig.Clone()
	tlsConfig.NextProtos = []string{"h2"}
	tlsConfig.MinVersion = tls.VersionTLS13

	pool := &Pool{
		streams:      sync.Map{},
		idChan:       make(chan string, maxCap),
		clientIP:     clientIP,
		tlsConfig:    tlsConfig,
		baseListener: baseListener,
		listenAddr:   baseListener.Addr().String(),
		maxCap:       maxCap,
		keepAlive:    keepAlive,
		h2Server: &http2.Server{
			MaxConcurrentStreams: uint32(maxCap),
			IdleTimeout:          keepAlive * 2,
		},
	}
	pool.ctx, pool.cancel = context.WithCancel(context.Background())
	return pool
}

// http2Stream 双向流实现
type http2Stream struct {
	reader io.ReadCloser
	writer io.WriteCloser
	closed atomic.Bool
}

func (s *http2Stream) Read(p []byte) (n int, err error) {
	return s.reader.Read(p)
}

func (s *http2Stream) Write(p []byte) (n int, err error) {
	return s.writer.Write(p)
}

func (s *http2Stream) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if s.reader != nil {
		s.reader.Close()
	}
	if s.writer != nil {
		s.writer.Close()
	}
	return nil
}

// createStream 创建新的客户端流
func (p *Pool) createStream() bool {
	client := p.h2Client.Load()
	if client == nil || *client == nil {
		return false
	}

	// 创建管道对用于双向通信
	reqReader, reqWriter := io.Pipe()
	defer func() {
		if reqWriter != nil {
			reqWriter.Close()
		}
		if reqReader != nil {
			reqReader.Close()
		}
	}()

	// 创建HTTP/2请求
	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Scheme: "https", Host: p.hostname, Path: "/stream"},
		Header: http.Header{},
		Body:   reqReader,
	}

	// 发送请求并获取响应
	ctx, cancel := context.WithTimeout(p.ctx, idReadTimeout)
	defer cancel()

	respChan := make(chan *http.Response, 1)
	errChan := make(chan error, 1)

	go func() {
		resp, err := (*client).RoundTrip(req)
		if err != nil {
			errChan <- err
			return
		}
		respChan <- resp
	}()

	// 立即发送握手字节
	if _, err := reqWriter.Write([]byte{0x00}); err != nil {
		return false
	}

	// 等待响应
	var resp *http.Response
	select {
	case resp = <-respChan:
	case <-errChan:
		return false
	case <-ctx.Done():
		return false
	}

	// 接收流ID
	buf := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		resp.Body.Close()
		return false
	}
	id := hex.EncodeToString(buf)

	// 创建双向流
	stream := &http2Stream{
		reader: resp.Body,
		writer: reqWriter,
	}

	// 建立映射并存入通道
	select {
	case p.idChan <- id:
		p.streams.Store(id, stream)
		reqWriter = nil
		reqReader = nil
		return true
	default:
		stream.Close()
		return false
	}
}

// handleStream 处理新的服务端流
func (p *Pool) handleStream(rw http.ResponseWriter, req *http.Request) {
	// 检查路径、方法和池容量
	if req.URL.Path != "/stream" || req.Method != "POST" || p.Active() >= p.maxCap {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}

	// 读取握手字节
	var handshake [1]byte
	if _, err := req.Body.Read(handshake[:]); err != nil {
		return
	}

	// 生成流ID
	rawID, id, err := p.generateID()
	if err != nil {
		return
	}

	// 防止重复流ID并发送ID
	if _, exist := p.streams.Load(id); exist {
		return
	}
	if _, err := rw.Write(rawID); err != nil {
		return
	}
	if flusher, ok := rw.(http.Flusher); ok {
		flusher.Flush()
	}

	// 创建双向流并尝试放入通道
	stream := &http2Stream{
		reader: req.Body,
		writer: &responseWriter{rw: rw},
	}

	select {
	case p.idChan <- id:
		p.streams.Store(id, stream)
		defer func() {
			p.streams.Delete(id)
			stream.Close()
		}()
	default:
		stream.Close()
		return
	}

	// 保持流打开直到上下文取消
	<-p.ctx.Done()
}

// responseWriter 包装http.ResponseWriter为io.WriteCloser
type responseWriter struct {
	rw     http.ResponseWriter
	closed atomic.Bool
}

func (w *responseWriter) Write(p []byte) (n int, err error) {
	if w.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	n, err = w.rw.Write(p)
	if flusher, ok := w.rw.(http.Flusher); ok {
		flusher.Flush()
	}
	return
}

func (w *responseWriter) Close() error {
	w.closed.Store(true)
	return nil
}

// establishConnection 建立HTTP/2连接
func (p *Pool) establishConnection() error {
	existingConn := p.h2Conn.Load()
	client := p.h2Client.Load()
	if existingConn != nil && client != nil && *client != nil {
		if err := (*client).Ping(p.ctx); err == nil {
			return nil
		}
		p.h2Conn.Store(nil)
		p.h2Client.Store(nil)
	}

	targetAddr, err := p.addrResolver()
	if err != nil {
		return fmt.Errorf("establishConnection: address resolution failed: %w", err)
	}

	tlsConfig := buildTLSConfig(p.tlsCode, p.hostname)

	// 建立TLS连接
	tlsConn, err := tls.Dial("tcp", targetAddr, tlsConfig)
	if err != nil {
		return err
	}

	// 创建HTTP/2传输层
	transport := &http2.Transport{}
	h2Client, err := transport.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return err
	}

	netConn := net.Conn(tlsConn)
	p.h2Conn.Store(&netConn)
	p.h2Client.Store(&h2Client)
	return nil
}

// startListener 启动HTTP/2监听器
func (p *Pool) startListener() error {
	if p.tlsConfig == nil {
		return fmt.Errorf("startListener: TLS config is required")
	}
	if p.baseListener == nil {
		return fmt.Errorf("startListener: base listener is required")
	}

	var l net.Listener = p.baseListener
	p.listener.Store(&l)
	return nil
}

// ClientManager 客户端HTTP/2池管理器
func (p *Pool) ClientManager() {
	if p.cancel != nil {
		p.cancel()
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())

	if p.keepAlive > 0 {
		go func() {
			ticker := time.NewTicker(p.keepAlive)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if client := p.h2Client.Load(); client != nil && *client != nil {
						(*client).Ping(p.ctx)
					}
				case <-p.ctx.Done():
					return
				}
			}
		}()
	}

	for p.ctx.Err() == nil {
		p.adjustInterval()
		capacity := int(p.capacity.Load())
		need := capacity - len(p.idChan)
		created := 0

		if need > 0 {
			if err := p.establishConnection(); err != nil {
				time.Sleep(acceptRetryInterval)
				continue
			}

			var wg sync.WaitGroup
			results := make(chan int, need)
			for range need {
				wg.Go(func() {
					if p.createStream() {
						results <- 1
					}
				})
			}
			wg.Wait()
			close(results)
			for r := range results {
				created += r
			}
		}

		p.adjustCapacity(created)

		select {
		case <-p.ctx.Done():
			return
		case <-time.After(time.Duration(p.interval.Load())):
		}
	}
}

// ServerManager 服务端HTTP/2池管理器
func (p *Pool) ServerManager() {
	if p.cancel != nil {
		p.cancel()
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())

	if err := p.startListener(); err != nil {
		return
	}

	listener := p.listener.Load()
	if listener == nil {
		return
	}

	// HTTP处理器
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.clientIP != "" {
			if host, _, _ := net.SplitHostPort(r.RemoteAddr); host != p.clientIP {
				http.Error(w, "unauthorized", http.StatusForbidden)
				return
			}
		}
		p.handleStream(w, r)
	})

	// 配置HTTP/2
	if err := http2.ConfigureServer(&http.Server{Handler: handler}, p.h2Server); err != nil {
		return
	}

	// 接受连接
	for p.ctx.Err() == nil {
		conn, err := (*listener).Accept()
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			time.Sleep(acceptRetryInterval)
			continue
		}

		// TLS包装和握手
		tlsConn := tls.Server(conn, p.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			continue
		}

		// 验证ALPN
		if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
			tlsConn.Close()
			continue
		}

		// 处理连接
		netConn := net.Conn(tlsConn)
		p.h2Conn.Store(&netConn)

		go func(c net.Conn) {
			defer c.Close()
			p.h2Server.ServeConn(c, &http2.ServeConnOpts{
				Context: p.ctx,
				Handler: handler,
			})
		}(tlsConn)
	}
}

// OutgoingGet 根据ID获取可用流
func (p *Pool) OutgoingGet(id string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(idRetryInterval)
	defer ticker.Stop()

	for {
		if stream, ok := p.streams.LoadAndDelete(id); ok {
			<-p.idChan
			if conn := p.h2Conn.Load(); conn != nil {
				return &StreamConn{
					ReadWriteCloser: stream.(io.ReadWriteCloser),
					conn:            *conn,
					localAddr:       (*conn).LocalAddr(),
					remoteAddr:      (*conn).RemoteAddr(),
				}, nil
			}
			return nil, fmt.Errorf("OutgoingGet: connection not available")
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return nil, fmt.Errorf("OutgoingGet: stream not found")
		}
	}
}

// IncomingGet 获取可用流并返回ID
func (p *Pool) IncomingGet(timeout time.Duration) (string, net.Conn, error) {
	ctx, cancel := context.WithTimeout(p.ctx, timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return "", nil, fmt.Errorf("IncomingGet: insufficient streams")
		case id := <-p.idChan:
			if stream, ok := p.streams.LoadAndDelete(id); ok {
				if conn := p.h2Conn.Load(); conn != nil {
					return id, &StreamConn{
						ReadWriteCloser: stream.(io.ReadWriteCloser),
						conn:            *conn,
						localAddr:       (*conn).LocalAddr(),
						remoteAddr:      (*conn).RemoteAddr(),
					}, nil
				}
			}
		}
	}
}

// Flush 清空池中的所有流
func (p *Pool) Flush() {
	var wg sync.WaitGroup
	p.streams.Range(func(key, value any) bool {
		wg.Go(func() {
			if stream, ok := value.(io.Closer); ok {
				stream.Close()
			}
		})
		return true
	})
	wg.Wait()

	p.streams = sync.Map{}
	p.idChan = make(chan string, p.maxCap)
}

// Close 关闭连接池并释放资源
func (p *Pool) Close() {
	if p.cancel != nil {
		p.cancel()
	}
	p.Flush()

	if conn := p.h2Conn.Swap(nil); conn != nil {
		(*conn).Close()
	}
	if listener := p.listener.Swap(nil); listener != nil {
		(*listener).Close()
	}
}

// Ready 检查连接池是否已初始化
func (p *Pool) Ready() bool {
	return p.ctx != nil
}

// Active 获取当前活跃流数
func (p *Pool) Active() int {
	return len(p.idChan)
}

// Capacity 获取当前池容量
func (p *Pool) Capacity() int {
	return int(p.capacity.Load())
}

// Interval 获取当前流创建间隔
func (p *Pool) Interval() time.Duration {
	return time.Duration(p.interval.Load())
}

// AddError 增加错误计数
func (p *Pool) AddError() {
	p.errCount.Add(1)
}

// ErrorCount 获取错误计数
func (p *Pool) ErrorCount() int {
	return int(p.errCount.Load())
}

// ResetError 重置错误计数
func (p *Pool) ResetError() {
	p.errCount.Store(0)
}

// adjustInterval 根据池使用情况动态调整流创建间隔
func (p *Pool) adjustInterval() {
	idle := len(p.idChan)
	capacity := int(p.capacity.Load())
	interval := time.Duration(p.interval.Load())

	if idle < int(float64(capacity)*intervalLowThreshold) && interval > p.minIvl {
		newInterval := max(interval-intervalAdjustStep, p.minIvl)
		p.interval.Store(int64(newInterval))
	}

	if idle > int(float64(capacity)*intervalHighThreshold) && interval < p.maxIvl {
		newInterval := min(interval+intervalAdjustStep, p.maxIvl)
		p.interval.Store(int64(newInterval))
	}
}

// adjustCapacity 根据创建成功率动态调整池容量
func (p *Pool) adjustCapacity(created int) {
	capacity := int(p.capacity.Load())
	ratio := float64(created) / float64(capacity)

	if ratio < capacityAdjustLowRatio && capacity > p.minCap {
		p.capacity.Add(-1)
	}

	if ratio > capacityAdjustHighRatio && capacity < p.maxCap {
		p.capacity.Add(1)
	}
}

// generateID 生成唯一流ID
func (p *Pool) generateID() ([]byte, string, error) {
	if p.first.CompareAndSwap(false, true) {
		return []byte{0, 0, 0, 0}, "00000000", nil
	}

	rawID := make([]byte, 4)
	if _, err := rand.Read(rawID); err != nil {
		return nil, "", err
	}
	id := hex.EncodeToString(rawID)
	return rawID, id, nil
}
