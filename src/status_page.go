package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

type startupStatusPage struct {
	groupName string
	ports     []uint16

	mu        sync.Mutex
	servers   map[uint16]*http.Server
	listeners map[uint16]net.Listener
	running   bool
	wakeCh    chan struct{}

	configuredEstimate time.Duration
	observedEstimate   time.Duration
	wakeRequestedAt    time.Time
}

func newStartupStatusPage(groupName string, ports []uint16, configuredEstimate time.Duration) *startupStatusPage {
	if configuredEstimate <= 0 {
		configuredEstimate = 30 * time.Second
	}

	return &startupStatusPage{
		groupName:          groupName,
		ports:              ports,
		wakeCh:             make(chan struct{}, 1),
		configuredEstimate: configuredEstimate,
	}
}

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

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sp.MarkWakeRequested()
		remaining, estimated := sp.startupTiming()

		select {
		case sp.wakeCh <- struct{}{}:
		default:
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>Container wird gestartet</title></head><body style=\"font-family: sans-serif; margin: 2rem; line-height: 1.5;\"><h1>Container startet gerade</h1><p>Die Anwendung in Gruppe <strong>%s</strong> wird gerade hochgefahren und ist in Kuerze verfuegbar.</p><p>Geschaetzte Startdauer: <strong>%d Sekunden</strong></p><p>Voraussichtlich verbleibend: <strong>%d Sekunden</strong></p><p>Bitte aktualisiere die Seite in ein paar Sekunden erneut.</p></body></html>", sp.groupName, estimated, remaining)
	})

	startedListener := 0
	for _, p := range sp.ports {
		port := p
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(int(port)))
		if err != nil {
			debugLogger.Printf("status page: could not listen on port %d for group %s: %v\n", port, sp.groupName, err)
			continue
		}

		srv := &http.Server{
			Handler:           handler,
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

func (sp *startupStatusPage) ConsumeStartSignal() bool {
	select {
	case <-sp.wakeCh:
		return true
	default:
		return false
	}
}

func (sp *startupStatusPage) MarkWakeRequested() {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.wakeRequestedAt.IsZero() {
		sp.wakeRequestedAt = time.Now()
	}
}

func (sp *startupStatusPage) RecordStartupDuration(d time.Duration) {
	if d <= 0 {
		return
	}

	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.observedEstimate == 0 {
		sp.observedEstimate = d
	} else {
		// Smooth startup estimates so one slow start does not dominate future ETAs.
		sp.observedEstimate = (sp.observedEstimate*3 + d) / 4
	}
	sp.wakeRequestedAt = time.Time{}
}

func (sp *startupStatusPage) startupTiming() (remainingSeconds int, estimatedSeconds int) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	estimate := sp.configuredEstimate
	if sp.observedEstimate > 0 {
		estimate = sp.observedEstimate
	}

	remaining := estimate
	if !sp.wakeRequestedAt.IsZero() {
		elapsed := time.Since(sp.wakeRequestedAt)
		remaining = estimate - elapsed
		if remaining < 0 {
			remaining = 0
		}
	}

	estimatedSeconds = int(math.Ceil(estimate.Seconds()))
	remainingSeconds = int(math.Ceil(remaining.Seconds()))
	if estimatedSeconds < 1 {
		estimatedSeconds = 1
	}
	if remainingSeconds < 0 {
		remainingSeconds = 0
	}

	return remainingSeconds, estimatedSeconds
}
