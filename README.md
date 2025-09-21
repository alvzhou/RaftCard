# RaftCard

**RaftCard** — credit-card application distributed service

RaftCard is a fault-tolerant backend for processing credit-card applications with minimal disruption—even during failures.

**Features**

Simple REST API

- POST /applications — submit a new application
- GET /applications/{id} — check current status

3-node Raft cluster
- One leader, two followers
- Write-ahead log replicated to a majority before commit
- Automatic leader election on failure

DecisionWorker
- After a log entry is committed, a decision is computed (currently based on basic signals like credit score and income) and recorded via another replicated command

**How it works (Raft)**

Client sends a command to the API; it’s forwarded to the Raft leader.

The leader replicates the log entry to followers.

On majority ack, the entry is committed and applied to the state machine.

The DecisionWorker issues a RecordDecision command, which is replicated and committed the same way.

GET /applications/{id} returns the committed status—no flip-flops.

**Key Strengths**
Outage-tolerant writes
With 3 nodes, the system keeps accepting applications if any 1 node is down.

Strong linearizability
Reads reflect the latest committed state from a single, ordered log; statuses don’t regress from APPROVED back to PENDING.

Built-in audit trail
The append-only log captures every change and decision for replay and compliance.

Scalable
Start with one Raft group; add shards (more groups) as volume grows.

**Trade-offs**

Complexity: Running a consensus protocol is more involved than a single DB/queue.

Latency: Each write waits for a majority of nodes, adding a small, predictable overhead.

