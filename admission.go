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
// target's own turnover, instead of this default.
const initialFirstContentTimeout = 60 * time.Second

// sessionForgetAfter is how long a conversation may go with no request before
// its age is forgotten and it ranks as new again, so a session that keeps
// taking turns — however old — is never outranked by one that has been away
// longer. A backstop for consumers that send no session-end signal, and the
// same one a conversation's target is forgotten on, so no record outlives it.
const sessionForgetAfter = 24 * time.Hour

// sessionRecord is a conversation's standing in admission: firstSeen is its
// rank, and lastSeen is when it last took a turn, which [sessionForgetAfter]
// is measured from.
type sessionRecord struct {
	firstSeen time.Time
	lastSeen  time.Time
}

func (r sessionRecord) idle(now time.Time) bool {
	return now.Sub(r.lastSeen) >= sessionForgetAfter
}

// sessionAge returns when tenant's conversation was first seen, recording the
// first sighting and refreshing the last. The age is the session's admission
// rank, so an old session's later turn outranks a younger session's earlier
// one, and the rank holds through quiet stretches shorter than
// [sessionForgetAfter]. A request with no conversation key ranks by its own
// arrival.
//
// TODO(TODO.d/admission-reservation-window.md): forget the session's age on
// the consumer's session-end signal.
func (s *server) sessionAge(tenant, key string) time.Time {
	now := time.Now()
	if key == "" {
		return now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictForgottenSessions(now)
	c := s.conversationLocked(tenant, key)
	if c.session != nil {
		c.session.lastSeen = now
		return c.session.firstSeen
	}
	c.session = &sessionRecord{firstSeen: now, lastSeen: now}
	return now
}

// evictForgottenSessions forgets every conversation quiet for
// [sessionForgetAfter]: its place in line and its target go together.
//
// Caller holds s.mu.
func (s *server) evictForgottenSessions(now time.Time) {
	s.evictConversationParts(func(c *conversation) {
		if c.session != nil && c.session.idle(now) {
			c.session, c.served = nil, nil
		}
	})
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
			changed:              func() { s.applyBusyRefusal(upstream); s.moveWaitingFirstRequests() },
		}
		s.admission[key] = q
	}
	return q
}

// queueBehindServerBusy returns p to its target's waiting line behind a busy
// refusal, reporting whether it did — but only when another of the server's
// models has a request running: a busy refusal with nothing running on the
// server is the server's answer about the model itself.
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
	account := s.upstreamAccountLocked(upstream)
	if account.busyRefusal == nil {
		account.busyRefusal = &busyRefusal{waiters: make(map[*pending]struct{})}
	}
	refusal := account.busyRefusal
	refusal.waiters[p] = struct{}{}
	refusal.awaited = make(map[*targetQueue]struct{}, len(running))
	for _, q := range running {
		refusal.awaited[q] = struct{}{}
		q.mu.Lock()
		if !q.refusedModelWaiting() || p.rank.Before(q.refusedModelWaitRank) {
			q.refusedModelWaitRank = p.rank
		}
		q.mu.Unlock()
	}
	return true
}

// busyRefusal is a server's standing busy refusal (DESIGN.md §Session-Ordered
// Admission, *The queue is keyed on the target*).
type busyRefusal struct {
	// waiters are the requests the server refused as busy. One waits on the
	// server while its serverBusyWait is set, including between a re-send's
	// refusal and its return to its queue, when it is in no queue at all.
	waiters map[*pending]struct{}
	// awaited are the models running at the latest refusal that have not
	// had their last request end since: each that does is room the
	// refused requests are sent again into.
	awaited map[*targetQueue]struct{}
}

// applyBusyRefusal applies a busy refusal's rules to the server's models after
// a change at one of them, from how they stand now. While a refused request
// waits on the server, each awaited model left with no request outstanding
// sends the refused requests again; once none waits — the server took the
// re-send, or its client was answered or left — the server's models take
// requests again in rank order. Runs without any queue's lock held.
func (s *server) applyBusyRefusal(upstream string) {
	s.mu.Lock()
	account := s.upstreams[upstream]
	if account == nil || account.busyRefusal == nil {
		s.mu.Unlock()
		return
	}
	refusal := account.busyRefusal
	var resendQueues []*targetQueue
	for p := range refusal.waiters {
		p.queue.mu.Lock()
		if p.serverBusyWait && slices.Contains(p.queue.waiting, p) && !slices.Contains(resendQueues, p.queue) {
			resendQueues = append(resendQueues, p.queue)
		}
		if !p.serverBusyWait {
			delete(refusal.waiters, p)
		}
		p.queue.mu.Unlock()
	}
	if len(refusal.waiters) == 0 {
		account.busyRefusal = nil
		var holding []*targetQueue
		for _, q := range s.admission {
			if q.upstream != upstream {
				continue
			}
			q.mu.Lock()
			if q.refusedModelWaiting() {
				holding = append(holding, q)
				q.refusedModelWaitRank = time.Time{}
			}
			q.mu.Unlock()
		}
		s.mu.Unlock()
		for _, q := range holding {
			q.mu.Lock()
			q.admitIfAllOutstandingConfirmed()
			q.mu.Unlock()
		}
		return
	}
	freed := false
	for q := range refusal.awaited {
		if q.idle() {
			delete(refusal.awaited, q)
			freed = true
		}
	}
	s.mu.Unlock()
	if !freed {
		return
	}
	for _, q := range resendQueues {
		q.mu.Lock()
		q.admit()
		q.mu.Unlock()
	}
}

// serverBusyWaitEnded ends the request's wait behind a busy refusal: the
// re-send was answered without the server taking it — no response header
// stood, so its error goes to the client — or the request's wait to re-send
// ended in error, its client having left. A request not waiting behind a
// busy refusal changes nothing. The wait held its queue's arrivals too, so
// that queue admits here as well: it is not necessarily among the queues
// the refusal's other waiters wait in.
func (s *server) serverBusyWaitEnded(upstream string, p *pending) {
	p.queue.mu.Lock()
	waiting := p.serverBusyWait
	p.serverBusyWait = false
	p.queue.mu.Unlock()
	if !waiting {
		return
	}
	s.applyBusyRefusal(upstream)
	p.queue.mu.Lock()
	p.queue.admitIfAllOutstandingConfirmed()
	p.queue.mu.Unlock()
}

// moveWaitingFirstRequests reruns the leave check of every first request
// waiting at a target unavailable for new sessions: such a target names no
// moment of its own to check at, so a request in its line learns another
// target stopped holding arrivals only from a queue change. The moments a
// check names stay the wait's timers' to keep. Takes the server's and each
// queue's lock itself, so callers must hold neither.
func (s *server) moveWaitingFirstRequests() {
	s.mu.Lock()
	queues := make([]*targetQueue, 0, len(s.admission))
	for _, q := range s.admission {
		queues = append(queues, q)
	}
	s.mu.Unlock()
	now := time.Now()
	for _, q := range queues {
		q.mu.Lock()
		_, crossed := q.failoverCrossingLocked(now)
		waiting := slices.Clone(q.waiting)
		q.mu.Unlock()
		if !crossed {
			continue
		}
		for _, p := range waiting {
			q.leaveIfAnotherTargetTakes(p, now)
		}
	}
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
	// changed runs after a send's response header arrives or a send ends,
	// so the server can apply its busy-refusal rules and rerun the leave
	// checks of waiting first requests; it is taken from the server at the
	// queue's creation and runs without q.mu held.
	changed func()
	// refusedModelWaitRank is the rank of the earliest request the server
	// refused as busy, zero while none waits. It is set while that request
	// waits on the server, through the re-send that ends the wait: the
	// model's running requests finish undisturbed, and it takes no new
	// request that waiter outranks. The server sets and clears it under
	// q.mu.
	refusedModelWaitRank time.Time
	// holdingSince is when the queue's present holding of arrivals began,
	// the clock the failover crossing counts from. A request leaving the
	// queue leaves it standing; it is zero while the queue would take an
	// arrival without holding it. holdingSinceLocked updates it.
	holdingSince time.Time
}

// refusedModelWaiting reports whether a request the server refused as busy
// waits on the server. Caller holds q.mu.
func (q *targetQueue) refusedModelWaiting() bool {
	return !q.refusedModelWaitRank.IsZero()
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
	// moveOn is the leave check of a first request waiting in enter: it
	// reports whether another target would take a first request arriving
	// now, and otherwise the moment to check again. Set only while that
	// wait runs, so both the wait's timers and a queue change rerun the
	// check while it is set.
	moveOn func() (bool, time.Time)
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
// now, and when the present holding began, keeping [targetQueue.holdingSince]
// from moving later once set. The members name the holding's start: an
// unconfirmed request past its grace period holds from the moment its
// grace period ended — a displaced request no less than one at the target,
// since displacement moved nothing — and a request that arrived to wait
// holds from its arrival. A target skipped on a first turn may never hold,
// leaving the clock zero. Caller holds q.mu.
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
	switch {
	case since.IsZero():
		q.holdingSince = time.Time{}
	case q.holdingSince.IsZero() || since.Before(q.holdingSince):
		q.holdingSince = since
	}
	return q.holdingSince, !q.holdingSince.IsZero()
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
// it again, and the check reruns then or when the target's queue changes.
// A nil moveOn waits regardless.
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
	// — or, while a refused model's request waits on the server, behind
	// that wait too, unless the arrival outranks the wait: the server's
	// top-ranked waiter is then for a running model, so the server takes
	// the arrival.
	_, held := q.holdingSinceLocked(now)
	refusedHolds := q.refusedModelWaiting() && !rank.Before(q.refusedModelWaitRank)
	if held || refusedHolds {
		q.insert(p)
		if moveOn != nil {
			p.moveOn = moveOn
			at, _ := q.failoverCrossingLocked(now)
			q.leaveWhenUnavailable(p, at)
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

// leaveWhenUnavailable runs p's leave check at at, and again at each
// moment the check names.
func (q *targetQueue) leaveWhenUnavailable(p *pending, at time.Time) {
	time.AfterFunc(time.Until(at), func() {
		if next := q.leaveIfAnotherTargetTakes(p, time.Now()); !next.IsZero() {
			q.leaveWhenUnavailable(p, next)
		}
	})
}

// leaveIfAnotherTargetTakes ends p's wait once the queue has held arrivals
// past the failover threshold and p's moveOn reports another target taking
// new sessions, and otherwise returns the moment to check again, or the
// zero time when none is known. The clock stops when the queue would take an
// arrival without holding it, and holding may begin again later, so the check
// reruns at each moment named. moveOn takes the server's lock, so it is called
// without q.mu held.
func (q *targetQueue) leaveIfAnotherTargetTakes(p *pending, now time.Time) (checkAgain time.Time) {
	q.mu.Lock()
	moveOn := p.moveOn
	if moveOn == nil || !slices.Contains(q.waiting, p) {
		q.mu.Unlock()
		return time.Time{}
	}
	next, crossed := q.failoverCrossingLocked(now)
	q.mu.Unlock()
	if !crossed && next.After(now) {
		return next
	}
	taking, askAgain := moveOn()
	if taking {
		q.leave(p)
		return time.Time{}
	}
	if askAgain.After(now) {
		return askAgain
	}
	return time.Time{}
}

// leave drops p from the waiting line and ends its wait, when it still
// waits: the walk routes the request as a first request arriving now.
func (q *targetQueue) leave(p *pending) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if slices.Contains(q.waiting, p) {
		q.dropWaiting(p)
		close(p.left)
	}
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
		q.changed()
		return true
	}
	stands := !q.endSend(p)
	q.mu.Unlock()
	q.changed()
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
	q.changed()
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

// endSend takes p's send out of the target's outstanding requests, admits
// whoever that frees a place for — no one while a refused model's request
// waits on the server — and reports whether the send was
// preempted before any content. Caller holds q.mu.
func (q *targetQueue) endSend(p *pending) bool {
	q.dropOutstanding(p)
	p.endedWhileRefusedModelWaiting = q.refusedModelWaiting()
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
// A headerless p restarts every other headerless request at the target
// alongside it, so a retry restores rank order; each of those ends as a
// displacement, so none doubles its own timeout.
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
	if p.headered {
		return
	}
	for _, o := range q.outstanding {
		if o.headered || o.preempted {
			continue
		}
		o.preempted = true
		q.insert(o)
		o.cancel(errPreempted)
	}
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
// refused model's request waits on the server and outranks that waiter,
// and reports whether the send still stands. A preempted send — by an
// outranking arrival, or by its own first-content timeout — does not: its
// content is discarded, and the caller sends the client nothing.
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
	q.changed()
}

// admitIfAllOutstandingConfirmed admits the queue's head once every request
// outstanding at the target has confirmed: once a queue exists, one request at
// a time sits at the target between confirmations. It admits no one while
// a refused model's request waits on the server and outranks the head.
// Caller holds q.mu.
func (q *targetQueue) admitIfAllOutstandingConfirmed() {
	for _, o := range q.outstanding {
		if !o.confirmed {
			return
		}
	}
	if q.refusedModelWaiting() && (len(q.waiting) == 0 || !q.waiting[0].rank.Before(q.refusedModelWaitRank)) {
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
	// The leave check belongs to the wait in enter; an admitted request
	// waits out any later displacement in await.
	p.moveOn = nil
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
