// Package fixture supplies deterministic HTTP behaviors for Inspect development.
package fixture

import (
	"fmt"
	"net/http"
	"time"
)

type Config struct {
	HeaderDelay time.Duration
	ChunkDelay  time.Duration
}

func wait(r *http.Request, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-r.Context().Done():
		return false
	}
}

func New(c Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) {
		if wait(r, c.HeaderDelay) {
			fmt.Fprint(w, "headers delayed\n")
		}
	})
	mux.HandleFunc("/body", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		for i := 0; i < 4; i++ {
			if !wait(r, c.ChunkDelay) {
				return
			}
			if _, err := fmt.Fprintf(w, "chunk %d\n", i); err != nil {
				return
			}
			http.NewResponseController(w).Flush()
		}
	})
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/empty", http.StatusFound)
	})
	mux.HandleFunc("/empty", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/disconnect", func(w http.ResponseWriter, r *http.Request) {
		h, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "HTTP/1 hijacking required", http.StatusNotImplemented)
			return
		}
		conn, out, err := h.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(out, "HTTP/1.1 200 OK\r\nContent-Length: 100\r\nConnection: close\r\n\r\nshort")
		out.Flush()
	})
	mux.HandleFunc("/timeout", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	return mux
}
