package syncer

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/oss-sync/db"
	"github.com/oss-sync/store"

	"golang.org/x/time/rate"
)

// SyncTask represents a single file to be synced from source to destination.
type SyncTask struct {
	Key  string
	Size int64
}

// Worker holds the dependencies needed to execute a single sync task.
type Worker struct {
	src     store.Source
	dst     store.Destination
	database *db.DB
	limiter  *rate.Limiter
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
	src store.Source,
	dst store.Destination,
	database *db.DB,
	limiter *rate.Limiter,
) *WorkerPool {
	ctx, cancel := context.WithCancel(ctx)
	pool := &WorkerPool{
		worker: Worker{
			src:      src,
			dst:      dst,
			database: database,
			limiter:  limiter,
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
			log.Printf("[ERROR] sync %s: %v", task.Key, err)
		}
	}
}

// process downloads from source and uploads to destination for a single task.
func (w *Worker) process(ctx context.Context, task SyncTask) error {
	stream, err := w.src.GetObjectStream(task.Key)
	if err != nil {
		_ = w.database.MarkFailed(task.Key, err.Error())
		return fmt.Errorf("download: %w", err)
	}
	defer stream.Close()

	limited := WrapReader(ctx, stream, w.limiter)

	if err := w.dst.PutObjectFromStream(task.Key, limited, task.Size); err != nil {
		_ = w.database.MarkFailed(task.Key, err.Error())
		return fmt.Errorf("upload: %w", err)
	}

	if err := w.database.MarkSynced(task.Key); err != nil {
		return fmt.Errorf("mark synced: %w", err)
	}

	log.Printf("[OK] synced %s (%d bytes)", task.Key, task.Size)
	return nil
}

