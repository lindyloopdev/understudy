package understudy

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// admissionGracePeriod is how long a request may sit at a target with no
// response header before an older session's arrival may displace it: long
// enough that an uncontended backend's normal header latency never trips it,
// short enough that a queue forms while the contention still matters.
const admissionGracePeriod = 2 * time.Second

// errPreempted is the cancellation cause for an upstream attempt displaced by
// session-ordered admission — understudy's own signal, not a target failure.
// The displaced attempt is re-sent when the queue admits it; nothing about
// it is recorded.
var errPreempted = errors.New("displaced for an older session")

// sessionForgetAfter is how long a conversation may go with no request before
// its age is forgotten and it ranks as new again, so a session that keeps
// taking turns — however old — is never outranked by one that has been away
// longer. A backstop for consumers that send no session-end signal.
const sessionForgetAfter = 24 * time.Hour

// sessionRecord is a conversation's standing in admission: firstSeen is its
// rank, and lastSeen is when it last took a turn, which [sessionForgetAfter]
// is measured from.
type sessionRecord struct {
	firstSeen time.Time
	lastSeen  time.Time
}

// sessionAge returns when tenant's conversation was first seen, recording the
// first sighting and refreshing the last. The age is the session's admission
// rank, so an old session's later turn outranks a younger session's earlier
// one, and the rank holds through quiet stretches shorter than
// [sessionForgetAfter]. A request with no conversation key ranks by its own
// arrival.
//
// TODO(TODO.d/session-ordered-admission.md): the consumer's session-end signal
// is unbuilt, and a stale entry is reset rather than dropped — so this record,
// keyed on caller-supplied content, still holds one entry per conversation
// ever seen.
func (s *server) sessionAge(tenant, key string) time.Time {
	now := time.Now()
	if key == "" {
		return now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ages, ok := s.sessionAges[tenant]
	if !ok {
		ages = make(map[string]sessionRecord)
		s.sessionAges[tenant] = ages
	}
	if rec, ok := ages[key]; ok && now.Sub(rec.lastSeen) < sessionForgetAfter {
		ages[key] = sessionRecord{firstSeen: rec.firstSeen, lastSeen: now}
		return rec.firstSeen
	}
	ages[key] = sessionRecord{firstSeen: now, lastSeen: now}
	return now
}

// admissionQueue returns the admission queue for a target's availability key
// (canonical url + key + model), the scope providers limit and slots are
// handed out on.
func (s *server) admissionQueue(key string) *targetQueue {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.admission[key]
	if !ok {
		q = &targetQueue{}
		s.admission[key] = q
	}
	return q
}

// targetQueue orders admission to one target by session age: the backend
// cannot see conversations, so the layer holding every request orders them —
// oldest session first, so sessions finish rather than interleave turn by
// turn. It exists only under contention: with nothing waiting, a request
// passes straight through.
type targetQueue struct {
	mu sync.Mutex
	// outstanding holds every attempt at the target — sent, or admitted and
	// on its way — until it ends or its first content confirms it.
	outstanding []*pending
	// waiting holds queued attempts in rank order, oldest session first.
	waiting []*pending
}

// pending is one attempt's standing at a target queue: its rank, when it
// reached the target, and how far the target's answer has come.
type pending struct {
	queue     *targetQueue
	rank      time.Time
	arrived   time.Time
	cancel    context.CancelCauseFunc
	admitted  chan struct{}
	headered  bool
	confirmed bool
	preempted bool
}

// pastGrace reports whether p's grace period has elapsed as of now.
func (p *pending) pastGrace(now time.Time) bool {
	return !p.arrived.Add(admissionGracePeriod).After(now)
}

// enter admits an attempt to q's target, blocking while the queue holds it.
// rank is the attempt's session age; cancel ends its upstream attempt when
// preemption displaces it. A request the queue admits returns unblocked and
// counts as outstanding from that moment, so it holds the queue's next
// admission until its first content confirms it.
func (q *targetQueue) enter(ctx context.Context, cancel context.CancelCauseFunc, rank time.Time) (*pending, error) {
	p := &pending{queue: q, rank: rank, cancel: cancel, admitted: make(chan struct{})}
	q.mu.Lock()
	now := time.Now()
	p.arrived = now
	// An arrival outranking the target's headerless requests forms the queue:
	// each of them past its grace period and ranked below the arrival is
	// canceled — canceling costs nothing, since no header means no work was
	// given — and takes its place in line by rank. A headered request is never
	// displaced: a header means the backend has admitted it, and the wire
	// cannot tell queueing from prefill.
	for _, o := range q.outstanding {
		if o.headered || o.preempted || !o.pastGrace(now) || !o.rank.After(rank) {
			continue
		}
		o.preempted = true
		q.insert(o)
		o.cancel(errPreempted)
	}
	// Growth is speculative: an arrival that displaces nothing still waits
	// behind any request unconfirmed past its grace period, headered or not.
	held := len(q.waiting) > 0
	for _, o := range q.outstanding {
		if !o.confirmed && o.pastGrace(now) {
			held = true
			break
		}
	}
	if held {
		q.insert(p)
		q.mu.Unlock()
		select {
		case <-p.admitted:
			return p, nil
		case <-ctx.Done():
			q.mu.Lock()
			defer q.mu.Unlock()
			if !slices.Contains(q.waiting, p) {
				// The queue admitted p as ctx was failing: the attempt goes on
				// and surfaces its own cause.
				return p, nil
			}
			q.dropWaiting(p)
			return nil, context.Cause(ctx)
		}
	}
	q.outstanding = append(q.outstanding, p)
	// The wait itself can form the queue: p headerless past its grace period
	// is itself the evidence of contention, so every admission arms the timer.
	// TODO(TODO.d/what-a-queue-formation-costs.md): one timer is armed per
	// admission and each sweeps everything outstanding, so nothing here bounds
	// how often a turn admitted and re-sent can be taken again by a later
	// admission's timer. Whether the admission rule already prevents it is
	// unestablished.
	time.AfterFunc(admissionGracePeriod, func() { q.formByWaiting(p) })
	q.mu.Unlock()
	return p, nil
}

// formByWaiting forms the queue: p has waited past its grace period headerless
// with no higher-ranked arrival, so the wait itself is the evidence of
// contention. Every outstanding headerless request past its grace period is
// canceled and queued by rank — canceling costs nothing, since no header
// means no work was given — except the highest-ranked unconfirmed one, which
// stays outstanding as the probe: the queue admits its next head only when
// nothing unconfirmed is at the backend. If that unconfirmed request is
// headered, the probe is queued too: preemption never takes a headered
// request, so it already holds the slot the probe would wait for.
func (q *targetQueue) formByWaiting(p *pending) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.headered || p.preempted || !slices.Contains(q.outstanding, p) {
		return
	}
	now := time.Now()
	var probe *pending
	probeHeld := false
	for _, o := range q.outstanding {
		if o.confirmed || o.preempted {
			continue
		}
		if probe == nil || o.rank.Before(probe.rank) {
			probe = o
		}
		if o.headered {
			probeHeld = true
		}
	}
	for _, o := range q.outstanding {
		if o.headered || o.preempted || !o.pastGrace(now) {
			continue
		}
		// TODO(TODO.d/session-ordered-admission.md): no case yet stages a
		// headered unconfirmed request ranked below a headerless one when the
		// wait forms the queue, so the probe-held arm is unexercised.
		if o == probe && !probeHeld {
			continue
		}
		o.preempted = true
		q.insert(o)
		o.cancel(errPreempted)
	}
}

// attemptEnded records the attempt's upstream call ending: err is nil when the
// call returned a response, whose header has now arrived. It reports whether
// the attempt was preempted — the one ending that is not a failure — so the
// caller waits for the queue to admit it again instead of recording or relaying
// anything. A response that slipped past a preemption decision is discarded:
// the re-sent attempt is the one the client receives.
func (q *targetQueue) attemptEnded(p *pending, err error) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err == nil && !p.preempted {
		p.headered = true
		return false
	}
	q.dropOutstanding(p)
	q.admitIfAllOutstandingConfirmed()
	return p.preempted
}

// await blocks until the queue admits a displaced attempt, honoring ctx: a
// client that leaves while held is not kept waiting.
func (q *targetQueue) await(ctx context.Context, p *pending) error {
	select {
	case <-p.admitted:
		return nil
	case <-ctx.Done():
		q.mu.Lock()
		defer q.mu.Unlock()
		if !slices.Contains(q.waiting, p) {
			// Admitted as ctx was failing; the re-send proceeds and fails with
			// its own cause.
			return nil
		}
		q.dropWaiting(p)
		return context.Cause(ctx)
	}
}

// rearm re-arms p for the re-sent attempt: a fresh upstream context to cancel
// and a fresh admitted channel, so the re-send can itself be displaced.
// Admission stamped a fresh arrival, so the grace period restarts here.
func (q *targetQueue) rearm(p *pending, cancel context.CancelCauseFunc) {
	q.mu.Lock()
	defer q.mu.Unlock()
	p.preempted = false
	p.cancel = cancel
	p.admitted = make(chan struct{})
}

// confirm records p's first content — the moment the target has begun
// answering — which frees the queue to admit its next waiter.
func (q *targetQueue) confirm(p *pending) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.confirmed {
		return
	}
	p.confirmed = true
	q.admitIfAllOutstandingConfirmed()
}

// finish ends p's standing at the target: its attempt is over, so it holds the
// queue's next admission no longer. A confirmed p refills its slot at once,
// with no grace check; any other ending admits only through growth's
// grace-gated check. It is not idempotent: finishing a confirmed p again
// admits another waiter, so each attempt must be finished once.
func (q *targetQueue) finish(p *pending) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dropOutstanding(p)
	q.dropWaiting(p)
	if p.confirmed {
		q.admit()
		return
	}
	q.admitIfAllOutstandingConfirmed()
}

// admitIfAllOutstandingConfirmed admits the queue's head once every request
// outstanding at the target has confirmed: once a queue exists, one request at
// a time sits at the target between confirmations. Caller holds q.mu.
func (q *targetQueue) admitIfAllOutstandingConfirmed() {
	for _, o := range q.outstanding {
		if !o.confirmed {
			return
		}
	}
	q.admit()
}

// admit admits the queue's next-ranked waiter, if any, consulting nothing
// else at the target. The admitted waiter counts as outstanding from that
// moment, so it holds the queue's next admission until its own first content
// confirms it. Caller holds q.mu.
func (q *targetQueue) admit() {
	if len(q.waiting) == 0 {
		return
	}
	p := q.waiting[0]
	q.waiting = q.waiting[1:]
	p.arrived = time.Now()
	q.outstanding = append(q.outstanding, p)
	close(p.admitted)
}

// insert places p in the waiting line by rank, oldest session first. Caller
// holds q.mu.
func (q *targetQueue) insert(p *pending) {
	q.waiting = append(q.waiting, p)
	slices.SortStableFunc(q.waiting, func(a, b *pending) int { return a.rank.Compare(b.rank) })
}

// dropWaiting removes p from the waiting line, if it is there. Caller holds
// q.mu.
func (q *targetQueue) dropWaiting(p *pending) {
	q.waiting = slices.DeleteFunc(q.waiting, func(o *pending) bool { return o == p })
}

// dropOutstanding removes p from the outstanding set, if it is there. Caller
// holds q.mu.
func (q *targetQueue) dropOutstanding(p *pending) {
	q.outstanding = slices.DeleteFunc(q.outstanding, func(o *pending) bool { return o == p })
}
