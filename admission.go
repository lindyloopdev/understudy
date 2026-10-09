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

// errUnavailableForNewSessions is the error a first turn's wait returns when
// its target has held arrivals past the failover threshold and another
// target is taking new sessions. Not a target failure: nothing is recorded
// against the target it left.
var errUnavailableForNewSessions = errors.New("left a target unavailable for new sessions")

// initialFirstContentTimeout is how long a request's first attempt at a
// target may produce no content before the attempt is ended and the request
// sent again. It must clear honest queueing at a slow backend: the longest
// healthy time to first content measured was 26.2s (lindy repo,
// notes/2026-09-30-time-to-first-content-measurements.md).
//
// TODO(TODO.d/session-ordered-admission.md): take the first value from the
// consumer's own time limit when one is supplied, instead of this default.
const initialFirstContentTimeout = 60 * time.Second

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
// is unbuilt, so a dropped tenant's conversation state is reclaimed by the
// forget backstop rather than at teardown.
func (s *server) sessionAge(tenant, key string) time.Time {
	now := time.Now()
	if key == "" {
		return now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictForgottenSessions(now)
	ages, ok := s.sessionAges[tenant]
	if !ok {
		ages = make(map[string]sessionRecord)
		s.sessionAges[tenant] = ages
	}
	if rec, ok := ages[key]; ok {
		ages[key] = sessionRecord{firstSeen: rec.firstSeen, lastSeen: now}
		return rec.firstSeen
	}
	ages[key] = sessionRecord{firstSeen: now, lastSeen: now}
	return now
}

// evictForgottenSessions drops every conversation quiet for
// [sessionForgetAfter], and every tenant left with nothing.
//
// TODO(TODO.d/expose-operator-metrics.md): nothing tests this eviction. Test
// it through the tracked-conversations gauge once it exists.
//
// Caller holds s.mu.
func (s *server) evictForgottenSessions(now time.Time) {
	for tenant, conversations := range s.sessionAges {
		for key, rec := range conversations {
			if now.Sub(rec.lastSeen) >= sessionForgetAfter {
				delete(conversations, key)
			}
		}
		// A tenant with nothing left is a run that ended.
		if len(conversations) == 0 {
			delete(s.sessionAges, tenant)
		}
	}
}

// admissionQueue returns the admission queue for a target's availability key
// (canonical url + key + model), the scope providers limit and slots are
// handed out on. upstream is the target's server's canonical url and key,
// which the queue keeps so a busy refusal can find the server's other models.
func (s *server) admissionQueue(key, upstream string) *targetQueue {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.admission[key]
	if !ok {
		q = &targetQueue{
			upstream:             upstream,
			firstContentSchedule: s.firstContentSchedule,
			failoverThreshold:    s.failoverThreshold,
			drained:              func() { s.modelsDrained(upstream) },
			headered:             func() { s.modelsResumed(upstream) },
		}
		s.admission[key] = q
	}
	return q
}

// queueBehindServerBusy returns p to its target's waiting line behind a busy
// refusal, reporting whether it did — but only when another of the server's
// models has a request running: a busy refusal with nothing running on the
// server is the server's answer about the model itself.
//
// TODO(TODO.d/session-ordered-admission.md): rank the server's waiting
// requests together across its models.
func (s *server) queueBehindServerBusy(upstream string, p *pending) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var running []*targetQueue
	for _, q := range s.admission {
		if q.upstream != upstream || q.idle() {
			continue
		}
		running = append(running, q)
	}
	if len(running) == 0 {
		return false
	}
	p.queue.mu.Lock()
	p.queue.insert(p)
	p.serverBusyWait = true
	p.queue.mu.Unlock()
	if s.busyLinks[upstream] == nil {
		s.busyLinks[upstream] = make(map[*targetQueue]struct{})
	}
	s.busyLinks[upstream][p.queue] = struct{}{}
	for _, q := range running {
		q.mu.Lock()
		q.refusedModelWaiting = true
		q.mu.Unlock()
	}
	return true
}

// modelsDrained admits each queue linked to the server's models by a busy
// refusal, once one of the server's queues has no request outstanding. Each
// queue's own admission rule re-runs, so a request refused again forms a new
// link; queues the wait holds stay held — [server.modelsResumed] releases
// them once the server takes the re-sent request.
func (s *server) modelsDrained(upstream string) {
	s.mu.Lock()
	linked := s.busyLinks[upstream]
	if len(linked) == 0 {
		s.mu.Unlock()
		return
	}
	delete(s.busyLinks, upstream)
	s.mu.Unlock()
	for q := range linked {
		q.mu.Lock()
		q.admit()
		q.mu.Unlock()
	}
}

// modelsResumed lets the server's models take requests again once the
// request a busy refusal queued is no longer waiting on the server: its
// re-send drew a standing response header, or was answered without the
// server taking it ([server.serverBusyWaitEnded]).
func (s *server) modelsResumed(upstream string) {
	s.mu.Lock()
	var holding []*targetQueue
	for _, q := range s.admission {
		if q.upstream != upstream {
			continue
		}
		q.mu.Lock()
		if q.refusedModelWaiting {
			holding = append(holding, q)
			q.refusedModelWaiting = false
		}
		q.mu.Unlock()
	}
	s.mu.Unlock()
	for _, q := range holding {
		q.mu.Lock()
		q.admitIfAllOutstandingConfirmed()
		q.mu.Unlock()
	}
}

// serverBusyWaitEnded ends the request's wait behind a busy refusal: the
// re-send was answered without the server taking it — no response header
// stood, so its error goes to the client. A request not waiting behind a
// busy refusal changes nothing.
func (s *server) serverBusyWaitEnded(upstream string, p *pending) {
	p.queue.mu.Lock()
	waiting := p.serverBusyWait
	p.serverBusyWait = false
	p.queue.mu.Unlock()
	if !waiting {
		return
	}
	s.modelsResumed(upstream)
}

// targetQueue orders admission to one target by session age: the backend
// cannot see conversations, so the layer holding every request orders them —
// oldest session first, so sessions finish rather than interleave turn by
// turn. It exists only under contention: with nothing waiting, a request
// passes straight through.
type targetQueue struct {
	mu sync.Mutex
	// upstream is the target's server, its canonical url and key: the scope
	// a busy refusal links the server's models across.
	upstream string
	// outstanding holds every attempt at the target — sent, or admitted and
	// on its way — until it ends or its first content confirms it.
	outstanding []*pending
	// waiting holds queued attempts in rank order, oldest session first.
	waiting []*pending
	// firstContentSchedule schedules every first-content timeout the queue
	// arms; it is taken from the server at the queue's creation.
	firstContentSchedule func(d time.Duration, fire func()) *time.Timer
	// failoverThreshold is how long the queue may hold arrivals before it
	// counts as unavailable for new sessions — the walk routes first turns
	// around the target, and a waiting first turn leaves for another target;
	// it is taken from the server at the queue's creation.
	failoverThreshold time.Duration
	// drained runs when the target's outstanding requests reach zero; it is
	// taken from the server at the queue's creation and runs without q.mu
	// held.
	drained func()
	// headered runs when a send's response header arrives and the send
	// stands; it is taken from the server at the queue's creation and runs
	// without q.mu held.
	headered func()
	// refusedModelWaiting is set while a request the server refused as busy
	// waits on the server, through the re-send that ends the wait: the
	// model's running requests finish undisturbed, but it takes no new
	// request until then. The server sets and clears it under q.mu.
	refusedModelWaiting bool
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
	// left is closed to end the wait of a first turn whose target has held
	// arrivals past the failover threshold, so the walk routes it elsewhere.
	left chan struct{}
	// firstContentTimeouts counts the attempts at this target the
	// first-content timeout has ended; each doubles the next attempt's
	// timeout. Per target: one target's slowness says nothing about
	// another's.
	firstContentTimeouts int
	// firstContentTimer is the first-content timeout now in force, if any.
	firstContentTimer *time.Timer
	// endedWhileRefusedModelWaiting records that the attempt ended while a
	// refused model's request waited on the server: the completion admits no
	// waiter, leaving the freed place to the refused model's request, and the
	// wait owes no refill once it ends.
	endedWhileRefusedModelWaiting bool
	// serverBusyWait is set while this attempt waits behind a busy refusal
	// and its wait holds the server's other models from taking new requests:
	// from the refusal until the server takes the re-sent request, or the
	// wait ends in an answer to the client. The server sets and clears it
	// under q.mu.
	serverBusyWait bool
}

// pastGrace reports whether p's grace period has elapsed as of now.
func (p *pending) pastGrace(now time.Time) bool {
	return !p.arrived.Add(admissionGracePeriod).After(now)
}

// holdingSinceLocked reports whether q would hold an arrival arriving at
// now, and the earliest moment its state shows that holding beginning: an
// unconfirmed request past its grace period holds from the moment its grace
// period ended — a displaced request no less than one at the target, since
// displacement moved nothing — and a request that arrived to wait holds from
// its arrival. The clock comes from the queue's own state,
// not from arrivals: a target skipped on a first turn may get none. Caller
// holds q.mu.
func (q *targetQueue) holdingSinceLocked(now time.Time) (time.Time, bool) {
	var since time.Time
	for _, p := range q.waiting {
		at := p.arrived
		if p.preempted {
			// A displaced request's arrival is its admission's; the
			// target held that admission from its grace period's end.
			at = at.Add(admissionGracePeriod)
		}
		if since.IsZero() || at.Before(since) {
			since = at
		}
	}
	for _, o := range q.outstanding {
		if o.confirmed || !o.pastGrace(now) {
			continue
		}
		if at := o.arrived.Add(admissionGracePeriod); since.IsZero() || at.Before(since) {
			since = at
		}
	}
	return since, !since.IsZero()
}

// failoverCrossingLocked reports the moment the queue's holding of arrivals
// crosses the failover threshold, and whether that moment has come by now.
// Both the leave of a waiting first turn and the walk's routing read this
// crossing, so the two cannot disagree about when busy becomes unavailable.
// at is zero when the queue holds nothing. Caller holds q.mu.
func (q *targetQueue) failoverCrossingLocked(now time.Time) (at time.Time, crossed bool) {
	since, held := q.holdingSinceLocked(now)
	if !held {
		return time.Time{}, false
	}
	at = since.Add(q.failoverThreshold)
	return at, !at.After(now)
}

// enter admits an attempt to q's target, blocking while the queue holds it.
// rank is the attempt's session age; cancel ends its upstream attempt when
// preemption displaces it. A request the queue admits returns unblocked and
// counts as outstanding from that moment, so it holds the queue's next
// admission until its first content confirms it. moveOn, when non-nil, is
// consulted when the target has held arrivals past the failover threshold;
// if it reports another target taking new sessions, the wait ends with
// [errUnavailableForNewSessions]; otherwise it reports a moment to consult
// it again, and the check reruns then. A nil moveOn waits regardless.
func (q *targetQueue) enter(ctx context.Context, cancel context.CancelCauseFunc, rank time.Time, moveOn func() (bool, time.Time)) (*pending, error) {
	p := &pending{queue: q, rank: rank, cancel: cancel, admitted: make(chan struct{}), left: make(chan struct{})}
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
	// behind any request unconfirmed past its grace period, headered or not
	// — or, while a refused model's request waits on the server, behind that
	// wait too.
	_, held := q.holdingSinceLocked(now)
	if held || q.refusedModelWaiting {
		q.insert(p)
		if moveOn != nil {
			at, _ := q.failoverCrossingLocked(now)
			q.leaveWhenUnavailable(p, at, moveOn)
		}
		q.mu.Unlock()
		select {
		case <-p.admitted:
			return p, nil
		case <-p.left:
			return nil, errUnavailableForNewSessions
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
	// A request the queue sends to the target a second time arms no timer of
	// its own, so a later arrival from an older session can have it cancelled
	// and queued again. The target has not begun to answer it by then, so
	// each repeat costs another send. It loses its place only to an older
	// session's request, which this queue would send first anyway.
	time.AfterFunc(admissionGracePeriod, func() { q.formByWaiting(p) })
	q.mu.Unlock()
	return p, nil
}

// leaveWhenUnavailable ends p's wait once the queue has held arrivals past
// the failover threshold and moveOn reports another target taking new
// sessions. Holding can begin later than at — the earliest holder may
// leave — so the check reruns at the crossing the queue's holding start
// then names, and at the moment moveOn names when it reports none taking
// new sessions.
func (q *targetQueue) leaveWhenUnavailable(p *pending, at time.Time, moveOn func() (bool, time.Time)) {
	time.AfterFunc(time.Until(at), func() {
		now := time.Now()
		q.mu.Lock()
		if !slices.Contains(q.waiting, p) {
			// Admitted, or gone with its client: the wait already ended.
			q.mu.Unlock()
			return
		}
		next, crossed := q.failoverCrossingLocked(now)
		q.mu.Unlock()
		if !crossed && next.After(now) {
			q.leaveWhenUnavailable(p, next, moveOn)
			return
		}
		// moveOn takes the server's lock, so it is called without q.mu held.
		taking, askAgain := moveOn()
		if !taking {
			if askAgain.After(now) {
				q.leaveWhenUnavailable(p, askAgain, moveOn)
			}
			return
		}
		q.mu.Lock()
		defer q.mu.Unlock()
		if slices.Contains(q.waiting, p) {
			q.dropWaiting(p)
			close(p.left)
		}
	})
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
		if o == probe && !probeHeld {
			continue
		}
		o.preempted = true
		q.insert(o)
		o.cancel(errPreempted)
	}
}

// headerArrived records that a send's response header has arrived, and
// reports whether the send still stands. A send already preempted — by an
// outranking arrival, or by its own first-content timeout — does not: its
// response slipped past the preemption decision and is discarded, and the
// caller sends the request again.
func (q *targetQueue) headerArrived(p *pending) bool {
	q.mu.Lock()
	if !p.preempted {
		p.headered = true
		// The server took the re-sent request, ending the hold its
		// wait had on the server's other models.
		p.serverBusyWait = false
		q.mu.Unlock()
		q.headered()
		return true
	}
	stands := !q.endSend(p)
	q.mu.Unlock()
	if !stands {
		q.notifyDrained()
	}
	return stands
}

// sendEnded records a send ending without an answer to relay — its call
// failed, or its relay ended before any content, however it ended — and
// reports whether it was preempted before any content, in which case the
// caller waits for the queue to admit the request again instead of recording
// or relaying anything.
func (q *targetQueue) sendEnded(p *pending) bool {
	q.mu.Lock()
	preempted := q.endSend(p)
	q.mu.Unlock()
	q.notifyDrained()
	return preempted
}

// unavailableForNewSessions reports whether the queue has held arrivals for
// the failover threshold by now — the crossing
// [targetQueue.failoverCrossingLocked] defines — so its target is
// unavailable for new sessions.
func (q *targetQueue) unavailableForNewSessions(now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, crossed := q.failoverCrossingLocked(now)
	return crossed
}

// requeueBehindContent returns p to the waiting line at its rank, reporting
// whether it did. It re-queues only when another request at the target is
// confirmed — its stream still open. The check and the re-queue are one
// step, so a stream ending in between cannot leave p waiting on nothing
// outstanding.
func (q *targetQueue) requeueBehindContent(p *pending) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !slices.ContainsFunc(q.outstanding, func(o *pending) bool { return o.confirmed }) {
		return false
	}
	q.insert(p)
	return true
}

// idle reports whether no request of the target is outstanding at its
// backend.
func (q *targetQueue) idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.outstanding) == 0
}

// notifyDrained runs the queue's drain notification when no request is
// outstanding. It runs without q.mu held, since the notification takes the
// server's lock; a request arriving first only defers the notification to
// its own ending.
func (q *targetQueue) notifyDrained() {
	if q.idle() {
		q.drained()
	}
}

// endSend takes p's send out of the target's outstanding requests, admits
// whoever that frees a place for — no one while a refused model's request
// waits on the server — and reports whether the send was
// preempted before any content. Caller holds q.mu.
func (q *targetQueue) endSend(p *pending) bool {
	q.dropOutstanding(p)
	p.endedWhileRefusedModelWaiting = q.refusedModelWaiting
	q.admitIfAllOutstandingConfirmed()
	return p.preempted && !p.confirmed
}

// armFirstContentTimeout arms the first-content timeout for p's current
// attempt: no content within the timeout — whether the attempt is still
// waiting for its header or reading a body that has sent none — ends the
// attempt and the request is sent again. Each earlier attempt at this target
// this timeout ended doubles it, so an honest attempt slower than the first is
// not canceled and restarted forever; a displacement leaves it where it was.
// First content disables it: once content has arrived, silence is the stream
// idle deadline's to bound, not this timeout's.
func (q *targetQueue) armFirstContentTimeout(p *pending) {
	q.mu.Lock()
	defer q.mu.Unlock()
	// The handler reads timer only under q.mu, which is held here until it
	// is set, so even a timer that fires at once finds it.
	var timer *time.Timer
	timer = q.firstContentSchedule(initialFirstContentTimeout<<p.firstContentTimeouts, func() { q.expireFirstContentTimeout(p, &timer) })
	p.firstContentTimer = timer
}

// expireFirstContentTimeout preempts p's attempt when its first-content timeout
// expires with no content arrived. Ending it commits nothing to the client —
// the relay withholds the upstream's status, headers, and pre-content bytes
// until first content — so the request can be sent again to the same target.
// timer is the timer that fired. One that is no longer p's timeout in force
// was started for a send that has since ended; that firing does nothing.
//
// TODO(TODO.d/session-ordered-admission.md): when the timeout fires on a
// backend that gives no work before its header, every headerless request at
// that target should restart together in send order, so the retry restores
// ordering rather than scrambling it; this ends only the one attempt.
func (q *targetQueue) expireFirstContentTimeout(p *pending, timer **time.Timer) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if *timer != p.firstContentTimer || p.confirmed || p.preempted || !slices.Contains(q.outstanding, p) {
		return
	}
	p.preempted = true
	p.firstContentTimeouts++
	q.insert(p)
	p.cancel(errors.New("no content within the first-content timeout"))
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
// [time.Timer.Stop] cannot stop a timer whose handler has not yet run, so the
// timer is cleared as well; such a late firing finds firstContentTimer no
// longer in force.
func (q *targetQueue) rearm(p *pending, cancel context.CancelCauseFunc) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.firstContentTimer != nil {
		p.firstContentTimer.Stop()
		p.firstContentTimer = nil
	}
	p.preempted = false
	p.cancel = cancel
	p.admitted = make(chan struct{})
}

// confirm records p's first content — the moment the target has begun
// answering — which frees the queue to admit its next waiter unless a
// refused model's request waits on the server, and reports
// whether the send still stands. A preempted send — by an outranking arrival,
// or by its own first-content timeout — does not: its content is discarded,
// and the caller sends the client nothing.
func (q *targetQueue) confirm(p *pending) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.preempted {
		return false
	}
	if !p.confirmed {
		p.confirmed = true
		q.admitIfAllOutstandingConfirmed()
	}
	return true
}

// finish ends p's standing at the target: its attempt is over, so it holds the
// queue's next admission no longer. A confirmed p refills its slot at once,
// with no grace check; any other ending admits only through growth's
// grace-gated check — unless a refused model's request waited on the server
// when the send ended, in which case the completion admits no one. It is not
// idempotent: finishing a confirmed p again admits another waiter, so each
// attempt must be finished once.
func (q *targetQueue) finish(p *pending) {
	q.mu.Lock()
	q.dropOutstanding(p)
	q.dropWaiting(p)
	if !p.endedWhileRefusedModelWaiting {
		if p.confirmed {
			q.admit()
		} else {
			q.admitIfAllOutstandingConfirmed()
		}
	}
	q.mu.Unlock()
	q.notifyDrained()
}

// admitIfAllOutstandingConfirmed admits the queue's head once every request
// outstanding at the target has confirmed: once a queue exists, one request at
// a time sits at the target between confirmations. It admits no one while a
// refused model's request waits on the server. Caller holds q.mu.
func (q *targetQueue) admitIfAllOutstandingConfirmed() {
	for _, o := range q.outstanding {
		if !o.confirmed {
			return
		}
	}
	if q.refusedModelWaiting {
		return
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

// insert places p in the waiting line by rank, oldest session first. It
// replaces p.admitted, which the last admission already closed: the closed
// channel would let a displaced attempt's re-send through without waiting
// for its next turn. Caller holds q.mu.
func (q *targetQueue) insert(p *pending) {
	p.admitted = make(chan struct{})
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
