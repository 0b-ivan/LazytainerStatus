package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

type answerRequest struct {
	Session     string `json:"session"`
	AnswerIndex int    `json:"answerIndex"`
	QuestionID  int    `json:"questionId"`
}

func (sp *startupStatusPage) quizGetHandler(w http.ResponseWriter, r *http.Request) {
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

func (sp *startupStatusPage) quizAnswerHandler(w http.ResponseWriter, r *http.Request) {
	if !sp.quizEnabled {
		http.NotFound(w, r)
		return
	}

	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req answerRequest
	if r.Method == http.MethodGet {
		req.Session = r.URL.Query().Get("session")
		req.AnswerIndex, _ = strconv.Atoi(r.URL.Query().Get("answerIndex"))
		req.QuestionID, _ = strconv.Atoi(r.URL.Query().Get("questionId"))
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request", http.StatusBadRequest)
			return
		}
	}

	if req.Session == "" || req.QuestionID <= 0 {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	state := getQuizScore(req.Session)
	state.TotalAnswered++

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

func (sp *startupStatusPage) registerQuizRoutes(mux *http.ServeMux) {
	mux.HandleFunc(statusAPIBaseRoute+"/quiz", sp.quizGetHandler)
	mux.HandleFunc(statusAPIBaseRoute+"/quiz/", sp.quizGetHandler)
	mux.HandleFunc("/api/quiz", sp.quizGetHandler)
	mux.HandleFunc("/api/quiz/", sp.quizGetHandler)

	mux.HandleFunc(statusAPIBaseRoute+"/quiz/answer", sp.quizAnswerHandler)
	mux.HandleFunc(statusAPIBaseRoute+"/quiz/answer/", sp.quizAnswerHandler)
	mux.HandleFunc("/api/quiz/answer", sp.quizAnswerHandler)
	mux.HandleFunc("/api/quiz/answer/", sp.quizAnswerHandler)

	mux.HandleFunc(statusAPIBaseRoute+"/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, statusAPIBaseRoute+"/quiz/answer"):
			sp.quizAnswerHandler(w, r)
		case strings.HasPrefix(r.URL.Path, statusAPIBaseRoute+"/quiz"):
			sp.quizGetHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	})

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/quiz/answer"):
			sp.quizAnswerHandler(w, r)
		case strings.HasPrefix(r.URL.Path, "/api/quiz"):
			sp.quizGetHandler(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}
