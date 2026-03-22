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
		_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="de">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>Container wird gestartet</title>
	<style>
		:root {
			--bg-top: #071b2e;
			--bg-bottom: #1b3a57;
			--card: #f4f8fb;
			--text: #102131;
			--muted: #4d6274;
			--accent: #147bd1;
			--accent-2: #46b4e8;
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
			background: radial-gradient(circle at 10%% 20%%, #2f5f84 0%%, transparent 45%%),
									radial-gradient(circle at 85%% 85%%, #0d8f9a 0%%, transparent 45%%),
									linear-gradient(160deg, var(--bg-top), var(--bg-bottom));
			font-family: "Segoe UI", "Noto Sans", sans-serif;
			padding: 24px;
		}

		.card {
			width: min(680px, 100%%);
			background: linear-gradient(180deg, #ffffff 0%%, var(--card) 100%%);
			border-radius: 18px;
			box-shadow: var(--shadow);
			padding: 30px;
			position: relative;
			overflow: hidden;
		}

		.card::after {
			content: "";
			position: absolute;
			inset: 0;
			background: linear-gradient(120deg, rgba(20, 123, 209, 0.08), rgba(70, 180, 232, 0.08));
			pointer-events: none;
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
			border-radius: 50%%;
			background: var(--ok);
			box-shadow: 0 0 0 0 rgba(15, 143, 103, 0.55);
			animation: pulse 1.6s infinite;
		}

		@keyframes pulse {
			0%% { box-shadow: 0 0 0 0 rgba(15, 143, 103, 0.55); }
			70%% { box-shadow: 0 0 0 12px rgba(15, 143, 103, 0); }
			100%% { box-shadow: 0 0 0 0 rgba(15, 143, 103, 0); }
		}

		h1 {
			margin: 0;
			font-size: clamp(1.35rem, 2.5vw, 1.8rem);
			letter-spacing: 0.01em;
		}

		p {
			margin: 10px 0;
			color: var(--muted);
			line-height: 1.5;
		}

		.stats {
			margin-top: 22px;
			display: grid;
			grid-template-columns: repeat(2, minmax(0, 1fr));
			gap: 12px;
		}

		.stat {
			background: #e9f3fb;
			border: 1px solid #d3e4f5;
			border-radius: 12px;
			padding: 14px;
		}

		.stat-label {
			font-size: 0.86rem;
			color: #4c6274;
			margin-bottom: 6px;
		}

		.stat-value {
			font-size: 1.4rem;
			color: var(--text);
			font-weight: 700;
			font-variant-numeric: tabular-nums;
		}

		.progress {
			margin-top: 18px;
			width: 100%%;
			height: 12px;
			background: #d6e6f4;
			border-radius: 999px;
			overflow: hidden;
		}

		.progress-bar {
			width: 0%%;
			height: 100%%;
			background: linear-gradient(90deg, var(--accent), var(--accent-2));
			transition: width 0.8s ease;
		}

		.tiny {
			margin-top: 12px;
			font-size: 0.85rem;
			color: #597387;
		}

		@media (max-width: 620px) {
			.card { padding: 20px; }
			.stats { grid-template-columns: 1fr; }
		}
	</style>
</head>
<body>
	<main class="card">
		<div class="header">
			<span class="dot" aria-hidden="true"></span>
			<h1>Container startet gerade</h1>
		</div>

		<p>Die Anwendung in Gruppe <strong>%s</strong> wird gerade hochgefahren und ist in Kuerze verfuegbar.</p>

		<section class="stats" aria-live="polite">
			<article class="stat">
				<div class="stat-label">Geschaetzte Startdauer</div>
				<div class="stat-value"><span id="estimated">%d</span> s</div>
			</article>
			<article class="stat">
				<div class="stat-label">Voraussichtlich verbleibend</div>
				<div class="stat-value"><span id="remaining">%d</span> s</div>
			</article>
		</section>

		<div class="progress" role="progressbar" aria-label="Startfortschritt" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0">
			<div class="progress-bar" id="progress"></div>
		</div>
		<p class="tiny">Die Anzeige aktualisiert sich live. Falls der Dienst bereits laeuft, bitte Seite neu laden.</p>
	</main>

	<script>
		(function () {
			const estimated = Math.max(1, Number(%d));
			let remaining = Math.max(0, Number(%d));
			let reloading = false;

			const estimatedEl = document.getElementById("estimated");
			const remainingEl = document.getElementById("remaining");
			const progressEl = document.getElementById("progress");
			const progressWrap = document.querySelector(".progress");

			function reloadNow() {
				if (reloading) {
					return;
				}
				reloading = true;
				window.location.reload();
			}

			function render() {
				estimatedEl.textContent = String(estimated);
				remainingEl.textContent = String(remaining);

				const done = Math.max(0, Math.min(100, ((estimated - remaining) / estimated) * 100));
				progressEl.style.width = done.toFixed(1) + "%%";
				progressWrap.setAttribute("aria-valuenow", String(Math.round(done)));
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
</html>`, sp.groupName, estimated, remaining, estimated, remaining)
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
