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
)

var ActiveConnections int32
var DownloadsCompleted int32

func GetActiveConnections() int32 {
	return atomic.LoadInt32(&ActiveConnections)
}

func ServeFileLAN(ctx context.Context, path string, port int, cert tls.Certificate, shareCode string, password string, downloadLimit int, onComplete func()) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr: net.JoinHostPort("0.0.0.0", strconv.Itoa(port)),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
		},
	}

	var completeOnce sync.Once

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

		http.ServeFile(w, r, path)

		// Serve completed, run completion callback
		if r.Method == "GET" {
			if r.Context().Err() != nil {
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
