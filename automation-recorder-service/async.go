package main

import (
	"context"
	"sync"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
)

type asyncJob struct {
	name string
	fn   func(context.Context) error
}

type asyncDispatcher struct {
	ch              chan asyncJob
	workerCount     int
	dropOnQueueFull bool
	baseCtx         context.Context
	startOnce       sync.Once
}

func newAsyncDispatcher(baseCtx context.Context, queueSize, workerCount int, dropOnQueueFull bool) *asyncDispatcher {
	if queueSize <= 0 {
		queueSize = 1000
	}
	if workerCount <= 0 {
		workerCount = 1
	}
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return &asyncDispatcher{ch: make(chan asyncJob, queueSize), workerCount: workerCount, dropOnQueueFull: dropOnQueueFull, baseCtx: baseCtx}
}

func (d *asyncDispatcher) start() {
	d.startOnce.Do(func() {
		for i := 0; i < d.workerCount; i++ {
			go func() {
				for job := range d.ch {
					log.Infof(d.baseCtx, "[ASYNC] Starting job: %s", job.name)
					start := time.Now()
					ctx, cancel := context.WithTimeout(d.baseCtx, 15*time.Second)
					err := job.fn(ctx)
					cancel()
					duration := time.Since(start)
					if err != nil {
						log.Warnf(d.baseCtx, "[ASYNC] Job %s failed after %v: %v", job.name, duration, err)
					} else {
						log.Infof(d.baseCtx, "[ASYNC] Job %s completed successfully in %v", job.name, duration)
					}
				}
			}()
		}
	})
}

func (d *asyncDispatcher) enqueue(ctx context.Context, name string, fn func(context.Context) error) {
	if d == nil {
		return
	}
	d.start()
	job := asyncJob{name: name, fn: fn}
	select {
	case d.ch <- job:
		log.Infof(ctx, "[ASYNC] Job %s enqueued (queue depth: %d/%d)", name, len(d.ch), cap(d.ch))
		return
	default:
		if d.dropOnQueueFull {
			log.Warnf(ctx, "[ASYNC] Queue full (%d); dropping job %s", cap(d.ch), name)
			return
		}
		log.Warnf(ctx, "[ASYNC] Queue full, blocking until space available for job %s", name)
		d.ch <- job
		log.Infof(ctx, "[ASYNC] Job %s enqueued after waiting", name)
	}
}
