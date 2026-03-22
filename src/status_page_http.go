package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

func (sp *startupStatusPage) Start() {
	sp.mu.Lock()
	if sp.running {
		sp.mu.Unlock()
		return
	}

	for {
		select {
		case <-sp.wakeCh:
		default:
			goto drained
		}
	}

drained:
	sp.servers = make(map[uint16]*http.Server)
	sp.listeners = make(map[uint16]net.Listener)
	sp.wakeRequestedAt = time.Time{}

	mux := http.NewServeMux()

	mux.HandleFunc(statusCSSRoute, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(sp.getStatusCSS()))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == statusCSSRoute {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(sp.getStatusCSS()))
			return
		}

		sp.MarkWakeRequested()
		remaining := sp.startupTiming()

		select {
		case sp.wakeCh <- struct{}{}:
		default:
		}

		if expectsJSON(r) {
			writeAPIWaitResponse(w, sp, remaining)
			return
		}

		sp.renderStatusPage(w, remaining)
	})

	sp.registerQuizRoutes(mux)

	startedListener := 0
	for _, p := range sp.ports {
		port := p
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(int(port)))
		if err != nil {
			debugLogger.Printf("status page: could not listen on port %d for group %s: %v\n", port, sp.groupName, err)
			continue
		}

		srv := &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}

		sp.listeners[port] = ln
		sp.servers[port] = srv
		startedListener++

		go func(prt uint16, server *http.Server, listener net.Listener) {
			err := server.Serve(listener)
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				debugLogger.Printf("status page: server on port %d for group %s exited with error: %v\n", prt, sp.groupName, err)
			}
		}(port, srv, ln)
	}

	sp.running = startedListener > 0
	sp.mu.Unlock()

	if startedListener > 0 {
		infoLogger.Printf("status page enabled for group %s on %d port(s)\n", sp.groupName, startedListener)
	}
}

func (sp *startupStatusPage) Stop() {
	sp.mu.Lock()
	if !sp.running {
		sp.mu.Unlock()
		return
	}

	servers := sp.servers
	listeners := sp.listeners
	sp.servers = nil
	sp.listeners = nil
	sp.running = false
	sp.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	for port, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			debugLogger.Printf("status page: shutdown error on port %d for group %s: %v\n", port, sp.groupName, err)
		}
	}

	for _, ln := range listeners {
		_ = ln.Close()
	}
}
