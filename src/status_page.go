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
	"strings"
	"sync"
	"time"
)

const (
	defaultStatusCSSPath  = "/app/status_page.css"
	statusCSSRoute        = "/_lazytainer/status-page.css"
	defaultEstimatePath   = "/tmp/lazytainer_startup_estimates.json"
	defaultQuizzesPath    = "/app/game_quotes.json"
	defaultQuizScorePath  = "/tmp/lazytainer_quiz_scores.json"
	defaultLastAccessPath = "/tmp/lazytainer_last_access.json"
)

type quizOption struct {
	Text string `json:"text"`
}

type quizQuestion struct {
	ID           int      `json:"id"`
	Quote        string   `json:"quote"`
	Game         string   `json:"game"`
	Options      []string `json:"options"`
	CorrectIndex int      `json:"correctIndex"`
}

type quizzesData struct {
	Quizzes []quizQuestion `json:"quizzes"`
}

type quizSessionState struct {
	QuestionIndex int
	Score         int
	TotalAnswered int
}

var quizDataStore = struct {
	mu        sync.Mutex
	loaded    bool
	questions []quizQuestion
}{
	questions: []quizQuestion{},
}

var quizScoreStore = struct {
	mu     sync.Mutex
	loaded bool
	path   string
	values map[string]quizSessionState
}{
	path:   defaultQuizScorePath,
	values: map[string]quizSessionState{},
}

var lastAccessStore = struct {
	mu     sync.Mutex
	loaded bool
	path   string
	values map[string]string
}{
	path:   defaultLastAccessPath,
	values: map[string]string{},
}

type startupEstimateRecord struct {
	EstimateSeconds     int64  `json:"estimateSeconds"`
	LastDurationSeconds int64  `json:"lastDurationSeconds,omitempty"`
	LastCompletedAt     string `json:"lastCompletedAt,omitempty"`
}

var startupEstimateStore = struct {
	mu     sync.Mutex
	loaded bool
	path   string
	values map[string]startupEstimateRecord
}{
	path:   defaultEstimatePath,
	values: map[string]startupEstimateRecord{},
}

type startupStatusPage struct {
	groupName string
	ports     []uint16

	mu        sync.Mutex
	servers   map[uint16]*http.Server
	listeners map[uint16]net.Listener
	running   bool
	wakeCh    chan struct{}

	configuredEstimate  time.Duration
	observedEstimate    time.Duration
	lastStartupDuration time.Duration
	lastCompletedAt     time.Time
	wakeRequestedAt     time.Time
	cssPath             string
	quizEnabled         bool
	debugMode           bool
}

func newStartupStatusPage(groupName string, ports []uint16, configuredEstimate time.Duration) *startupStatusPage {
	if configuredEstimate <= 0 {
		configuredEstimate = 30 * time.Second
	}

	storedEstimate, lastDuration, lastCompletedAt := getStoredStartupData(groupName)

	return &startupStatusPage{
		groupName:           groupName,
		ports:               ports,
		wakeCh:              make(chan struct{}, 1),
		configuredEstimate:  configuredEstimate,
		observedEstimate:    storedEstimate,
		lastStartupDuration: lastDuration,
		lastCompletedAt:     lastCompletedAt,
		cssPath:             cssPathFromEnv(),
		quizEnabled:         quizEnabledFromEnv(),
		debugMode:           debugModeFromEnv(),
	}
}

func quizEnabledFromEnv() bool {
	value := strings.TrimSpace(os.Getenv("STATUS_PAGE_MINIGAME_ENABLED"))
	if value == "" {
		return true
	}

	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return true
	}

	return enabled
}

func debugModeFromEnv() bool {
	value := strings.TrimSpace(os.Getenv("STATUS_PAGE_DEBUG_MODE"))
	if value == "" {
		return false
	}

	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}

	return enabled
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

	var loaded map[string]startupEstimateRecord
	if err := json.Unmarshal(raw, &loaded); err == nil {
		startupEstimateStore.values = loaded
		return
	}

	// Backward compatibility for older format: {"group": <seconds>}.
	var legacy map[string]int64
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return
	}

	converted := make(map[string]startupEstimateRecord, len(legacy))
	for groupName, seconds := range legacy {
		converted[groupName] = startupEstimateRecord{EstimateSeconds: seconds}
	}
	startupEstimateStore.values = converted
}

func getStoredStartupData(groupName string) (estimate time.Duration, lastDuration time.Duration, lastCompletedAt time.Time) {
	startupEstimateStore.mu.Lock()
	defer startupEstimateStore.mu.Unlock()

	loadStartupEstimatesLocked()
	record, exists := startupEstimateStore.values[groupName]
	if !exists {
		return 0, 0, time.Time{}
	}

	if record.EstimateSeconds > 0 {
		estimate = time.Duration(record.EstimateSeconds) * time.Second
	}
	if record.LastDurationSeconds > 0 {
		lastDuration = time.Duration(record.LastDurationSeconds) * time.Second
	}
	if record.LastCompletedAt != "" {
		parsed, err := time.Parse(time.RFC3339, record.LastCompletedAt)
		if err == nil {
			lastCompletedAt = parsed
		}
	}

	return estimate, lastDuration, lastCompletedAt
}

func saveStartupEstimate(groupName string, estimate time.Duration, lastDuration time.Duration, lastCompletedAt time.Time) {
	startupEstimateStore.mu.Lock()
	defer startupEstimateStore.mu.Unlock()

	loadStartupEstimatesLocked()

	record := startupEstimateRecord{}
	if estimate > 0 {
		record.EstimateSeconds = int64(estimate.Round(time.Second) / time.Second)
	}
	if lastDuration > 0 {
		record.LastDurationSeconds = int64(lastDuration.Round(time.Second) / time.Second)
	}
	if !lastCompletedAt.IsZero() {
		record.LastCompletedAt = lastCompletedAt.UTC().Format(time.RFC3339)
	}
	startupEstimateStore.values[groupName] = record

	raw, err := json.Marshal(startupEstimateStore.values)
	if err != nil {
		return
	}

	if err := os.WriteFile(startupEstimateStore.path, raw, 0o644); err != nil {
		return
	}
}

func loadQuizzesLocked() {
	if quizDataStore.loaded {
		return
	}

	quizDataStore.loaded = true

	path := os.Getenv("GAME_QUOTES_FILE")
	if path == "" {
		path = defaultQuizzesPath
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		debugLogger.Printf("quiz: could not read quizzes file %s: %v\n", path, err)
		return
	}

	var data quizzesData
	if err := json.Unmarshal(raw, &data); err != nil {
		debugLogger.Printf("quiz: could not parse quizzes JSON: %v\n", err)
		return
	}

	quizDataStore.questions = data.Quizzes
}

func getRandomQuiz() *quizQuestion {
	quizDataStore.mu.Lock()
	defer quizDataStore.mu.Unlock()

	loadQuizzesLocked()

	if len(quizDataStore.questions) == 0 {
		return nil
	}

	idx := time.Now().UnixNano() % int64(len(quizDataStore.questions))
	return &quizDataStore.questions[idx]
}

func loadQuizScoresLocked() {
	if quizScoreStore.loaded {
		return
	}

	quizScoreStore.loaded = true
	quizScoreStore.path = os.Getenv("QUIZ_SCORE_FILE")
	if quizScoreStore.path == "" {
		quizScoreStore.path = defaultQuizScorePath
	}

	raw, err := os.ReadFile(quizScoreStore.path)
	if err != nil {
		return
	}

	var scores map[string]quizSessionState
	if err := json.Unmarshal(raw, &scores); err != nil {
		return
	}

	quizScoreStore.values = scores
}

func saveQuizScore(sessionID string, state quizSessionState) {
	quizScoreStore.mu.Lock()
	defer quizScoreStore.mu.Unlock()

	loadQuizScoresLocked()

	quizScoreStore.values[sessionID] = state

	raw, err := json.Marshal(quizScoreStore.values)
	if err != nil {
		return
	}

	if err := os.WriteFile(quizScoreStore.path, raw, 0o644); err != nil {
		return
	}
}

func getQuizScore(sessionID string) quizSessionState {
	quizScoreStore.mu.Lock()
	defer quizScoreStore.mu.Unlock()

	loadQuizScoresLocked()

	state, exists := quizScoreStore.values[sessionID]
	if !exists {
		return quizSessionState{QuestionIndex: 0, Score: 0, TotalAnswered: 0}
	}

	return state
}

func lastAccessPathFromEnv() string {
	path := os.Getenv("LAST_ACCESS_FILE")
	if path == "" {
		return defaultLastAccessPath
	}

	return path
}

func loadLastAccessLocked() {
	if lastAccessStore.loaded {
		return
	}

	lastAccessStore.loaded = true
	lastAccessStore.path = lastAccessPathFromEnv()

	raw, err := os.ReadFile(lastAccessStore.path)
	if err != nil {
		return
	}

	var data map[string]string
	if err := json.Unmarshal(raw, &data); err != nil {
		return
	}

	lastAccessStore.values = data
}

func visitorInfoText(groupName string) string {
	lastAccessStore.mu.Lock()
	defer lastAccessStore.mu.Unlock()

	loadLastAccessLocked()

	now := time.Now()
	lastRaw := lastAccessStore.values[groupName]
	lastAccessStore.values[groupName] = now.UTC().Format(time.RFC3339)

	raw, err := json.Marshal(lastAccessStore.values)
	if err == nil {
		_ = os.WriteFile(lastAccessStore.path, raw, 0o644)
	}

	if lastRaw == "" {
		return "Du bist der erste Besucher seit dem letzten Neustart. Gib mir kurz, ich lade gerade die Map. Bis dahin: nimm ein paar Quiz-Runden mit."
	}

	lastAccess, err := time.Parse(time.RFC3339, lastRaw)
	if err != nil {
		return "Du bist der erste Besucher seit dem letzten Zugriff. Gib mir kurz, ich lade gerade die Map. Bis dahin: nimm ein paar Quiz-Runden mit."
	}

	return fmt.Sprintf(
		"Du bist der erste Besucher seit dem letzten Zugriff am %s um %s. Gib mir kurz, ich lade gerade die Map. Bis dahin: nimm ein paar Quiz-Runden mit.",
		lastAccess.Local().Format("02.01.2006"),
		lastAccess.Local().Format("15:04:05"),
	)
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

.last-startup {
	margin-top: 14px;
	font-size: 0.92rem;
	color: #3f5568;
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

func expectsJSON(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	path := strings.ToLower(r.URL.Path)

	if strings.Contains(accept, "application/json") {
		return true
	}
	if strings.Contains(contentType, "application/json") {
		return true
	}
	if strings.HasPrefix(path, "/api") {
		return true
	}

	return false
}

func (sp *startupStatusPage) lastStartupSummary() string {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.lastStartupDuration <= 0 || sp.lastCompletedAt.IsZero() {
		return "Noch kein gemessener Start vorhanden."
	}

	return fmt.Sprintf(
		"Der letzte Start dauerte %d Sekunden am %s um %s.",
		int(sp.lastStartupDuration.Round(time.Second)/time.Second),
		sp.lastCompletedAt.Local().Format("02.01.2006"),
		sp.lastCompletedAt.Local().Format("15:04:05"),
	)
}

func writeAPIWaitResponse(w http.ResponseWriter, sp *startupStatusPage, remaining int) {
	if remaining < 1 {
		remaining = 1
	}

	lastSummary := sp.lastStartupSummary()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(remaining))
	w.Header().Set("X-Lazytainer-Status", "starting")
	w.Header().Set("X-Lazytainer-Wait-Seconds", strconv.Itoa(remaining))
	w.WriteHeader(http.StatusServiceUnavailable)

	response := map[string]any{
		"status":           "starting",
		"group":            sp.groupName,
		"waitSeconds":      remaining,
		"retryAfter":       remaining,
		"lastStartup":      lastSummary,
		"statusCode":       http.StatusServiceUnavailable,
		"statusCodeReason": "Service Unavailable",
	}

	_ = json.NewEncoder(w).Encode(response)
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

		if expectsJSON(r) {
			writeAPIWaitResponse(w, sp, remaining)
			return
		}

		lastStartupSummary := sp.lastStartupSummary()
		visitorInfo := visitorInfoText(sp.groupName)
		quizSectionStyle := ""
		quizInfoText := "Der Container waermt sich gerade auf. Solange kannst du mit Gaming-Zitaten ein paar Punkte farmen."
		countdownLabel := "Verbleibend bis zum Reload"
		debugInfoText := ""
		if !sp.quizEnabled {
			quizSectionStyle = "display:none;"
			quizInfoText = "Das Minigame ist aktuell deaktiviert. Der Container startet trotzdem ganz normal."
		}
		if sp.debugMode {
			countdownLabel = "Debug-Modus aktiv: kein automatischer Reload"
			debugInfoText = "Debug-Modus aktiv: Die Statusseite bleibt stehen, damit du sie in Ruhe untersuchen kannst."
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", strconv.Itoa(max(1, remaining)))
		w.Header().Set("X-Lazytainer-Status", "starting")
		w.Header().Set("X-Lazytainer-Wait-Seconds", strconv.Itoa(max(1, remaining)))
		w.WriteHeader(http.StatusServiceUnavailable)

		// Session ID via cookie
		sessionID := fmt.Sprintf("%s_%d", sp.groupName, time.Now().Unix())
		http.SetCookie(w, &http.Cookie{
			Name:  "quiz_session",
			Value: sessionID,
			Path:  "/",
		})

		_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="de">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>Container wird gestartet</title>
	<link rel="stylesheet" href="`+statusCSSRoute+`">
	<style>
		.container { display: flex; gap: 20px; flex-wrap: wrap; margin-top: 20px; }
		.section { flex: 1; min-width: 280px; }
		.quiz-card { background: #f9fafb; border-radius: 12px; padding: 16px; margin-top: 12px; }
		.quiz-question { font-weight: 600; color: #102131; margin-bottom: 12px; }
		.quiz-options { display: flex; flex-direction: column; gap: 8px; }
		.quiz-option { display: flex; align-items: center; gap: 8px; }
		.quiz-option input[type="radio"] { cursor: pointer; }
		.quiz-option label { cursor: pointer; flex: 1; }
		.btn-next { margin-top: 12px; padding: 8px 16px; background: #0f8f67; color: white; border: none; border-radius: 6px; cursor: pointer; font-weight: 600; }
		.btn-next:hover { background: #0d7a57; }
		.quiz-feedback { margin-top: 12px; padding: 10px; border-radius: 6px; font-size: 0.9rem; }
		.feedback-correct { background: #d4edda; color: #155724; }
		.feedback-incorrect { background: #f8d7da; color: #721c24; }
		.scoreboard { background: #ffffff; border: 1px solid #e0e7ff; border-radius: 12px; padding: 12px; margin-bottom: 12px; }
		.score-line { display: flex; justify-content: space-between; font-size: 0.9rem; color: #4d6274; }
		.score-value { font-weight: 600; color: #0f8f67; }
		.score-info { margin-top: 8px; font-size: 0.88rem; color: #5b6f81; line-height: 1.4; }
		.quiz-hint { margin-top: 10px; font-size: 0.88rem; color: #8a5b00; }
	</style>
</head>
<body>
	<div class="scoreboard">
		<div class="score-line">
			<span>Punkte:</span>
			<span class="score-value" id="score-display">0/0</span>
		</div>
		<p class="score-info">Scoreboard: Erste Zahl = richtige Treffer, zweite Zahl = gespielte Fragen.</p>
		<p class="score-info">%s</p>
		<p class="score-info">%s</p>
		<p class="score-info">%s</p>
	</div>

	<div class="container">
		<div class="section">
			<div class="card">
				<div class="header">
					<span class="dot" aria-hidden="true"></span>
					<h1>Container bootet gerade</h1>
				</div>
				<p>Container der Gruppe <strong>%s</strong> faehrt gerade hoch und ist gleich am Start.</p>
				<p class="last-startup">%s</p>
				<div class="countdown" aria-live="polite"><span id="remaining">%d</span> s</div>
				<p class="countdown-label">%s</p>
			</div>
		</div>

		<div class="section" id="quiz-section" style="%s">
			<div class="card">
				<h2 style="margin: 0 0 16px 0; font-size: 1.1rem;">🎮 Spiele-Quiz</h2>
				<div id="quiz-container" class="quiz-card"></div>
			</div>
		</div>
	</div>

	<script>
		const MINIGAME_ENABLED = %t;
		const DEBUG_MODE = %t;
		const SESSION_ID = '%s';
		let currentQuestion = null;
		let score = 0;
		let totalAnswered = 0;
		let answered = false;

		async function loadQuiz() {
			try {
				const res = await fetch('/api/quiz?session=' + encodeURIComponent(SESSION_ID), {
					headers: { 'Accept': 'application/json' }
				});
				const data = await res.json();
				currentQuestion = data.question;
				score = data.score;
				totalAnswered = data.totalAnswered;
				renderQuiz();
				updateScoreboard();
			} catch (e) {
				console.error('Quiz load error:', e);
				document.getElementById('quiz-container').innerHTML = '<p style="color: #d32f2f;">Quiz konnte nicht geladen werden.</p>';
			}
		}

		function renderQuiz() {
			if (!currentQuestion) {
				document.getElementById('quiz-container').innerHTML = '<p>Keine Fragen verfügbar.</p>';
				return;
			}

			const container = document.getElementById('quiz-container');
			const optionsHTML = currentQuestion.options.map((opt, i) => 
				'<div class="quiz-option"><input type="radio" id="opt' + i + '" name="answer" value="' + i + '" ' + (answered ? 'disabled' : '') + '><label for="opt' + i + '">' + opt + '</label></div>'
			).join('');

			container.innerHTML = 
				'<div class="quiz-question">Zitat: "' + currentQuestion.quote + '"</div>' +
				'<div class="quiz-options">' + optionsHTML + '</div>' +
				'<button id="submit-btn" class="btn-next" onclick="submitAnswer()" ' + (answered ? 'style="display:none"' : '') + '>Antwort senden</button>' +
				'<button id="next-btn" class="btn-next" onclick="nextQuestion()" style="display:none; margin-left: 8px;">Weiter</button>' +
				'<div id="quiz-hint" class="quiz-hint"></div>' +
				'<div id="feedback"></div>';
		}

		function updateScoreboard() {
			document.getElementById('score-display').textContent = score + '/' + totalAnswered;
		}

		async function submitAnswer() {
			if (answered) return;

			const selected = document.querySelector('input[name="answer"]:checked');
			const hintEl = document.getElementById('quiz-hint');
			if (!selected) {
				if (hintEl) {
					hintEl.textContent = 'Bitte waehle erst eine Antwort aus.';
				}
				return;
			}
			if (hintEl) {
				hintEl.textContent = '';
			}

			answered = true;
			const answerIndex = parseInt(selected.value, 10);
			const submitBtn = document.getElementById('submit-btn');
			if (submitBtn) {
				submitBtn.setAttribute('disabled', 'disabled');
				submitBtn.textContent = 'Pruefe...';
			}

			try {
				let res = await fetch('/api/quiz/answer', {
					method: 'POST',
					headers: { 'Content-Type': 'application/json' },
					body: JSON.stringify({
						session: SESSION_ID,
						answerIndex: answerIndex,
						questionId: currentQuestion.id
					})
				});

				// Fallback for environments that normalize paths differently.
				if (res.status === 404 || res.status === 405) {
					res = await fetch('/api/quiz/answer', {
						method: 'POST',
						headers: { 'Content-Type': 'application/json' },
						body: JSON.stringify({
							session: SESSION_ID,
							answerIndex: answerIndex,
							questionId: currentQuestion.id
						})
					});
				}

				if (!res.ok) {
					throw new Error('Antwort konnte nicht gesendet werden (' + res.status + ')');
				}

				const data = await res.json();
				const feedback = document.getElementById('feedback');
				const nextBtn = document.getElementById('next-btn');
				
				if (data.correct) {
					feedback.className = 'quiz-feedback feedback-correct';
					feedback.textContent = '✓ Richtig! ' + (data.message || '');
					score = data.score;
				} else {
					feedback.className = 'quiz-feedback feedback-incorrect';
					feedback.textContent = '✗ Falsch! Die Antwort ist: ' + currentQuestion.options[currentQuestion.correctIndex];
				}

				totalAnswered = data.totalAnswered;
				updateScoreboard();
				if (nextBtn) {
					nextBtn.style.display = 'inline-block';
				}
			} catch (e) {
				console.error('Submit error:', e);
				const feedback = document.getElementById('feedback');
				if (feedback) {
					feedback.className = 'quiz-feedback feedback-incorrect';
					feedback.textContent = 'Antwort konnte nicht gesendet werden. Bitte erneut versuchen.';
				}
				answered = false;
				if (submitBtn) {
					submitBtn.removeAttribute('disabled');
					submitBtn.textContent = 'Antwort senden';
				}
			}
		}

		function nextQuestion() {
			answered = false;
			loadQuiz();
		}

		// Countdown timer
		let remaining = Math.max(0, Number(%d));
		let reloading = false;

		const remainingEl = document.getElementById("remaining");

		function reloadNow() {
			if (reloading) return;
			reloading = true;
			window.location.reload();
		}

		function render() {
			remainingEl.textContent = String(remaining);
		}

		render();
		if (remaining === 0) {
			if (!DEBUG_MODE) {
				reloadNow();
			}
		} else {
			setInterval(function () {
				if (remaining > 0) {
					remaining -= 1;
					render();
					if (remaining === 0 && !DEBUG_MODE) {
						reloadNow();
					}
				}
			}, 1000);
		}

		// Load quiz on page load only when enabled
		if (MINIGAME_ENABLED) {
			loadQuiz();
		}
	</script>
</body>
</html>`, debugInfoText, quizInfoText, visitorInfo, sp.groupName, lastStartupSummary, remaining, countdownLabel, quizSectionStyle, sp.quizEnabled, sp.debugMode, sessionID, remaining)
	})

	// Quiz API: Get next question
	quizGetHandler := func(w http.ResponseWriter, r *http.Request) {
		if !sp.quizEnabled {
			http.NotFound(w, r)
			return
		}

		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		sessionID := r.URL.Query().Get("session")
		if sessionID == "" {
			http.Error(w, "Missing session", http.StatusBadRequest)
			return
		}

		state := getQuizScore(sessionID)
		question := getRandomQuiz()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		response := map[string]any{
			"question":      question,
			"score":         state.Score,
			"totalAnswered": state.TotalAnswered,
		}
		_ = json.NewEncoder(w).Encode(response)
	}
	mux.HandleFunc("/api/quiz", quizGetHandler)
	mux.HandleFunc("/api/quiz/", quizGetHandler)

	// Quiz API: Submit answer
	quizAnswerHandler := func(w http.ResponseWriter, r *http.Request) {
		if !sp.quizEnabled {
			http.NotFound(w, r)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		type answerRequest struct {
			Session     string `json:"session"`
			AnswerIndex int    `json:"answerIndex"`
			QuestionID  int    `json:"questionId"`
		}

		var req answerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}

		state := getQuizScore(req.Session)
		state.TotalAnswered++

		// Find question by ID
		var question *quizQuestion
		quizDataStore.mu.Lock()
		loadQuizzesLocked()
		for i := range quizDataStore.questions {
			if quizDataStore.questions[i].ID == req.QuestionID {
				question = &quizDataStore.questions[i]
				break
			}
		}
		quizDataStore.mu.Unlock()

		correct := false
		if question != nil && req.AnswerIndex == question.CorrectIndex {
			correct = true
			state.Score++
		}

		saveQuizScore(req.Session, state)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		response := map[string]any{
			"correct":       correct,
			"score":         state.Score,
			"totalAnswered": state.TotalAnswered,
			"message":       "",
		}
		_ = json.NewEncoder(w).Encode(response)
	}
	mux.HandleFunc("/api/quiz/answer", quizAnswerHandler)
	mux.HandleFunc("/api/quiz/answer/", quizAnswerHandler)

	// Prefix fallback for API paths, avoids accidental handling by "/" route.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/quiz/answer"):
			quizAnswerHandler(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/quiz"):
			quizGetHandler(w, r)
		default:
			http.NotFound(w, r)
		}
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
	sp.lastStartupDuration = d
	sp.lastCompletedAt = time.Now()
	sp.wakeRequestedAt = time.Time{}
	saveStartupEstimate(sp.groupName, sp.observedEstimate, sp.lastStartupDuration, sp.lastCompletedAt)
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
