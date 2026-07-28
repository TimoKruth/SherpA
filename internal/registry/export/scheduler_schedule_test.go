package export

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recordedWaits captures the delays the scheduler asks to sleep for, so the
// schedule can be asserted without real time passing.
type recordedWaits struct {
	mu     sync.Mutex
	delays []time.Duration
	cancel context.CancelFunc
	stopAt int
}

func (r *recordedWaits) wait(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	r.delays = append(r.delays, d)
	count := len(r.delays)
	r.mu.Unlock()
	if count >= r.stopAt {
		r.cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (r *recordedWaits) first() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.delays) == 0 {
		return -1
	}
	return r.delays[0]
}

func scheduleTestOps(t *testing.T, waits *recordedWaits, now time.Time, cycles *int) schedulerOps {
	t.Helper()
	ops := defaultSchedulerOps()
	ops.now = func() time.Time { return now }
	ops.wait = waits.wait
	// Deterministic jitter: always the midpoint of the window.
	ops.jitter = func(window time.Duration) time.Duration { return window / 2 }
	ops.runExport = func(context.Context, string, string, string) error {
		*cycles++
		return nil
	}
	ops.upload = func(context.Context, *os.File, os.FileInfo, string, string) (UploadResult, error) {
		return validatedUploadResult("stored"), nil
	}
	return ops
}

func queueTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSchedulerRunsPromptlyWhenNoPriorCycleIsRecorded(t *testing.T) {
	dir := queueTestDir(t)
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := &recordedWaits{cancel: cancel, stopAt: 1}
	cycles := 0
	ops := scheduleTestOps(t, waits, now, &cycles)

	scheduler, err := prepareScheduler(schedulerTestConfig(dir, time.Hour), ops)
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()
	_ = scheduler.Run(ctx)

	got := waits.first()
	if got < 0 {
		t.Fatal("scheduler never waited")
	}
	// A fresh queue must not sit idle for a whole interval before its first
	// export; that is the failure that left the collector 40h stale.
	if got >= time.Hour {
		t.Fatalf("first delay = %v, want well under the %v interval", got, time.Hour)
	}
}

func TestSchedulerWaitsOnlyTheRemainderWhenPriorCycleIsRecent(t *testing.T) {
	dir := queueTestDir(t)
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := &recordedWaits{cancel: cancel, stopAt: 1}
	cycles := 0
	ops := scheduleTestOps(t, waits, now, &cycles)

	cfg := schedulerTestConfig(dir, time.Hour)
	scheduler, err := prepareScheduler(cfg, ops)
	if err != nil {
		t.Fatal(err)
	}
	// A cycle ran 10 minutes ago; a restart must not re-export immediately.
	if err := ops.writeLastCycle(scheduler.queue, now.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()
	_ = scheduler.Run(ctx)

	got := waits.first()
	want := 50 * time.Minute
	if got < want-time.Minute || got > want+time.Minute {
		t.Fatalf("first delay = %v, want approximately %v", got, want)
	}
	if cycles != 0 {
		t.Fatalf("cycles = %d, want 0: a restart inside the interval must not re-export", cycles)
	}
}

func TestSchedulerRunsPromptlyWhenPriorCycleIsOverdue(t *testing.T) {
	dir := queueTestDir(t)
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := &recordedWaits{cancel: cancel, stopAt: 1}
	cycles := 0
	ops := scheduleTestOps(t, waits, now, &cycles)

	cfg := schedulerTestConfig(dir, time.Hour)
	scheduler, err := prepareScheduler(cfg, ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := ops.writeLastCycle(scheduler.queue, now.Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()
	_ = scheduler.Run(ctx)

	if got := waits.first(); got >= time.Hour {
		t.Fatalf("first delay = %v, want prompt when the last cycle is overdue", got)
	}
}

func TestSchedulerRecordsEachCycleSoRestartsResumeTheSchedule(t *testing.T) {
	dir := queueTestDir(t)
	now := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Allow one cycle to complete, then stop on the second wait.
	waits := &recordedWaits{cancel: cancel, stopAt: 2}
	cycles := 0
	ops := scheduleTestOps(t, waits, now, &cycles)

	cfg := schedulerTestConfig(dir, time.Hour)
	scheduler, err := prepareScheduler(cfg, ops)
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()
	_ = scheduler.Run(ctx)

	if cycles != 1 {
		t.Fatalf("cycles = %d, want 1", cycles)
	}
	recorded, ok, err := ops.readLastCycle(scheduler.queue)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("no cycle recorded; a restart would repeat the export")
	}
	if !recorded.Equal(now) {
		t.Fatalf("recorded cycle = %v, want %v", recorded, now)
	}
}

func TestCycleMarkerIsNeitherAPendingArchiveNorAStalePartial(t *testing.T) {
	dir := queueTestDir(t)
	now := time.Now().UTC()
	ops := defaultSchedulerOps()

	scheduler, err := prepareScheduler(schedulerTestConfig(dir, time.Hour), ops)
	if err != nil {
		t.Fatal(err)
	}
	defer scheduler.Close()
	if err := ops.writeLastCycle(scheduler.queue, now.Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}

	archives, err := discoverPendingArchivesIn(scheduler.queue)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 0 {
		t.Fatalf("marker discovered as a pending archive: %#v", archives)
	}

	// Deliberately far older than the partial cutoff.
	if _, err := cleanupStaleExportPartialsIn(scheduler.queue, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ops.readLastCycle(scheduler.queue); err != nil || !ok {
		t.Fatalf("marker removed by stale-partial cleanup: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(dir, exportLastCycleMarkerName)); err != nil {
		t.Fatalf("marker missing on disk: %v", err)
	}
}

func TestStartupJitterNeverExceedsTheInterval(t *testing.T) {
	// A fixed jitter window would dominate any interval shorter than itself,
	// delaying exports far beyond their configured schedule.
	for _, interval := range []time.Duration{5 * time.Millisecond, time.Second, time.Minute, time.Hour} {
		dir := queueTestDir(t)
		ops := defaultSchedulerOps()
		var requested time.Duration
		ops.jitter = func(window time.Duration) time.Duration {
			requested = window
			return 0
		}
		scheduler, err := prepareScheduler(schedulerTestConfig(dir, interval), ops)
		if err != nil {
			t.Fatal(err)
		}
		_ = scheduler.nextDelay(time.Time{}, false)
		if requested > interval {
			t.Errorf("interval %v: jitter window %v exceeds the interval", interval, requested)
		}
		if err := scheduler.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
