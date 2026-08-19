package engine

// ActorOption records WHO performed a privileged operation, for the audit log.
//
// Variadic rather than a signature change because RegisterIdentity and AssignRole
// have roughly a hundred and seventy call sites between them, nearly all tests,
// and burying a provenance change in that much churn is how a provenance change
// stops being reviewable. Same reasoning as StageOption.
//
// WHY THIS EXISTS AT ALL: these operations did not know who was performing them.
// AssignRole took (identity, role) and nothing else, so the engine could not have
// recorded a grantor even if something had asked it to — and on 2026-08-19 nobody
// could establish who had granted an agent the admin role on a shared daemon, or
// when. Not expensive to determine: unrecoverable, from anything on the machine.
// The identity record holds a bare []Role with no provenance at any layer, which
// is the right design — provenance belongs in the log, where Actor and Time come
// for free, rather than denormalized onto the object it describes where it has to
// be maintained twice and drifts.
type ActorOption func(*actorOpts)

type actorOpts struct {
	actor string
}

// By names the identity performing the operation.
func By(actor string) ActorOption {
	return func(o *actorOpts) { o.actor = actor }
}

// actorOf resolves the caller, or "unattributed" when nothing said.
//
// Deliberately NOT "" or "system": an empty actor in a log reads as a field
// nobody filled in, and "system" asserts breeze did it, which would be a claim
// rather than an absence. "unattributed" says exactly what is known — that the
// operation happened and its author was not recorded — which is the honest state
// for a caller that predates this, or a test.
func actorOf(opts []ActorOption) string {
	var o actorOpts
	for _, fn := range opts {
		fn(&o)
	}
	if o.actor == "" {
		return "unattributed"
	}
	return o.actor
}
