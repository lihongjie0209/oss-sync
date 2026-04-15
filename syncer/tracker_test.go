package syncer

import "testing"

func TestTrackerRemainsVisibleAfterDiscoveryDone(t *testing.T) {
	const scope = "scope-a"
	t.Cleanup(func() { UnregisterTracker(scope) })

	tracker := RegisterTracker(scope)
	tracker.AddDiscovered(3)
	tracker.MarkDiscoveryDone()

	stats := GetLiveStatsForScopes([]string{scope})
	if stats.FilesDiscovered != 3 {
		t.Fatalf("expected discovered count to remain visible, got %+v", stats)
	}
	if !stats.DiscoveryDone {
		t.Fatalf("expected discovery done to remain visible, got %+v", stats)
	}
}
