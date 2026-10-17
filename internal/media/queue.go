package media

import (
	"context"
	"slices"
	"sync"
)

// task is one queued or running unit of work on a media row.
type task struct {
	mediaID   int64
	messageID int64
	high      bool // on demand: runs first and may take the slow paths
	running   bool
	done      chan struct{}
	err       error
}

// workQueue runs each media row at most once at a time. On-demand work goes
// before background work; background work is dropped when the queue is full
// (a later sweep finds it again).
type workQueue struct {
	mu        sync.Mutex
	high, low []*task
	byID      map[int64]*task // queued or running
	wake      chan struct{}
	maxLow    int
}

func newQueue(maxLow int) *workQueue {
	return &workQueue{byID: map[int64]*task{}, wake: make(chan struct{}, 1), maxLow: maxLow}
}

func (q *workQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// submit queues work on a media row, or returns the task already queued or
// running for it (promoted when this request is on demand). ok is false when
// background work was dropped because the queue is full.
func (q *workQueue) submit(mediaID, messageID int64, high bool) (*task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t, found := q.byID[mediaID]; found {
		if high && !t.high && !t.running {
			for i, x := range q.low {
				if x == t {
					q.low = append(q.low[:i], q.low[i+1:]...)
					break
				}
			}
			t.high = true
			q.high = append(q.high, t)
			q.signal()
		}
		return t, true
	}
	if !high && len(q.low) >= q.maxLow {
		return nil, false
	}
	t := &task{mediaID: mediaID, messageID: messageID, high: high, done: make(chan struct{})}
	q.byID[mediaID] = t
	if high {
		q.high = append(q.high, t)
	} else {
		q.low = append(q.low, t)
	}
	q.signal()
	return t, true
}

// next blocks until work is available or ctx ends.
func (q *workQueue) next(ctx context.Context) (*task, bool) {
	for {
		q.mu.Lock()
		var t *task
		switch {
		case len(q.high) > 0:
			t, q.high = q.high[0], q.high[1:]
		case len(q.low) > 0:
			t, q.low = q.low[0], q.low[1:]
		}
		if t != nil {
			t.running = true
			if len(q.high)+len(q.low) > 0 {
				q.signal()
			}
			q.mu.Unlock()
			return t, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, false
		case <-q.wake:
		}
	}
}

// claim makes the caller the only work on a media row until release: it
// takes over a task still queued for the row (its waiters are released when
// the claim ends), waits for a running one to end, and holds the row as a
// running task, so a submit for it joins the claim instead of starting work.
func (q *workQueue) claim(ctx context.Context, mediaID, messageID int64) (release func(), err error) {
	for {
		q.mu.Lock()
		t, found := q.byID[mediaID]
		if !found || !t.running {
			if found {
				q.high = slices.DeleteFunc(q.high, func(x *task) bool { return x == t })
				q.low = slices.DeleteFunc(q.low, func(x *task) bool { return x == t })
			} else {
				t = &task{mediaID: mediaID, messageID: messageID, done: make(chan struct{})}
				q.byID[mediaID] = t
			}
			t.running = true
			q.mu.Unlock()
			return func() { q.finish(t, nil) }, nil
		}
		q.mu.Unlock()
		select {
		case <-t.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// finish records the outcome and releases everyone waiting on the task.
func (q *workQueue) finish(t *task, err error) {
	q.mu.Lock()
	delete(q.byID, t.mediaID)
	t.err = err
	q.mu.Unlock()
	close(t.done)
}

// waiting is the number of queued tasks, running ones excluded.
func (q *workQueue) waiting() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.high) + len(q.low)
}

// position is how many tasks run before t (0 when running).
func (q *workQueue) position(t *task) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if t.running {
		return 0
	}
	for i, x := range q.high {
		if x == t {
			return i + 1
		}
	}
	for i, x := range q.low {
		if x == t {
			return len(q.high) + i + 1
		}
	}
	return 0
}
