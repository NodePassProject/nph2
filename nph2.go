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

type Pool struct {
	streams      sync.Map
	idChan       chan string
	tlsCode      string
	hostname     string
	clientIP     string
	tlsConfig    *tls.Config
	addrResolver func() (string, error)
	listenAddr   string
	baseListener net.Listener
	h2Conn       atomic.Pointer[net.Conn]
	h2Client     atomic.Pointer[*http2.ClientConn]
	h2Server     *http2.Server
	listener     atomic.Pointer[net.Listener]
	first        atomic.Bool
	errCount     atomic.Int32
	capacity     atomic.Int32
	minCap       int
	maxCap       int
	interval     atomic.Int64
	minIvl       time.Duration
	maxIvl       time.Duration
	keepAlive    time.Duration
	ctx          context.Context
	cancel       context.CancelFunc
}

type StreamConn struct {
	io.ReadWriteCloser
	conn       net.Conn
	localAddr  net.Addr
	remoteAddr net.Addr
}

func (s *StreamConn) LocalAddr() net.Addr {
	return s.localAddr
}

func (s *StreamConn) RemoteAddr() net.Addr {
	return s.remoteAddr
}

func (s *StreamConn) SetDeadline(t time.Time) error {
	if tc, ok := s.conn.(interface{ SetDeadline(time.Time) error }); ok {
		return tc.SetDeadline(t)
	}
	return nil
}

func (s *StreamConn) SetReadDeadline(t time.Time) error {
	if tc, ok := s.conn.(interface{ SetReadDeadline(time.Time) error }); ok {
		return tc.SetReadDeadline(t)
	}
	return nil
}

func (s *StreamConn) SetWriteDeadline(t time.Time) error {
	if tc, ok := s.conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return tc.SetWriteDeadline(t)
	}
	return nil
}

func (s *StreamConn) ConnectionState() tls.ConnectionState {
	if tc, ok := s.conn.(*tls.Conn); ok {
		return tc.ConnectionState()
	}
	return tls.ConnectionState{}
}

func buildTLSConfig(tlsCode, hostname string) *tls.Config {
	switch tlsCode {
	case "2":
		return &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         hostname,
			NextProtos:         []string{"h2"},
			MinVersion:         tls.VersionTLS13,
		}
	default:
		return &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"h2"},
			MinVersion:         tls.VersionTLS13,
		}
	}
}

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

func (p *Pool) createStream() bool {
	client := p.h2Client.Load()
	if client == nil || *client == nil {
		return false
	}

	reqReader, reqWriter := io.Pipe()
	defer func() {
		if reqWriter != nil {
			reqWriter.Close()
		}
		if reqReader != nil {
			reqReader.Close()
		}
	}()

	req := &http.Request{
		Method: "POST",
		URL:    &url.URL{Scheme: "https", Host: p.hostname, Path: "/stream"},
		Header: http.Header{},
		Body:   reqReader,
	}

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

	if _, err := reqWriter.Write([]byte{0x00}); err != nil {
		return false
	}

	var resp *http.Response
	select {
	case resp = <-respChan:
	case <-errChan:
		return false
	case <-ctx.Done():
		return false
	}

	buf := make([]byte, 4)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		resp.Body.Close()
		return false
	}
	id := hex.EncodeToString(buf)

	stream := &http2Stream{
		reader: resp.Body,
		writer: reqWriter,
	}

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

func (p *Pool) handleStream(rw http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/stream" || req.Method != "POST" || p.Active() >= p.maxCap {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}

	var handshake [1]byte
	if _, err := req.Body.Read(handshake[:]); err != nil {
		return
	}

	rawID, id, err := p.generateID()
	if err != nil {
		return
	}

	if _, exist := p.streams.Load(id); exist {
		return
	}
	if _, err := rw.Write(rawID); err != nil {
		return
	}
	if flusher, ok := rw.(http.Flusher); ok {
		flusher.Flush()
	}

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

	<-p.ctx.Done()
}

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

	tlsConn, err := tls.Dial("tcp", targetAddr, tlsConfig)
	if err != nil {
		return err
	}

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

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.clientIP != "" {
			if host, _, _ := net.SplitHostPort(r.RemoteAddr); host != p.clientIP {
				http.Error(w, "unauthorized", http.StatusForbidden)
				return
			}
		}
		p.handleStream(w, r)
	})

	if err := http2.ConfigureServer(&http.Server{Handler: handler}, p.h2Server); err != nil {
		return
	}

	for p.ctx.Err() == nil {
		conn, err := (*listener).Accept()
		if err != nil {
			if p.ctx.Err() != nil {
				return
			}
			time.Sleep(acceptRetryInterval)
			continue
		}

		tlsConn := tls.Server(conn, p.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			conn.Close()
			continue
		}

		if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
			tlsConn.Close()
			continue
		}

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

func (p *Pool) Ready() bool {
	return p.ctx != nil
}

func (p *Pool) Active() int {
	return len(p.idChan)
}

func (p *Pool) Capacity() int {
	return int(p.capacity.Load())
}

func (p *Pool) Interval() time.Duration {
	return time.Duration(p.interval.Load())
}

func (p *Pool) AddError() {
	p.errCount.Add(1)
}

func (p *Pool) ErrorCount() int {
	return int(p.errCount.Load())
}

func (p *Pool) ResetError() {
	p.errCount.Store(0)
}

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
