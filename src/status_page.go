package main

import (
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
	statusAPIBaseRoute    = "/_lazytainer/api"
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

// Returns elapsed seconds since wake was requested.
func (sp *startupStatusPage) elapsedSeconds() int {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if sp.wakeRequestedAt.IsZero() {
		return 0
	}

	elapsed := time.Since(sp.wakeRequestedAt)
	elapsedSec := int(elapsed.Seconds())
	if elapsedSec < 0 {
		elapsedSec = 0
	}
	return elapsedSec
}
