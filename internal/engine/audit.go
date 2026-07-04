package engine

import "time"

// AuditEvent is one line of the append-only audit log — historical events that don't
// belong in the current-state snapshot (hook failures, stage transitions). Unlike
// onChange/snapshot persistence (which fires asynchronously in a goroutine — see
// Engine.changed), audit records are emitted synchronously, still holding e.mu, so
// their Seq ordering in the log is always gapless and exactly matches mutation order.
type AuditEvent struct {
	Seq    int
	Time   time.Time
	Kind   string
	Actor  string
	Detail string
}

func (e *Engine) SetAuditFn(fn func(AuditEvent)) {
	e.mu.Lock()
	e.auditFn = fn
	e.mu.Unlock()
}

// audit must be called with e.mu held.
func (e *Engine) audit(kind, actor, detail string) {
	if e.auditFn == nil {
		return
	}
	e.auditSeq++
	e.auditFn(AuditEvent{Seq: e.auditSeq, Time: e.now(), Kind: kind, Actor: actor, Detail: detail})
}
