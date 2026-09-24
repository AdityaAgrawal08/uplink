package lan

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

var RequestsSeen int32
var DownloadsCompleted int32

// ActiveConnections is kept for backward compatibility (tests/scripts may
// reference it); it aliases RequestsSeen which counts handshake attempts.
var ActiveConnections int32

func GetActiveConnections() int32 {
	return atomic.LoadInt32(&RequestsSeen)
}

// progressWriter wraps http.ResponseWriter to track bytes written.
type progressWriter struct {
	http.ResponseWriter
	total    int64
	written  int64
	lastProg int64
	onProg   func(written, total int64)
	once     sync.Once
	serveErr error
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.ResponseWriter.Write(p)
	if err != nil {
		pw.serveErr = err
	}
	pw.written += int64(n)
	if pw.onProg != nil {
		pw.once.Do(func() {
			pw.onProg(pw.written, pw.total)
		})
		// Throttle callbacks to ~every 256KB (synchronous, no goroutine
		// storm; caller must still guard shared printer with a mutex).
		if pw.written-pw.lastProg >= 256*1024 {
			pw.lastProg = pw.written
			pw.onProg(pw.written, pw.total)
		}
	}
	return n, err
}

// Flush passthrough preserves http.Flusher so ServeFile keeps streaming.
func (pw *progressWriter) Flush() {
	if f, ok := pw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func ServeFileLAN(ctx context.Context, path string, port int, cert tls.Certificate, shareCode string, password string, downloadLimit int, onComplete func()) error {
	return ServeFileLANWithProgress(ctx, path, port, cert, shareCode, password, downloadLimit, nil, onComplete)
}

func ServeFileLANWithProgress(ctx context.Context, path string, port int, cert tls.Certificate, shareCode string, password string, downloadLimit int, onProgress func(written, total int64), onComplete func()) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr: net.JoinHostPort("0.0.0.0", strconv.Itoa(port)),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	var completeOnce sync.Once

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&RequestsSeen, 1)
		atomic.AddInt32(&ActiveConnections, 1)

		// 1. Enforce share code matching
		clientShareCode := r.Header.Get("X-Uplink-Share-Code")
		if subtle.ConstantTimeCompare([]byte(clientShareCode), []byte(shareCode)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte("Unauthorized: share code mismatch"))
			return
		}

		// 2. Enforce password protection
		if password != "" {
			clientPassword := r.Header.Get("X-Uplink-Password")
			if subtle.ConstantTimeCompare([]byte(clientPassword), []byte(password)) != 1 {
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte("Forbidden: invalid password"))
				return
			}
		}

		// 3. Enforce download limits
		var reserved bool
		if downloadLimit > 0 && r.Method == "GET" {
			for {
				completed := atomic.LoadInt32(&DownloadsCompleted)
				if int(completed) >= downloadLimit {
					w.WriteHeader(http.StatusGone)
					w.Write([]byte("Gone: download limit exceeded"))
					return
				}
				if atomic.CompareAndSwapInt32(&DownloadsCompleted, completed, completed+1) {
					reserved = true
					break
				}
			}
		}

		// 4. Validate file state changes (mtime)
		currentInfo, err := os.Stat(path)
		if err != nil || currentInfo.ModTime().Unix() != info.ModTime().Unix() || currentInfo.Size() != info.Size() {
			if reserved {
				atomic.AddInt32(&DownloadsCompleted, -1)
			}
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte("Conflict: file modified since server start"))
			return
		}

		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
		w.Header().Set("ETag", fmt.Sprintf(`"%x-%x"`, info.ModTime().Unix(), info.Size()))

		// Byte-exact completion: only fire onComplete when ServeFile wrote
		// the full object without error and the client did not disconnect.
		// The old r.Context().Err() check is unreliable (cancellation often
		// surfaces after the handler returns).
		var servedBytes int64
		var serveErr error
		if r.Method == "GET" && onProgress != nil {
			pw := &progressWriter{ResponseWriter: w, total: info.Size(), onProg: onProgress}
			http.ServeFile(pw, r, path)
			servedBytes = pw.written
			serveErr = pw.serveErr
		} else if r.Method == "GET" {
			cw := &progressWriter{ResponseWriter: w, total: info.Size()}
			http.ServeFile(cw, r, path)
			servedBytes = cw.written
			serveErr = cw.serveErr
		} else {
			http.ServeFile(w, r, path)
		}

		// Serve completed, run completion callback
		if r.Method == "GET" {
			if serveErr != nil || servedBytes != info.Size() || r.Context().Err() != nil {
				if reserved {
					atomic.AddInt32(&DownloadsCompleted, -1)
				}
			} else {
				go func() {
					completeOnce.Do(func() {
						if onComplete != nil {
							onComplete()
						}
					})
				}()
			}
		}
	})

	server.Handler = handler

	go func() {
		<-ctx.Done()
		_ = server.Shutdown(context.Background())
	}()

	err = server.ListenAndServeTLS("", "")
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
