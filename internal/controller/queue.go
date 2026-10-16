// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sync"
	"time"
)

// workQueue is a de-duplicating queue of Application names: an Application
// is processed by one worker at a time, and refreshes requested while it is
// in flight run once more after it finishes.
type workQueue struct {
	mu       sync.Mutex
	cond     *sync.Cond
	order    []string
	queued   map[string]bool // waiting in order
	hard     map[string]bool // a hard refresh was requested
	inFlight map[string]bool
	dirty    map[string]bool // re-queue when the in-flight item finishes
	closed   bool
}

func newWorkQueue() *workQueue {
	q := &workQueue{queued: map[string]bool{}, hard: map[string]bool{}, inFlight: map[string]bool{}, dirty: map[string]bool{}}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *workQueue) add(name string, hard bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if hard {
		q.hard[name] = true
	}
	if q.inFlight[name] {
		q.dirty[name] = true
		return
	}
	if q.queued[name] {
		return
	}
	q.queued[name] = true
	q.order = append(q.order, name)
	q.cond.Signal()
}

// get blocks for the next item; ok=false once the queue is shut down.
func (q *workQueue) get() (name string, hard bool, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.order) == 0 && !q.closed {
		q.cond.Wait()
	}
	if q.closed {
		return "", false, false
	}
	name, q.order = q.order[0], q.order[1:]
	delete(q.queued, name)
	hard = q.hard[name]
	delete(q.hard, name)
	q.inFlight[name] = true
	return name, hard, true
}

func (q *workQueue) done(name string) {
	q.mu.Lock()
	delete(q.inFlight, name)
	again := q.dirty[name]
	delete(q.dirty, name)
	q.mu.Unlock()
	if again {
		q.add(name, false)
	}
}

func (q *workQueue) shutdown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	q.cond.Broadcast()
}

// len reports the waiting items (tests, metrics).
func (q *workQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}

// debouncer calls fn(key) once per burst of triggers.
type debouncer struct {
	mu     sync.Mutex
	delay  time.Duration
	fn     func(string)
	timers map[string]*time.Timer
}

func newDebouncer(delay time.Duration, fn func(string)) *debouncer {
	return &debouncer{delay: delay, fn: fn, timers: map[string]*time.Timer{}}
}

func (d *debouncer) trigger(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[key]; ok {
		t.Reset(d.delay)
		return
	}
	d.timers[key] = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		delete(d.timers, key)
		d.mu.Unlock()
		d.fn(key)
	})
}

func (d *debouncer) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, t := range d.timers {
		t.Stop()
		delete(d.timers, k)
	}
}
