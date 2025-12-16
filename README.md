# NPH2 Package

[![Go Reference](https://pkg.go.dev/badge/github.com/NodePassProject/http2.svg)](https://pkg.go.dev/github.com/NodePassProject/http2)
[![License](https://img.shields.io/badge/License-BSD_3--Clause-blue.svg)](https://opensource.org/licenses/BSD-3-Clause)

A high-performance, reliable HTTP/2 stream pool management system for Go applications.

## Table of Contents

- [Features](#features)
- [Installation](#installation)
- [Quick Start](#quick-start)
- [Usage](#usage)
  - [Client Stream Pool](#client-stream-pool)
  - [Server Stream Pool](#server-stream-pool)
  - [Managing Pool Health](#managing-pool-health)
- [Security Features](#security-features)
  - [Client IP Restriction](#client-ip-restriction)
  - [TLS Security Modes](#tls-security-modes)
- [Stream Multiplexing](#stream-multiplexing)
- [Dynamic Adjustment](#dynamic-adjustment)
- [Advanced Usage](#advanced-usage)
- [Performance Considerations](#performance-considerations)
- [Troubleshooting](#troubleshooting)
- [Best Practices](#best-practices)
  - [Pool Configuration](#1-pool-configuration)
  - [Stream Management](#2-stream-management)
  - [Error Handling and Monitoring](#3-error-handling-and-monitoring)
  - [Production Deployment](#4-production-deployment)
  - [Performance Optimization](#5-performance-optimization)
  - [Testing and Development](#6-testing-and-development)
- [License](#license)

## Features

- **Single HTTP/2 connection architecture** with efficient stream multiplexing
- **Lock-free design** using atomic operations for maximum performance
- **Thread-safe stream management** with `sync.Map` and atomic pointers
- **Support for both client and server stream pools**
- **Dynamic capacity and interval adjustment** based on real-time usage patterns
- **Automatic stream health monitoring** and lifecycle management
- **Concurrent stream creation** for optimal performance
- **Mandatory TLS encryption** for all connections
- **Multiple TLS security modes** (InsecureSkipVerify for dev, verified certificates for production)
- **4-byte hex stream identification** for efficient tracking
- **Graceful error handling and recovery** with automatic retry mechanisms
- **Configurable stream creation intervals** with dynamic adjustment
- **Auto-reconnection** on connection failures
- **Built-in keep-alive management** with configurable periods
- **Zero lock contention** for high concurrency scenarios
- **Standard HTTP/2 protocol** for better compatibility and firewall traversal

## Installation

```bash
go get github.com/NodePassProject/http2
```

## Quick Start

Here's a minimal example to get you started:

```go
package main

import (
    "log"
    "time"
    "github.com/NodePassProject/http2"
)

func main() {
    // Create address resolver
    addrResolver := func() (string, error) {
        return "example.com:443", nil
    }
    
    // Create a client pool
    clientPool := http2.NewClientPool(
        5, 20,                              // min/max capacity
        500*time.Millisecond, 5*time.Second, // min/max intervals
        30*time.Second,                     // keep-alive period
        "0",                                // TLS mode
        "example.com",                      // hostname
        addrResolver,                       // address resolver function
    )
    defer clientPool.Close()
    
    // Start the pool manager
    go clientPool.ClientManager()
    
    // Wait for pool to initialize
    time.Sleep(100 * time.Millisecond)
    
    // Get a stream from the pool by ID (8-character hex string)
    stream, err := clientPool.OutgoingGet("a1b2c3d4", 10*time.Second)
    if err != nil {
        log.Printf("Failed to get stream: %v", err)
        return
    }
    defer stream.Close()
    
    // Use stream...
    _, err = stream.Write([]byte("Hello HTTP/2"))
    if err != nil {
        log.Printf("Write error: %v", err)
    }
}
```

## Usage

### Client Stream Pool

```go
package main

import (
    "log"
    "time"
    "github.com/NodePassProject/http2"
)

func main() {
    // Create address resolver
    addrResolver := func() (string, error) {
        return "example.com:443", nil
    }
    
    // Create a new client pool with:
    // - Minimum capacity: 5 streams
    // - Maximum capacity: 20 streams
    // - Minimum interval: 500ms between stream creation attempts
    // - Maximum interval: 5s between stream creation attempts
    // - Keep-alive period: 30s for connection health monitoring
    // - TLS mode: "2" (verified certificates)
    // - Hostname for certificate verification: "example.com"
    // - Address resolver: Function that returns target HTTP/2 address
    clientPool := http2.NewClientPool(
        5, 20,
        500*time.Millisecond, 5*time.Second,
        30*time.Second,
        "2",
        "example.com",
        addrResolver,
    )
    defer clientPool.Close()
    
    // Start the client manager
    go clientPool.ClientManager()
    
    // Get a stream by ID with timeout (ID is 8-char hex string from server)
    timeout := 10 * time.Second
    stream, err := clientPool.OutgoingGet("a1b2c3d4", timeout)
    if err != nil {
        log.Printf("Stream not found: %v", err)
        return
    }
    defer stream.Close()
    
    // Use the stream...
    data := []byte("Hello from client")
    if _, err := stream.Write(data); err != nil {
        log.Printf("Write failed: %v", err)
    }
}
```

**Note:** `OutgoingGet` takes a stream ID and timeout duration, and returns `(net.Conn, error)`. 
The error indicates if the stream with the specified ID was not found or if the timeout was exceeded.

### Server Stream Pool

```go
package main

import (
    "crypto/tls"
    "log"
    "time"
    "github.com/NodePassProject/http2"
)

func main() {
    // Load TLS certificate (REQUIRED)
    cert, err := tls.LoadX509KeyPair("cert.pem", "key.pem")
    if err != nil {
        log.Fatal(err)
    }
    
    // Create TLS config (REQUIRED - this library mandates TLS encryption)
    tlsConfig := &tls.Config{
        Certificates: []tls.Certificate{cert},
    }
    
    // Create a new server pool
    // - Maximum capacity: 20 streams
    // - Restrict to specific client IP (optional, "" for any IP)
    // - TLS config is REQUIRED (nil will cause NewServerPool to return nil)
    // - Listen on address: "0.0.0.0:443"
    // - Keep-alive period: 30s for connection health monitoring
    serverPool := http2.NewServerPool(
        20,                    // maxCap
        "192.168.1.10",       // clientIP (use "" to allow any IP)
        tlsConfig,            // TLS configuration (REQUIRED)
        "0.0.0.0:443",        // listenAddr
        30*time.Second,       // keep-alive period
    )
    defer serverPool.Close()
    
    // Start the server manager
    go serverPool.ServerManager()
    
    // Get incoming streams with timeout
    for {
        id, stream, err := serverPool.IncomingGet(10 * time.Second)
        if err != nil {
            log.Printf("Failed to get stream: %v", err)
            continue
        }
        
        // Handle the stream in a goroutine
        go handleStream(id, stream)
    }
}

func handleStream(id string, stream net.Conn) {
    defer stream.Close()
    
    log.Printf("Handling stream with ID: %s", id)
    
    // Read from stream
    buf := make([]byte, 4096)
    n, err := stream.Read(buf)
    if err != nil {
        log.Printf("Read error: %v", err)
        return
    }
    
    // Process data...
    log.Printf("Received: %s", string(buf[:n]))
    
    // Write response
    if _, err := stream.Write([]byte("Response from server")); err != nil {
        log.Printf("Write error: %v", err)
    }
}
```

**Note:** `IncomingGet` returns `(string, net.Conn, error)` where the string is the stream ID, which can be used to identify streams.

### Managing Pool Health

```go
// Check if pool is ready
if !pool.Ready() {
    log.Println("Pool not initialized")
    return
}

// Get current pool statistics
active := pool.Active()           // Number of streams in the pool
capacity := pool.Capacity()       // Current capacity setting
interval := pool.Interval()       // Current creation interval

log.Printf("Pool stats - Active: %d, Capacity: %d, Interval: %v", 
    active, capacity, interval)

// Error tracking
pool.AddError()                   // Increment error counter
errorCount := pool.ErrorCount()   // Get current error count
pool.ResetError()                 // Reset error counter

// Flush all streams (useful for testing or emergency cleanup)
pool.Flush()
```

## Security Features

### Client IP Restriction

The server pool supports restricting connections to a specific client IP address:

```go
// Allow connections only from 192.168.1.10
serverPool := http2.NewServerPool(
    20,
    "192.168.1.10",  // Only this IP can connect
    tlsConfig,
    "0.0.0.0:443",
    30*time.Second,
)

// Allow connections from any IP
serverPool := http2.NewServerPool(
    20,
    "",  // Empty string allows any IP
    tlsConfig,
    "0.0.0.0:443",
    30*time.Second,
)
```

### TLS Security Modes

**This library requires TLS encryption for all connections.** The package supports two TLS security modes:

| Mode | Description | Security Level | Use Case |
|------|-------------|----------------|----------|
| `"0"` or `"1"` | InsecureSkipVerify | Medium | Development, testing, internal networks with self-signed certificates |
| `"2"` | Verified certificates | High | Production, public networks |

#### Mode "0" or "1": Development Mode (InsecureSkipVerify)
- Skips certificate verification
- Uses TLS encryption but doesn't validate certificates
- Useful for development and testing with self-signed certificates
- **Not recommended for production**

```go
clientPool := http2.NewClientPool(
    5, 20,
    500*time.Millisecond, 5*time.Second,
    30*time.Second,
    "0",  // or "1" - TLS with InsecureSkipVerify
    "example.com",
    addrResolver,
)
```

#### Mode "2": Production Mode (Verified Certificates)
- Performs full certificate verification
- Requires valid TLS certificates
- **Recommended for production**

```go
clientPool := http2.NewClientPool(
    5, 20,
    500*time.Millisecond, 5*time.Second,
    30*time.Second,
    "2",
    "example.com",  // Must match certificate CN/SAN
    addrResolver,
)
```

## Stream Multiplexing

HTTP/2 naturally supports stream multiplexing over a single TCP connection. This package manages multiple streams efficiently:

```go
// All streams share a single HTTP/2 connection
// The pool automatically manages stream lifecycle

// Stream 1
stream1, _ := pool.OutgoingGet("00000001", 5*time.Second)
go handleStream(stream1)

// Stream 2 - uses the same underlying HTTP/2 connection
stream2, _ := pool.OutgoingGet("00000002", 5*time.Second)
go handleStream(stream2)

// Stream 3 - also shares the connection
stream3, _ := pool.OutgoingGet("00000003", 5*time.Second)
go handleStream(stream3)
```

**Benefits:**
- Reduced connection overhead
- Better resource utilization
- Lower latency for stream creation
- Efficient use of network resources
- Standard HTTP/2 compatibility

## HTTP/2 Keep-Alive

The pool implements connection keep-alive to maintain HTTP/2 connection health and detect broken connections:

### Keep-Alive Features

- **Automatic Keep-Alive**: HTTP/2 connections use PING frames for health checks
- **Configurable Period**: Set custom keep-alive periods for both client and server pools
- **Connection Health**: Helps detect and remove dead connections from the pool
- **Network Efficiency**: Reduces unnecessary connection overhead

### Usage Examples

```go
// Client pool with 30-second keep-alive
clientPool := http2.NewClientPool(
    5, 20,
    500*time.Millisecond, 5*time.Second,
    30*time.Second,  // Keep-alive period
    "2",             // TLS mode
    "example.com",   // hostname
    addrResolver,
)

// Server pool with 60-second keep-alive
serverPool := http2.NewServerPool(
    20,
    "192.168.1.10",
    tlsConfig,
    "0.0.0.0:443",
    60*time.Second,  // Keep-alive period
)
```

### Keep-Alive Best Practices

| Period Range | Use Case | Pros | Cons |
|-------------|----------|------|------|
| 15-30s | High-frequency apps, real-time systems | Quick dead connection detection | Higher network overhead |
| 30-60s | General purpose applications | Balanced performance/overhead | Standard detection time |
| 60-120s | Low-frequency, batch processing | Minimal network overhead | Slower dead connection detection |

**Recommendations:**
- **Web applications**: 30-60 seconds
- **Real-time systems**: 15-30 seconds
- **Batch processing**: 60-120 seconds
- **Behind NAT/Firewall**: Use shorter periods (15-30s)

## Dynamic Adjustment

The pool automatically adjusts its behavior based on usage patterns:

### Capacity Adjustment
- **Increases capacity** when stream creation success rate is high (>80%)
- **Decreases capacity** when stream creation success rate is low (<20%)
- Stays within configured `minCap` and `maxCap` bounds

### Interval Adjustment
- **Decreases interval** when pool utilization is low (<20% filled)
- **Increases interval** when pool utilization is high (>80% filled)
- Stays within configured `minIvl` and `maxIvl` bounds

```go
// Initial configuration
minCap, maxCap := 5, 20
minIvl, maxIvl := 500*time.Millisecond, 5*time.Second

// Pool starts at minCap (5) with minIvl (500ms)
// If streams are consumed quickly and creation succeeds:
// - Capacity increases toward maxCap (20)
// - Interval may adjust based on pool fullness

// If stream creation fails or streams aren't used:
// - Capacity decreases toward minCap (5)
// - Interval may increase to reduce creation attempts
```

## Advanced Usage

### Custom Address Resolver

The address resolver function allows dynamic server selection:

```go
// Simple static resolver
addrResolver := func() (string, error) {
    return "server.example.com:443", nil
}

// Dynamic resolver with load balancing
servers := []string{
    "server1.example.com:443",
    "server2.example.com:443",
    "server3.example.com:443",
}
var serverIndex int

addrResolver := func() (string, error) {
    server := servers[serverIndex%len(servers)]
    serverIndex++
    return server, nil
}

// Resolver with service discovery
addrResolver := func() (string, error) {
    addr, err := consul.GetService("http2-service")
    if err != nil {
        return "", err
    }
    return addr, nil
}
```

### Graceful Shutdown

```go
// Create pool with context
clientPool := http2.NewClientPool(
    5, 20,
    500*time.Millisecond, 5*time.Second,
    30*time.Second,
    "2",
    "example.com",
    addrResolver,
)

// Start manager
go clientPool.ClientManager()

// Handle shutdown signal
sigChan := make(chan os.Signal, 1)
signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

<-sigChan
log.Println("Shutting down...")

// Close pool gracefully
clientPool.Close()
```

### Error Handling

```go
// Monitor errors
go func() {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()
    
    for range ticker.C {
        errors := pool.ErrorCount()
        if errors > 100 {
            log.Printf("High error count: %d", errors)
            pool.ResetError()
            
            // Take corrective action
            pool.Flush()
        }
    }
}()
```

## Performance Considerations

### Stream Pool Sizing

| Pool Size | Pros | Cons | Best For |
|-----------|------|------|----------|
| Too Small (< 5) | Low resource usage | Stream contention, delays | Low-traffic applications |
| Optimal (5-50) | Balanced performance | Requires monitoring | Most applications |
| Too Large (> 100) | No contention | Resource waste, server overload | High-traffic, many clients |

**Sizing Guidelines:**
- Start with `minCap = baseline_load` and `maxCap = peak_load × 1.5`
- Monitor stream usage with `pool.Active()` and `pool.Capacity()`
- Adjust based on observed patterns

### Connection Overhead
- HTTP/2 uses a single TCP connection for all streams
- Initial connection establishment includes TLS handshake
- Consider keep-alive settings for long-running connections

### TLS Performance Impact

**Note: This library requires TLS for all connections.**

| Aspect | InsecureSkipVerify (Mode 0/1) | Verified TLS (Mode 2) |
|--------|-------------------------------|----------------------|
| **Handshake Time** | ~10-50ms | ~50-100ms |
| **Memory Usage** | Medium | High |
| **CPU Overhead** | Medium | High |
| **Throughput** | ~80-90% of max | ~60-80% of max |
| **Security** | Medium (encrypted but no cert validation) | High (full validation) |

### Stream Creation
- Pre-created streams are available immediately
- Dynamic adjustment optimizes for usage patterns
- Configure `minCap` and `maxCap` based on expected load

### Memory Usage
- Each stream consumes memory for buffers
- Monitor with `pool.Active()` and `pool.Capacity()`
- Use `pool.Flush()` to reclaim memory if needed

### Latency
- Pre-created streams have near-zero latency
- Stream creation on-demand adds network roundtrip
- Use appropriate `minCap` to maintain ready streams

### Stream Validation Overhead
- HTTP/2 connection validation happens at connection level
- **Cost**: ~1-5ms per validation (PING frame roundtrip)
- **Frequency**: As configured by keep-alive period
- **Trade-off**: Reliability vs. slight performance overhead

## Troubleshooting

### Common Issues

#### 1. Connection Timeout
**Symptoms:** Connections fail to establish  
**Solutions:**
- Check network connectivity to target host
- Verify server address and port are correct
- Increase connection timeout in address resolver:
  ```go
  addrResolver := func() (string, error) {
      // Ensure server is reachable
      return "example.com:443", nil
  }
  ```

#### 2. TLS Handshake Failure
**Symptoms:** TLS connections fail with certificate errors  
**Solutions:**
- Verify certificate validity and expiration
- Check hostname matches certificate Common Name
- For testing, temporarily use TLS mode `"1"`:
  ```go
  // Temporary workaround for testing
  pool := http2.NewClientPool(5, 20, minIvl, maxIvl, keepAlive, "1", hostname, addrResolver)
  ```

#### 3. Pool Exhaustion
**Symptoms:** `IncomingGet()` or `OutgoingGet()` times out  
**Solutions:**
- Increase maximum capacity
- Reduce stream hold time in application code
- Check for stream leaks (ensure streams are properly closed)
- Monitor with `pool.Active()` and `pool.ErrorCount()`
- Use appropriate timeout values

#### 4. High Error Rate
**Symptoms:** Frequent stream creation failures  
**Solutions:**
- Check server-side issues
- Track errors with `pool.AddError()` and `pool.ErrorCount()`
- Implement exponential backoff

### Streams Not Available
```go
stream, err := pool.OutgoingGet(id, 10*time.Second)
if err != nil {
    // Check if pool is running
    if !pool.Ready() {
        log.Println("Pool not initialized - did you start ClientManager?")
        return
    }
    
    // Check pool statistics
    log.Printf("Active streams: %d, Capacity: %d", 
        pool.Active(), pool.Capacity())
    
    // Increase timeout or capacity if needed
}
```

### Connection Failures
```go
// Monitor errors
if pool.ErrorCount() > threshold {
    log.Println("High error rate detected")
    
    // Check network connectivity
    // Verify server address
    // Check TLS configuration
    // Review firewall rules
    
    pool.ResetError()
}
```

### TLS Certificate Issues
```go
// For development, use mode "0" or "1"
clientPool := http2.NewClientPool(
    5, 20,
    500*time.Millisecond, 5*time.Second,
    30*time.Second,
    "1",  // Skip certificate verification
    "example.com",
    addrResolver,
)

// For production, use mode "2" with valid certificates
// Ensure hostname matches certificate CN/SAN
```

### Debugging Checklist

- [ ] **Network connectivity**: Can you reach the target server?
- [ ] **Port availability**: Is the target port open and listening?
- [ ] **Certificate validity**: For TLS, are certificates valid and not expired?
- [ ] **Pool capacity**: Is `maxCap` sufficient for your load?
- [ ] **Stream leaks**: Are you properly closing streams with `defer stream.Close()`?
- [ ] **Error monitoring**: Are you tracking `pool.ErrorCount()`?
- [ ] **Manager running**: Did you start `ClientManager()` or `ServerManager()`?

### Debug Logging

Add logging at key points for better debugging:

```go
// Client side logging
go func() {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for range ticker.C {
        log.Printf("Pool stats - Active: %d, Capacity: %d, Errors: %d",
            pool.Active(), pool.Capacity(), pool.ErrorCount())
    }
}()

// Track address resolution
addrResolver := func() (string, error) {
    addr := "example.com:443"
    log.Printf("Resolving address: %s", addr)
    return addr, nil
}
```

## Best Practices

### 1. Pool Configuration

#### Choose Appropriate Capacity
```go
// Low traffic (1-10 concurrent streams)
minCap, maxCap := 1, 10

// Medium traffic (10-50 concurrent streams)
minCap, maxCap := 10, 50

// High traffic (50+ concurrent streams)
minCap, maxCap := 20, 200
```

#### Configure Intervals Wisely
```go
// Fast-paced applications
minIvl, maxIvl := 100*time.Millisecond, 2*time.Second

// Moderate pace
minIvl, maxIvl := 500*time.Millisecond, 5*time.Second

// Slow-paced or resource-constrained
minIvl, maxIvl := 1*time.Second, 10*time.Second
```

### 2. Stream Management

#### Always Close Streams
```go
stream, err := pool.OutgoingGet(id, timeout)
if err != nil {
    return err
}
defer stream.Close()  // Essential!

// Use the stream...
```

#### Handle Timeouts Appropriately
```go
// Short timeout for real-time applications
stream, err := pool.OutgoingGet(id, 1*time.Second)

// Longer timeout for less time-sensitive operations
stream, err := pool.OutgoingGet(id, 30*time.Second)
```

### 3. Error Handling and Monitoring

#### Implement Health Checks
```go
func monitorPool(pool *http2.Pool) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    
    for range ticker.C {
        if !pool.Ready() {
            log.Println("Pool not ready!")
            continue
        }
        
        active := pool.Active()
        capacity := pool.Capacity()
        errors := pool.ErrorCount()
        
        log.Printf("Pool health - Active: %d/%d, Errors: %d", 
            active, capacity, errors)
        
        if errors > 50 {
            log.Println("High error count, resetting...")
            pool.ResetError()
        }
    }
}
```

### 4. Production Deployment

#### Use Verified TLS Certificates
```go
// Production configuration
clientPool := http2.NewClientPool(
    10, 100,
    500*time.Millisecond, 5*time.Second,
    30*time.Second,
    "2",  // Verified certificates only
    "api.production.com",
    addrResolver,
)
```

#### Implement Graceful Shutdown
```go
// Setup signal handling
sigChan := make(chan os.Signal, 1)
signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

// Wait for signal
<-sigChan

// Graceful shutdown
log.Println("Shutting down...")
pool.Close()
time.Sleep(1 * time.Second)  // Allow connections to close
```

### 5. Performance Optimization

#### Avoid Common Anti-patterns
```go
// ANTI-PATTERN: Creating pools repeatedly
func badHandler(w http.ResponseWriter, r *http.Request) {
    // DON'T: Create a new pool for each request
    pool := http2.NewClientPool(5, 10, time.Second, time.Second, 30*time.Second, "2", "api.com", addrResolver)
    defer pool.Close()
}

// GOOD PATTERN: Reuse pools
type Server struct {
    apiPool *http2.Pool // Shared pool instance
}

func (s *Server) goodHandler(w http.ResponseWriter, r *http.Request) {
    // DO: Reuse existing pool
    stream, err := s.apiPool.OutgoingGet(id, 10*time.Second)
    if err != nil {
        http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
        return
    }
    defer stream.Close()
    // Use stream...
}
```

#### Optimize for Your Use Case
```go
// High-throughput, low-latency services
highThroughputPool := http2.NewClientPool(
    50, 200,                           // Large pool for many concurrent streams
    100*time.Millisecond, 1*time.Second, // Fast stream creation
    15*time.Second,                    // Short keep-alive for quick failure detection
    "2", "fast-api.com", addrResolver,
)

// Batch processing, memory-constrained services
batchPool := http2.NewClientPool(
    5, 20,                             // Smaller pool to conserve memory
    2*time.Second, 10*time.Second,     // Slower stream creation
    60*time.Second,                    // Longer keep-alive for stable connections
    "2", "batch-api.com", addrResolver,
)
```

#### Pre-warm the Pool
```go
pool := http2.NewClientPool(...)
go pool.ClientManager()

// Wait for initial streams to be created
time.Sleep(2 * time.Second)

// Now start serving traffic
```

#### Monitor and Adjust
```go
// Periodically review pool statistics
stats := map[string]interface{}{
    "active":   pool.Active(),
    "capacity": pool.Capacity(),
    "interval": pool.Interval(),
    "errors":   pool.ErrorCount(),
}

// Adjust configuration if needed
// - Increase maxCap if pool is consistently full
// - Decrease minCap if utilization is low
// - Adjust intervals based on creation patterns
```

### 6. Testing and Development

#### Development Configuration
```go
// Development/testing setup
func createDevPool() *http2.Pool {
    return http2.NewClientPool(
        2, 5,                           // Smaller pool for development
        time.Second, 3*time.Second,
        30*time.Second,
        "1",                           // InsecureSkipVerify acceptable for dev
        "localhost",                   // Local development hostname
        func() (string, error) {
            return "localhost:8443", nil
        },
    )
}
```

#### Unit Testing with Pools
```go
func TestPoolIntegration(t *testing.T) {
    // Create test TLS certificate
    cert, err := tls.LoadX509KeyPair("test-cert.pem", "test-key.pem")
    require.NoError(t, err)
    
    tlsConfig := &tls.Config{
        Certificates: []tls.Certificate{cert},
    }
    
    // Start server pool (TLS required)
    serverPool := http2.NewServerPool(
        5,
        "",  // Allow any IP for testing
        tlsConfig, // TLS is mandatory
        "localhost:0",
        10*time.Second,
    )
    go serverPool.ServerManager()
    defer serverPool.Close()
    
    // Start client pool (use mode "1" for testing with self-signed certs)
    clientPool := http2.NewClientPool(
        2, 5,
        time.Second, 3*time.Second,
        10*time.Second,
        "1", // InsecureSkipVerify for testing
        "localhost",
        func() (string, error) {
            return "localhost:8443", nil
        },
    )
    go clientPool.ClientManager()
    defer clientPool.Close()
    
    // Wait for initialization
    time.Sleep(100 * time.Millisecond)
    
    // Test getting stream
    id, stream, err := serverPool.IncomingGet(5 * time.Second)
    require.NoError(t, err)
    require.NotNil(t, stream)
    require.NotEmpty(t, id)
    defer stream.Close()
    
    // Test client get stream
    clientStream, err := clientPool.OutgoingGet(id, 5*time.Second)
    require.NoError(t, err)
    require.NotNil(t, clientStream)
    defer clientStream.Close()
    
    // Test write and read
    testData := []byte("test message")
    _, err = clientStream.Write(testData)
    require.NoError(t, err)
    
    buf := make([]byte, len(testData))
    _, err = stream.Read(buf)
    require.NoError(t, err)
    require.Equal(t, testData, buf)
    
    // Test error case - non-existent ID
    _, err = clientPool.OutgoingGet("non-existent-id", 1*time.Millisecond)
    require.Error(t, err)
    
    // Test timeout case
    _, _, err = serverPool.IncomingGet(1 * time.Millisecond)
    require.Error(t, err)
}
```

**These best practices will help you get the most out of the HTTP/2 stream pool package while maintaining reliability and performance in production environments.**

## License

This project is licensed under the BSD 3-Clause License - see the [LICENSE](LICENSE) file for details.
