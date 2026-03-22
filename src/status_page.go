package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	defaultStatusCSSPath = "/app/status_page.css"
	statusCSSRoute       = "/_lazytainer/status-page.css"
	defaultEstimatePath  = "/tmp/lazytainer_startup_estimates.json"
)

var startupEstimateStore = struct {
	mu     sync.Mutex
	loaded bool
	path   string
	values map[string]int64
}{
	path:   defaultEstimatePath,
	values: map[string]int64{},
}

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
	cssPath            string
}

func newStartupStatusPage(groupName string, ports []uint16, configuredEstimate time.Duration) *startupStatusPage {
	if configuredEstimate <= 0 {
		configuredEstimate = 30 * time.Second
	}

	storedEstimate := getStoredStartupEstimate(groupName)

	return &startupStatusPage{
		groupName:          groupName,
		ports:              ports,
		wakeCh:             make(chan struct{}, 1),
		configuredEstimate: configuredEstimate,
		observedEstimate:   storedEstimate,
		cssPath:            cssPathFromEnv(),
	}
}

func estimatePathFromEnv() string {
	path := os.Getenv("STARTUP_ESTIMATE_FILE")
	if path == "" {
		return defaultEstimatePath
	}

	return path
}

func loadStartupEstimatesLocked() {
	if startupEstimateStore.loaded {
		return
	}

	startupEstimateStore.path = estimatePathFromEnv()
	startupEstimateStore.loaded = true

	raw, err := os.ReadFile(startupEstimateStore.path)
	if err != nil {
		return
	}

	var loaded map[string]int64
	if err := json.Unmarshal(raw, &loaded); err != nil {
		return
	}

	startupEstimateStore.values = loaded
}

func getStoredStartupEstimate(groupName string) time.Duration {
	startupEstimateStore.mu.Lock()
	defer startupEstimateStore.mu.Unlock()

	loadStartupEstimatesLocked()
	seconds, exists := startupEstimateStore.values[groupName]
	if !exists || seconds <= 0 {
		return 0
	}

	return time.Duration(seconds) * time.Second
}

func saveStartupEstimate(groupName string, d time.Duration) {
	startupEstimateStore.mu.Lock()
	defer startupEstimateStore.mu.Unlock()

	loadStartupEstimatesLocked()
	startupEstimateStore.values[groupName] = int64(d.Round(time.Second) / time.Second)

	raw, err := json.Marshal(startupEstimateStore.values)
	if err != nil {
		return
	}

	if err := os.WriteFile(startupEstimateStore.path, raw, 0o644); err != nil {
		return
	}
}

func cssPathFromEnv() string {
	cssPath := os.Getenv("STATUS_PAGE_CSS_FILE")
	if cssPath == "" {
		return defaultStatusCSSPath
	}

	return cssPath
}

func (sp *startupStatusPage) getStatusCSS() string {
	cssBytes, err := os.ReadFile(sp.cssPath)
	if err != nil {
		debugLogger.Printf("status page: could not read CSS file %s for group %s: %v\n", sp.cssPath, sp.groupName, err)
		return `:root {
	--bg-top: #071b2e;
	--bg-bottom: #1b3a57;
	--card: #f4f8fb;
	--text: #102131;
	--muted: #4d6274;
	--ok: #0f8f67;
	--shadow: 0 24px 50px rgba(5, 20, 33, 0.35);
}

* { box-sizing: border-box; }

body {
	margin: 0;
	min-height: 100vh;
	display: grid;
	place-items: center;
	color: var(--text);
	background: radial-gradient(circle at 10% 20%, #2f5f84 0%, transparent 45%),
							radial-gradient(circle at 85% 85%, #0d8f9a 0%, transparent 45%),
							linear-gradient(160deg, var(--bg-top), var(--bg-bottom));
	font-family: "Segoe UI", "Noto Sans", sans-serif;
	padding: 24px;
}

.card {
	width: min(640px, 100%);
	background: linear-gradient(180deg, #ffffff 0%, var(--card) 100%);
	border-radius: 18px;
	box-shadow: var(--shadow);
	padding: 30px;
}

.header {
	display: flex;
	align-items: center;
	gap: 12px;
	margin-bottom: 16px;
}

.dot {
	width: 12px;
	height: 12px;
	border-radius: 50%;
	background: var(--ok);
	box-shadow: 0 0 0 0 rgba(15, 143, 103, 0.55);
	animation: pulse 1.6s infinite;
}

@keyframes pulse {
	0% { box-shadow: 0 0 0 0 rgba(15, 143, 103, 0.55); }
	70% { box-shadow: 0 0 0 12px rgba(15, 143, 103, 0); }
	100% { box-shadow: 0 0 0 0 rgba(15, 143, 103, 0); }
}

h1 {
	margin: 0;
	font-size: clamp(1.35rem, 2.5vw, 1.8rem);
}

p {
	margin: 10px 0;
	color: var(--muted);
	line-height: 1.5;
}

.countdown {
	margin-top: 20px;
	font-size: clamp(2rem, 8vw, 3.4rem);
	font-weight: 800;
	font-variant-numeric: tabular-nums;
	color: var(--text);
}

.countdown-label {
	margin-top: 4px;
	font-size: 0.9rem;
	color: #597387;
}

@media (max-width: 620px) {
	.card { padding: 20px; }
}`
	}

	return string(cssBytes)
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

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="de">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>Container wird gestartet</title>
	<link rel="stylesheet" href="`+statusCSSRoute+`">
</head>
<body>
	<main class="card">
		<div class="header">
			<span class="dot" aria-hidden="true"></span>
			<h1>Container startet gerade</h1>
		</div>

		<p>Die Anwendung in Gruppe <strong>%s</strong> wird gerade hochgefahren und ist in Kuerze verfuegbar.</p>
		<div class="countdown" aria-live="polite"><span id="remaining">%d</span> s</div>
		<p class="countdown-label">Verbleibend bis zum automatischen Reload</p>
	</main>

	<script>
		(function () {
			let remaining = Math.max(0, Number(%d));
			let reloading = false;

			const remainingEl = document.getElementById("remaining");

			function reloadNow() {
				if (reloading) {
					return;
				}
				reloading = true;
				window.location.reload();
			}

			function render() {
				remainingEl.textContent = String(remaining);
			}

			render();
			if (remaining === 0) {
				reloadNow();
				return;
			}

			setInterval(function () {
				if (remaining > 0) {
					remaining -= 1;
					render();
					if (remaining === 0) {
						reloadNow();
					}
				}
			}, 1000);
		})();
	</script>
</body>
	</html>`, sp.groupName, remaining, remaining)
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

	if d < 3*time.Second {
		d = 3 * time.Second
	}
	if d > 15*time.Minute {
		d = 15 * time.Minute
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
	saveStartupEstimate(sp.groupName, sp.observedEstimate)
}

func (sp *startupStatusPage) startupTiming() int {
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

	remainingSeconds := int(remaining.Seconds() + 0.999)
	if remainingSeconds < 0 {
		remainingSeconds = 0
	}

	return remainingSeconds
}
