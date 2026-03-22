package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

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
