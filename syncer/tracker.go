package syncer

import (
	"sync"
	"sync/atomic"
)

type TransferTracker struct {
	bytesTransferred atomic.Int64
	filesCompleted   atomic.Int64
	filesDiscovered  atomic.Int64
	discoveryDone    atomic.Bool
}

type LiveTransferStats struct {
	BytesTransferred int64
	FilesCompleted   int64
	FilesDiscovered  int64
	DiscoveryDone    bool
}

var trackerRegistry sync.Map

func RegisterTracker(scope string) *TransferTracker {
	tracker := &TransferTracker{}
	trackerRegistry.Store(scope, tracker)
	return tracker
}

func UnregisterTracker(scope string) {
	trackerRegistry.Delete(scope)
}

func GetLiveStatsForScopes(scopes []string) LiveTransferStats {
	var stats LiveTransferStats
	for _, scope := range scopes {
		value, ok := trackerRegistry.Load(scope)
		if !ok {
			continue
		}
		tracker := value.(*TransferTracker)
		stats.BytesTransferred += tracker.bytesTransferred.Load()
		stats.FilesCompleted += tracker.filesCompleted.Load()
		stats.FilesDiscovered += tracker.filesDiscovered.Load()
		stats.DiscoveryDone = stats.DiscoveryDone || tracker.discoveryDone.Load()
	}
	return stats
}

func (t *TransferTracker) AddBytes(n int) {
	if t == nil || n <= 0 {
		return
	}
	t.bytesTransferred.Add(int64(n))
}

func (t *TransferTracker) MarkFileCompleted() {
	if t == nil {
		return
	}
	t.filesCompleted.Add(1)
}

func (t *TransferTracker) AddDiscovered(n int) {
	if t == nil || n <= 0 {
		return
	}
	t.filesDiscovered.Add(int64(n))
}

func (t *TransferTracker) MarkDiscoveryDone() {
	if t == nil {
		return
	}
	t.discoveryDone.Store(true)
}
