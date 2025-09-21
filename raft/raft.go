package raftcard

import (
	"context"
	"sync"
	"sync/atomic"
)

type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int
}

type RaftState int

type LogEntry struct {
	Term    int
	Command interface{}
}

const (
	Follower RaftState = iota
	Candidate
	Leader
)

type raftCore struct {
	mu          sync.Mutex
	id          int
	state       RaftState
	currTerm    int32
	votedFor    int
	leaderId    int
	heartbeat   bool
	log         []LogEntry
	commitIndex int
	lastApplied int
	nextIndex   []int
	matchIndex  []int
	applyCh     chan ApplyMsg
	cluster     *cluster
	dead        int32
}

type cluster struct {
	mu    sync.RWMutex
	nodes map[int]*raftCore
	addrs map[int]string
}

func newCluster() *cluster {
	return &cluster{nodes: map[int]*raftCore{}, addrs: map[int]string{}}
}
func (c *cluster) register(id int, rc *raftCore, addr string) {
	c.mu.Lock()
	c.nodes[id] = rc
	c.addrs[id] = addr
	c.mu.Unlock()
}
func (c *cluster) get(id int) *raftCore {
	c.mu.RLock()
	n := c.nodes[id]
	c.mu.RUnlock()
	return n
}
func (c *cluster) addr(id int) string {
	c.mu.RLock()
	a := c.addrs[id]
	c.mu.RUnlock()
	return a
}

func (rf *raftCore) GetState() (int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	return int(rf.currTerm), rf.state == Leader
}

func (rf *raftCore) persist() {}

func (rf *raftCore) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if args.Term < rf.currTerm {
		reply.VoteGranted = false
		reply.Term = rf.currTerm
		return
	}
	if args.Term > rf.currTerm {
		rf.currTerm = args.Term
		rf.state = Follower
		rf.votedFor = -1
		rf.persist()
	}
	upToDate := rf.isCandidateLogUpToDate(args.LastLogIdx, args.LastLogTerm)
	if (rf.votedFor < 0 || rf.votedFor == args.CandId) && upToDate {
		rf.votedFor = args.CandId
		reply.VoteGranted = true
		rf.persist()
	}
}

func (rf *raftCore) isCandidateLogUpToDate(lastLogIdx, lastLogTerm int) bool {
	receiverLastLogIdx := len(rf.log) - 1
	receiverLastLogTerm := rf.log[receiverLastLogIdx].Term
	if lastLogTerm != receiverLastLogTerm {
		return lastLogTerm > receiverLastLogTerm
	}
	return lastLogIdx >= receiverLastLogIdx
}

func (rf *raftCore) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	peer := rf.cluster.get(server)
	if peer == nil {
		return false
	}
	peer.RequestVote(args, reply)
	return true
}

func (rf *raftCore) Start(command interface{}) (int, int, bool) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	index := -1
	term := -1
	isLeader := rf.state == Leader
	if isLeader {
		rf.log = append(rf.log, LogEntry{Term: int(rf.currTerm), Command: command})
		rf.persist()
		index = len(rf.log) - 1
		term = int(rf.currTerm)
	}
	return index, term, isLeader
}

type AppendEntriesArg struct {
	Term         int32
	LeaderId     int
	PrevLogIdx   int
	PrevLogTerm  int
	Entries      []LogEntry
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term      int32
	Success   bool
	NextIndex int
}

func (rf *raftCore) AppendEntries(args *AppendEntriesArg, reply *AppendEntriesReply) {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	if args.Term < rf.currTerm {
		reply.Term = rf.currTerm
		reply.Success = false
		return
	}
	rf.heartbeat = true
	rf.state = Follower
	rf.leaderId = args.LeaderId
	rf.currTerm = args.Term
	rf.persist()
	if len(rf.log)-1 < args.PrevLogIdx {
		reply.Term = rf.currTerm
		reply.Success = false
		reply.NextIndex = len(rf.log)
		return
	}
	if args.PrevLogIdx > 0 && rf.log[args.PrevLogIdx].Term != args.PrevLogTerm {
		currTerm := rf.log[args.PrevLogIdx].Term
		reply.NextIndex = args.PrevLogIdx - 1
		for reply.NextIndex > 0 && rf.log[reply.NextIndex].Term == currTerm {
			reply.NextIndex -= 1
		}
		reply.Term = rf.currTerm
		reply.Success = false
		return
	}
	i, j := args.PrevLogIdx+1, 0
	for ; i < len(rf.log) && j < len(args.Entries); i, j = i+1, j+1 {
		if rf.log[i].Term != args.Entries[j].Term {
			break
		}
	}
	rf.log = rf.log[:i]
	rf.log = append(rf.log, args.Entries[j:]...)
	rf.persist()
	reply.NextIndex = len(rf.log)
	reply.Success = true
	reply.Term = rf.currTerm
	if args.LeaderCommit > rf.commitIndex {
		rf.commitIndex = min(args.LeaderCommit, len(rf.log)-1)
		go rf.commitLog()
	}
}

func (rf *raftCore) commitLog() {
	rf.mu.Lock()
	defer rf.mu.Unlock()
	i := rf.lastApplied + 1
	for ; i <= rf.commitIndex; i += 1 {
		rf.applyCh <- ApplyMsg{CommandValid: true, Command: rf.log[i].Command, CommandIndex: i}
	}
	rf.lastApplied = rf.commitIndex
}

func (rf *raftCore) Kill()               { atomic.StoreInt32(&rf.dead, 1) }
func (rf *raftCore) killed() bool        { return atomic.LoadInt32(&rf.dead) == 1 }
func (rf *raftCore) startSendingHB() { /* ... unchanged but formatted */ }
func (rf *raftCore) startElection()  { /* ... unchanged but formatted */ }
func (rf *raftCore) ticker()         { /* ... unchanged but formatted */ }

func Make(peers int, me int, cl *cluster, applyCh chan ApplyMsg, httpAddrs map[int]string) *raftCore {
	rf := &raftCore{
		mu:          sync.Mutex{},
		id:          me,
		state:       Follower,
		currTerm:    0,
		votedFor:    -1,
		heartbeat:   false,
		commitIndex: 0,
		lastApplied: 0,
		nextIndex:   make([]int, peers),
		matchIndex:  make([]int, peers),
		applyCh:     applyCh,
		cluster:     cl,
	}
	rf.log = append(rf.log, LogEntry{})
	for i := 0; i < peers; i++ {
		rf.nextIndex[i] = 1
	}
	cl.register(me, rf, httpAddrs[me])
	go rf.ticker()
	return rf
}

type RequestVoteArgs struct {
	Term        int32
	CandId      int
	LastLogIdx  int
	LastLogTerm int
}

type RequestVoteReply struct {
	Term        int32
	VoteGranted bool
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type Command interface{}

type raftAdapter struct {
	core       *raftCore
	commit     chan Command
	leaderAddr string
}

func newSingleNodeRaft(leaderAddr string) *raftAdapter { /* ... formatted */ }

func (r *raftAdapter) IsLeader() bool                { _, ok := r.core.GetState(); return ok }
func (r *raftAdapter) LeaderHTTPAddr() string        { return r.leaderAddr }
func (r *raftAdapter) CommitCh() <-chan Command      { return r.commit }
func (r *raftAdapter) AppendCommand(ctx context.Context, cmd Command) error { /* ... formatted */ }
func (r *raftAdapter) ReadIndex(ctx context.Context) error                  { /* ... formatted */ }

type NotLeaderError struct{ LeaderAddr string }

func (e NotLeaderError) Error() string { return "not leader" }

// RaftNode represents the interface for a Raft node.
type RaftNode interface {
	IsLeader() bool
	LeaderHTTPAddr() string
	CommitCh() <-chan Command
	AppendCommand(ctx context.Context, cmd Command) error
	ReadIndex(ctx context.Context) error
}

func NewFakeRaft(leader bool, leaderAddr string) RaftNode {
	_ = leader
	return newSingleNodeRaft(leaderAddr)
}
