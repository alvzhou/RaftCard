// handler.go
// HTTP routes and handlers. Uses the server struct from server.go and the RaftNode from raft.go
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	StatusPending  = "PENDING"
	StatusApproved = "APPROVED"
	StatusDeclined = "DECLINED"
	StatusReferred = "REFERRED"
)

type CreateApplicationRequest struct {
	IdempotencyKey string          `json:"idempotency_key"`
	Applicant      json.RawMessage `json:"applicant"` // keep raw for stable hashing
}

type CreateApplicationResponse struct {
	ApplicationID string `json:"application_id"`
	Status        string `json:"status"`
}

type StatusResponse struct {
	ApplicationID string     `json:"application_id"`
	Status        string     `json:"status"`
	Reasons       []string   `json:"reasons,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
}


type idemRecord struct {
	Hash       [32]byte
	StatusCode int
	Response   []byte // JSON payload previously returned
	CreatedAt  time.Time
}

type idemStore struct{ m sync.Map }

func (s *idemStore) Load(key string) (idemRecord, bool) {
	if v, ok := s.m.Load(key); ok {
		return v.(idemRecord), true
	}
	return idemRecord{}, false
}

func (s *idemStore) Save(key string, rec idemRecord) { s.m.Store(key, rec) }

type Application struct {
	ID        string
	Status    string
	Reasons   []string
	CreatedAt time.Time
	DecidedAt *time.Time
}

type readModel struct {
	mu   sync.RWMutex
	apps map[string]*Application
}

func newReadModel() *readModel { return &readModel{apps: make(map[string]*Application)} }

func (rm *readModel) apply(cmd Command) {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	switch cmd.Type {
	case CmdCreateApplication:
		if _, exists := rm.apps[cmd.AppID]; !exists {
			rm.apps[cmd.AppID] = &Application{
				ID:        cmd.AppID,
				Status:    StatusPending,
				CreatedAt: time.Now().UTC(),
			}
		}
	case CmdRecordDecision:
		if app, ok := rm.apps[cmd.AppID]; ok {
			if app.Status == StatusPending { // one terminal decision only
				app.Status = cmd.Decision
				app.Reasons = append([]string(nil), cmd.Reasons...)
				now := time.Now().UTC()
				app.DecidedAt = &now
			}
		}
	}
}

func (rm *readModel) get(appID string) (*Application, bool) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	app, ok := rm.apps[appID]
	return app, ok
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/applications", s.handleCreateApplication)
	mux.HandleFunc("/applications/", s.handleGetStatus) // /applications/{id}
	return s.withCommon(mux)
}

func (s *server) withCommon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		next.ServeHTTP(w, r)
	})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("{\"ok\":true}"))
}

// POST /applications
func (s *server) handleCreateApplication(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	// If this node isn't leader, instruct client to use leader.
	if !s.raft.IsLeader() {
		leader := s.raft.LeaderHTTPAddr()
		w.Header().Set("Leader-Location", leader)
		http.Error(w, http.StatusText(http.StatusTemporaryRedirect), http.StatusTemporaryRedirect)
		return
	}

	var req CreateApplicationRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid json: %v", err), http.StatusBadRequest)
		return
	}
	if req.IdempotencyKey == "" || len(req.Applicant) == 0 {
		http.Error(w, "idempotency_key and applicant are required", http.StatusBadRequest)
		return
	}

	// Idempotency check
	payloadHash := sha256.Sum256(req.Applicant)
	if rec, ok := s.idem.Load(req.IdempotencyKey); ok {
		if rec.Hash != payloadHash {
			http.Error(w, "idempotency conflict: same key, different payload", http.StatusConflict)
			return
		}
		w.WriteHeader(rec.StatusCode)
		w.Write(rec.Response)
		return
	}

	appID := randomID("app_", 8)
	cmdID := randomID("cmd_", 8)
	cmd := Command{
		Type:        CmdCreateApplication,
		AppID:       appID,
		PayloadHash: hex.EncodeToString(payloadHash[:]),
		CommandID:   cmdID,
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.raft.AppendCommand(ctx, cmd); err != nil {
		var nle NotLeaderError
		if errors.As(err, &nle) {
			w.Header().Set("Leader-Location", nle.LeaderAddr)
			http.Error(w, http.StatusText(http.StatusTemporaryRedirect), http.StatusTemporaryRedirect)
			return
		}
		http.Error(w, fmt.Sprintf("append failed: %v", err), http.StatusServiceUnavailable)
		return
	}

	resp := CreateApplicationResponse{ApplicationID: appID, Status: StatusPending}
	b, _ := json.Marshal(resp)

	s.idem.Save(req.IdempotencyKey, idemRecord{Hash: payloadHash, StatusCode: http.StatusAccepted, Response: b, CreatedAt: time.Now()})

	w.WriteHeader(http.StatusAccepted)
	w.Write(b)
}

func (s *server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/applications/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if err := s.raft.ReadIndex(r.Context()); err != nil {
		var nle NotLeaderError
		if errors.As(err, &nle) {
			w.Header().Set("Leader-Location", nle.LeaderAddr)
			http.Error(w, http.StatusText(http.StatusTemporaryRedirect), http.StatusTemporaryRedirect)
			return
		}
		http.Error(w, fmt.Sprintf("read barrier failed: %v", err), http.StatusServiceUnavailable)
		return
	}

	if app, ok := s.read.get(id); ok {
		resp := StatusResponse{
			ApplicationID: app.ID,
			Status:        app.Status,
			Reasons:       app.Reasons,
			CreatedAt:     app.CreatedAt,
			DecidedAt:     app.DecidedAt,
		}
		json.NewEncoder(w).Encode(resp)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}
