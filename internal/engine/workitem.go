package engine

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// WorkItem is a unit of work with people attached to it: who asked for it, who
// is doing it, who checks it, and where it has got to.
//
// Deliberately NOT stored in the hours database, even though `breeze board`
// reads tasks from there. hours' schema is a third-party contract this repo
// refuses to write outside of (see hourslog.checkSchema), and workflow state
// belongs where the identities, roles, notifications and audit log already are.
// hours records where time WENT; this records what is being done and by whom.
type WorkItem struct {
	ID       string
	Title    string
	Creator  string
	Assignee string
	Reviewer string
	Status   WorkStatus
	// Blocked is free text, set alongside StatusBlocked. A blocked item that does
	// not say what it is waiting for is a status nobody can act on — it reads as
	// "someone gave up" rather than as a request.
	Blocked   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// WorkStatus is deliberately a short closed set. An open vocabulary turns into
// six spellings of "in progress" across four agents, and then no query works.
type WorkStatus string

const (
	StatusOpen    WorkStatus = "open"
	StatusDoing   WorkStatus = "doing"
	StatusReview  WorkStatus = "review"
	StatusDone    WorkStatus = "done"
	StatusBlocked WorkStatus = "blocked"
)

// WorkStatuses is the display order, which is also the usual direction of travel.
var WorkStatuses = []WorkStatus{StatusOpen, StatusDoing, StatusReview, StatusDone, StatusBlocked}

func ValidStatus(s WorkStatus) bool { return slices.Contains(WorkStatuses, s) }

// CreateWorkItem records a new item. Creator is the caller.
func (e *Engine) CreateWorkItem(title, creator, assignee, reviewer string) (*WorkItem, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("a work item needs a title — it is what everyone else will see")
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// Named people must exist. A typo'd assignee is worse than an empty one: the
	// item looks owned, nobody is notified, and it sits.
	for label, who := range map[string]string{"assignee": assignee, "reviewer": reviewer} {
		if who == "" {
			continue
		}
		if _, ok := e.identities[who]; !ok {
			return nil, fmt.Errorf("%s %q is not a registered identity — an item that looks assigned to nobody is worse than an unassigned one", label, who)
		}
	}

	e.workSeq++
	item := &WorkItem{
		ID: "w" + strconv.Itoa(e.workSeq), Title: title,
		Creator: creator, Assignee: assignee, Reviewer: reviewer,
		Status:    StatusOpen,
		CreatedAt: e.now(), UpdatedAt: e.now(),
	}
	e.work[item.ID] = item
	e.audit("task.created", creator, fmt.Sprintf("id=%s title=%q assignee=%s reviewer=%s", item.ID, title, orNone(assignee), orNone(reviewer)))
	e.changed()

	cp := *item
	return &cp, nil
}

// WorkUpdate is the set of changes to apply. Nil fields mean "leave alone",
// which is why they are pointers: assigning "" is a real operation (unassign)
// and has to be distinguishable from not mentioning the field.
type WorkUpdate struct {
	Status   *WorkStatus
	Assignee *string
	Reviewer *string
	Blocked  *string
}

// UpdateWorkItem applies changes and returns the item plus who should hear about
// it. Notification is the CALLER's to send: the engine holds a lock here, and
// mess is a subprocess.
func (e *Engine) UpdateWorkItem(id, actor string, up WorkUpdate) (*WorkItem, []string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	item, ok := e.work[id]
	if !ok {
		return nil, nil, fmt.Errorf("no work item %q", id)
	}
	if up.Status != nil && !ValidStatus(*up.Status) {
		return nil, nil, fmt.Errorf("status %q is not one of %s", *up.Status, statusList())
	}
	for label, who := range map[string]*string{"assignee": up.Assignee, "reviewer": up.Reviewer} {
		if who == nil || *who == "" {
			continue
		}
		if _, ok := e.identities[*who]; !ok {
			return nil, nil, fmt.Errorf("%s %q is not a registered identity", label, *who)
		}
	}

	var changes []string
	before := item.Status
	if up.Status != nil && *up.Status != item.Status {
		item.Status = *up.Status
		changes = append(changes, fmt.Sprintf("%s → %s", before, item.Status))
	}
	if up.Assignee != nil && *up.Assignee != item.Assignee {
		changes = append(changes, fmt.Sprintf("assignee %s → %s", orNone(item.Assignee), orNone(*up.Assignee)))
		item.Assignee = *up.Assignee
	}
	if up.Reviewer != nil && *up.Reviewer != item.Reviewer {
		changes = append(changes, fmt.Sprintf("reviewer %s → %s", orNone(item.Reviewer), orNone(*up.Reviewer)))
		item.Reviewer = *up.Reviewer
	}
	if up.Blocked != nil {
		item.Blocked = *up.Blocked
	}
	// Nothing actually moved: no audit line, no notification, no snapshot write.
	// A "changed" message for a change that did not happen teaches people to
	// ignore the channel.
	if len(changes) == 0 {
		cp := *item
		return &cp, nil, nil
	}
	item.UpdatedAt = e.now()
	e.audit("task.updated", actor, fmt.Sprintf("id=%s %s", id, strings.Join(changes, ", ")))
	e.changed()

	cp := *item
	return &cp, e.stakeholdersLocked(item, actor), nil
}

// stakeholdersLocked is who hears about a change: the people ATTACHED to the
// item, minus whoever made it.
//
// Not everyone, and not a broadcast: an agent who was never named on this item
// has no stake in it, and a channel that fires for work nobody asked you about
// is one people mute. Excluding the actor is the same instinct — you do not need
// telling what you just did, and a notification you caused is the fastest way to
// learn the notifications are noise.
//
// NotifyOptOut is honoured by the sender (notifyResolution's path), so an
// identity that opted out of breeze's mess traffic stays opted out here.
func (e *Engine) stakeholdersLocked(item *WorkItem, actor string) []string {
	var out []string
	for _, who := range []string{item.Creator, item.Assignee, item.Reviewer} {
		if who == "" || who == actor || slices.Contains(out, who) {
			continue
		}
		if id, ok := e.identities[who]; ok && !id.NotifyOptOut {
			out = append(out, who)
		}
	}
	return out
}

// WorkItems returns every item, newest first. Sorted rather than map order: a
// list that reorders between two runs reads as though something changed.
func (e *Engine) WorkItems() []WorkItem {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]WorkItem, 0, len(e.work))
	for _, w := range e.work {
		out = append(out, *w)
	}
	slices.SortFunc(out, func(a, b WorkItem) int {
		return strings.Compare(workOrder(b.ID), workOrder(a.ID))
	})
	return out
}

// workOrder pads the numeric part so w10 sorts after w9 rather than before it.
func workOrder(id string) string {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "w"))
	if err != nil {
		return id
	}
	return fmt.Sprintf("%012d", n)
}

func statusList() string {
	out := make([]string, 0, len(WorkStatuses))
	for _, s := range WorkStatuses {
		out = append(out, string(s))
	}
	return strings.Join(out, ", ")
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
