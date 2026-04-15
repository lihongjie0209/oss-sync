package syncer

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/oss-sync/db"
	"github.com/oss-sync/store"

	"golang.org/x/time/rate"
)

// SyncTask represents a single file to be synced from source to destination.
type SyncTask struct {
	SourceKey string
	DestKey   string
	Size      int64
}

// Worker holds the dependencies needed to execute a single sync task.
type Worker struct {
	scope    string
	src      store.Source
	dst      store.Destination
	database *db.DB
	limiter  *rate.Limiter
	tracker  *TransferTracker
	retries  int
}

// WorkerPool manages a pool of concurrent sync workers.
type WorkerPool struct {
	worker Worker
	tasks  chan SyncTask
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

// NewWorkerPool creates a worker pool with the given concurrency.
func NewWorkerPool(
	ctx context.Context,
	concurrency int,
	scope string,
	src store.Source,
	dst store.Destination,
	database *db.DB,
	limiter *rate.Limiter,
	tracker *TransferTracker,
	retries int,
) *WorkerPool {
	ctx, cancel := context.WithCancel(ctx)
	pool := &WorkerPool{
		worker: Worker{
			scope:    scope,
			src:      src,
			dst:      dst,
			database: database,
			limiter:  limiter,
			tracker:  tracker,
			retries:  retries,
		},
		tasks:  make(chan SyncTask, concurrency*2),
		ctx:    ctx,
		cancel: cancel,
	}

	pool.wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go pool.runWorker()
	}

	return pool
}

// Submit enqueues a task. Blocks if the queue is full.
// Returns false if the pool context has been cancelled.
func (p *WorkerPool) Submit(task SyncTask) bool {
	select {
	case p.tasks <- task:
		return true
	case <-p.ctx.Done():
		return false
	}
}

// Close signals all workers to stop after draining the queue and waits for them.
func (p *WorkerPool) Close() {
	close(p.tasks)
	p.wg.Wait()
	p.cancel()
}

// Cancel stops all workers immediately (use on fatal errors).
func (p *WorkerPool) Cancel() {
	p.cancel()
}

func (p *WorkerPool) runWorker() {
	defer p.wg.Done()
	for task := range p.tasks {
		if p.ctx.Err() != nil {
			return
		}
		if err := p.worker.process(p.ctx, task); err != nil {
			log.Printf("[ERROR] sync %s -> %s: %v", task.SourceKey, task.DestKey, err)
		}
	}
}

// process downloads from source and uploads to destination for a single task.
func (w *Worker) process(ctx context.Context, task SyncTask) error {
	attempts := w.retries
	if attempts <= 0 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		lastErr = w.processOnce(ctx, task)
		if lastErr == nil {
			if err := w.database.MarkSynced(w.scope, task.DestKey); err != nil {
				return fmt.Errorf("mark synced: %w", err)
			}
			w.tracker.MarkFileCompleted()
			log.Printf("[OK] synced %s -> %s (%d bytes)", task.SourceKey, task.DestKey, task.Size)
			return nil
		}

		if attempt < attempts {
			log.Printf("[WARN] attempt %d/%d failed for %s -> %s: %v", attempt, attempts, task.SourceKey, task.DestKey, lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
			}
		}
	}

	_ = w.database.MarkFailed(w.scope, task.DestKey, lastErr.Error())
	return lastErr
}

func (w *Worker) processOnce(ctx context.Context, task SyncTask) error {
	stream, err := w.src.GetObjectStream(task.SourceKey)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer stream.Close()

	limited := WrapReader(ctx, stream, w.limiter, w.tracker)

	if err := w.dst.PutObjectFromStream(task.DestKey, limited, task.Size); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}
