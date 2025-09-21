package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

type server struct {
	raft     RaftNode
	idem     *idemStore
	read     *readModel
	applied  chan Command
	logger   *log.Logger
}

func newServer(raft RaftNode) *server {
	return &server{
		raft:    raft,
		idem:    &idemStore{},
		read:    newReadModel(),
		applied: make(chan Command, 1024),
		logger:  log.New(os.Stdout, "raftcard ", log.LstdFlags|log.Lmicroseconds),
	}
}

func (s *server) runApplyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-s.raft.CommitCh():
			s.read.apply(cmd)
			// fan-out (non-blocking) for workers
			select { case s.applied <- cmd: default: }
		}
	}
}

func (s *server) runDecisionWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case cmd := <-s.applied:
			if cmd.Type == CmdCreateApplication {
				d := decide(cmd.AppID)
				out := Command{Type: CmdRecordDecision, AppID: cmd.AppID, Decision: d.Decision, Reasons: d.Reasons, CommandID: randomID("cmd_", 8)}
				ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = s.raft.AppendCommand(ctx2, out) 
				cancel()
			}
		}
	}
}

type decision struct {
	Decision string
	Reasons  []string
}

func decide(appID string) decision {
	// toy model: hash appID → score 0..255
	h := sha256.Sum256([]byte(appID))
	score := int(h[0])
	switch {
	case score >= 180:
		return decision{Decision: StatusApproved, Reasons: []string{"score>=180"}}
	case score >= 120:
		return decision{Decision: StatusReferred, Reasons: []string{"score>=120"}}
	default:
		return decision{Decision: StatusDeclined, Reasons: []string{"score<120"}}
	}
}

func randomID(prefix string, nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil { panic(err) }
	return prefix + hex.EncodeToString(b)
}

func main() {
	addr := ":8080"
	if v := os.Getenv("ADDR"); v != "" { addr = v }
	leader := true
	if v := os.Getenv("LEADER"); v == "0" || strings.EqualFold(v, "false") { leader = false }
	leaderAddr := os.Getenv("LEADER_ADDR")
	if leaderAddr == "" { leaderAddr = "http://localhost:8080" }

	raft := Raft(leader, leaderAddr)
	srv := newServer(raft)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.runApplyLoop(ctx)
	go srv.runDecisionWorker(ctx)

	h := &http.Server{ Addr: addr, Handler: srv.routes() }
	srv.logger.Printf("listening on %s (leader=%v)\n", addr, raft.IsLeader())
	if err := h.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err.Error())
	}
}