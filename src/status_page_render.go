package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type statusPageViewData struct {
	DebugInfoText    string
	QuizInfoText     string
	VisitorInfo      string
	LastStartup      string
	InlineCSS        string
	SessionID        string
	CountdownLabel   string
	ScoreboardStyle  string
	QuizSectionStyle string
	Remaining        int
	ElapsedSeconds   int
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
	background: radial-gradient(circle at 10%% 20%%, #2f5f84 0%%, transparent 45%%),
							radial-gradient(circle at 85%% 85%%, #0d8f9a 0%%, transparent 45%%),
							linear-gradient(160deg, var(--bg-top), var(--bg-bottom));
	font-family: "Segoe UI", "Noto Sans", sans-serif;
	padding: 24px;
}

.card {
	width: min(640px, 100%%);
	background: linear-gradient(180deg, #ffffff 0%%, var(--card) 100%%);
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

func (sp *startupStatusPage) buildStatusPageViewData(remaining int) statusPageViewData {
	elapsedSecs := sp.elapsedSeconds()
	quizVisibleOnLoad := sp.quizEnabled && elapsedSecs >= 40

	view := statusPageViewData{
		DebugInfoText:    "",
		QuizInfoText:     "Der Container waermt sich gerade auf. Solange kannst du mit Gaming-Zitaten ein paar Punkte farmen.",
		VisitorInfo:      visitorInfoText(sp.groupName),
		LastStartup:      sp.lastStartupSummary(),
		InlineCSS:        sp.getStatusCSS(),
		SessionID:        fmt.Sprintf("%s_%d", sp.groupName, time.Now().Unix()),
		CountdownLabel:   "Verbleibend bis zum Reload",
		ScoreboardStyle:  "",
		QuizSectionStyle: "",
		Remaining:        remaining,
		ElapsedSeconds:   elapsedSecs,
	}

	if !sp.quizEnabled {
		view.ScoreboardStyle = "display:none;"
		view.QuizSectionStyle = "display:none;"
		view.QuizInfoText = "Das Minigame ist aktuell deaktiviert. Der Container startet trotzdem ganz normal."
	} else if !quizVisibleOnLoad {
		view.ScoreboardStyle = "display:none;"
		view.QuizSectionStyle = "display:none;"
		view.QuizInfoText = "Minigame wird nach 40 Sekunden Wartezeit automatisch freigeschaltet."
	}

	if sp.debugMode {
		view.CountdownLabel = "Debug-Modus aktiv: kein automatischer Reload"
		view.DebugInfoText = "Debug-Modus aktiv: Die Statusseite bleibt stehen, damit du sie in Ruhe untersuchen kannst."
	}

	return view
}

func (sp *startupStatusPage) renderStatusPage(w http.ResponseWriter, remaining int) {
	view := sp.buildStatusPageViewData(remaining)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(max(1, remaining)))
	w.Header().Set("X-Lazytainer-Status", "starting")
	w.Header().Set("X-Lazytainer-Wait-Seconds", strconv.Itoa(max(1, remaining)))
	w.WriteHeader(http.StatusServiceUnavailable)

	http.SetCookie(w, &http.Cookie{
		Name:  "quiz_session",
		Value: view.SessionID,
		Path:  "/",
	})

	_, _ = fmt.Fprintf(w, statusPageTemplate,
		view.InlineCSS,
		view.ScoreboardStyle,
		view.DebugInfoText,
		view.QuizInfoText,
		view.VisitorInfo,
		sp.groupName,
		view.LastStartup,
		view.Remaining,
		view.CountdownLabel,
		view.QuizSectionStyle,
		sp.quizEnabled,
		sp.debugMode,
		view.ElapsedSeconds,
		statusAPIBaseRoute,
		view.SessionID,
		view.Remaining,
	)
}

const statusPageTemplate = `<!doctype html>
<html lang="de">
<head>
	<meta charset="utf-8">
	<meta name="viewport" content="width=device-width, initial-scale=1">
	<title>Container wird gestartet</title>
	<style>%s</style>
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
	<div class="scoreboard" id="scoreboard" style="%s">
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
		const ELAPSED_SECONDS = %d;
		const QUIZ_START_THRESHOLD = 40;
		const QUIZ_API_BASE = '%s';
		const SESSION_ID = '%s';
		let currentQuestion = null;
		let score = 0;
		let totalAnswered = 0;
		let answered = false;

		async function loadQuiz() {
			try {
				const res = await fetch(QUIZ_API_BASE + '/quiz?session=' + encodeURIComponent(SESSION_ID), {
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
				let res = await fetch(
					QUIZ_API_BASE + '/quiz/answer?session=' + encodeURIComponent(SESSION_ID)
						+ '&answerIndex=' + encodeURIComponent(String(answerIndex))
						+ '&questionId=' + encodeURIComponent(String(currentQuestion.id)),
					{
						headers: { 'Accept': 'application/json' }
					}
				);

				if (res.status === 404 || res.status === 405 || res.status === 501) {
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

		function showQuizSection() {
			const quizSection = document.getElementById('quiz-section');
			if (quizSection) {
				quizSection.style.display = '';
			}
		}

		function showScoreboard() {
			const scoreboard = document.getElementById('scoreboard');
			if (scoreboard) {
				scoreboard.style.display = '';
			}
		}

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

		if (MINIGAME_ENABLED) {
			if (ELAPSED_SECONDS >= QUIZ_START_THRESHOLD) {
				showScoreboard();
				showQuizSection();
				loadQuiz();
			} else {
				const msUntilThreshold = (QUIZ_START_THRESHOLD - ELAPSED_SECONDS) * 1000;
				setTimeout(function() {
					showScoreboard();
					showQuizSection();
					loadQuiz();
				}, msUntilThreshold);
			}
		}
	</script>
</body>
</html>`
