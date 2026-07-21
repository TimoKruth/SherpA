package collector

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLedgerRoundTripContainsOnlyApprovedMetadata(t *testing.T) {
	ledger := openTestLedger(t, defaultLedgerOps())
	now := time.Now().UTC().Truncate(time.Second)
	stored := now.Add(time.Minute)
	attempted := now.Add(30 * time.Second)
	record := ObjectRecord{
		ObjectID: "sha256:" + testDigestHex, ArchiveName: "sherpa-" + testDigestHex,
		CompressedSize: 123, EncryptedSize: 456, ReceivedAt: now, StoredAt: &stored,
		LastAttemptAt: &attempted, LatestRetryClass: "temporary", RetryCount: 2,
	}
	if err := ledger.Put(record); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, found, err := ledger.Get(record.ObjectID)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if !reflect.DeepEqual(got, record) {
		t.Fatalf("record = %#v, want %#v", got, record)
	}

	raw, err := os.ReadFile(filepath.Join(ledger.path, testDigestHex+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	wantFields := []string{"archive_name", "compressed_size", "encrypted_size", "last_attempt_at", "latest_retry_class", "object_id", "received_at", "retry_count", "stored_at"}
	gotFields := make([]string, 0, len(fields))
	for field := range fields {
		gotFields = append(gotFields, field)
	}
	sortStrings(gotFields)
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("JSON fields = %v", gotFields)
	}
	for _, forbidden := range []string{"source_ip", "token", "repository", "storage", "database", "member", "request", ledger.path} {
		if bytes.Contains(bytes.ToLower(raw), []byte(strings.ToLower(forbidden))) {
			t.Fatalf("record contains forbidden value %q", forbidden)
		}
	}
}

func TestLedgerPutUsesMode0600AtomicReplacement(t *testing.T) {
	var events []string
	ops := defaultLedgerOps()
	ops.fsync = func(fd int) error { events = append(events, "sync"); return unix.Fsync(fd) }
	ops.rename = func(oldFD int, oldName string, newFD int, newName string) error {
		events = append(events, "rename")
		return unix.Renameat(oldFD, oldName, newFD, newName)
	}
	ledger := openTestLedger(t, ops)
	record := validTestRecord(time.Now().UTC())
	oldUmask := unix.Umask(0o777)
	defer unix.Umask(oldUmask)
	if err := ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	if want := []string{"sync", "rename", "sync"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v", events)
	}
	var stat unix.Stat_t
	path := filepath.Join(ledger.path, testDigestHex+".json")
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	if got := stat.Mode & 0o7777; got != 0o600 {
		t.Fatalf("raw mode = %04o", got)
	}

	record.RetryCount = 3
	if err := ledger.Put(record); err != nil {
		t.Fatal(err)
	}
	got, found, err := ledger.Get(record.ObjectID)
	if err != nil || !found || got.RetryCount != 3 {
		t.Fatalf("replacement = %#v found=%v err=%v", got, found, err)
	}
	entries, _ := os.ReadDir(ledger.path)
	if len(entries) != 1 || entries[0].Name() != testDigestHex+".json" {
		t.Fatalf("temp residue = %v", entryNames(entries))
	}
}

func TestLedgerListSortsByReceiptTime(t *testing.T) {
	ledger := openTestLedger(t, defaultLedgerOps())
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	ids := []string{strings.Repeat("b", 64), strings.Repeat("a", 64), strings.Repeat("c", 64)}
	times := []time.Time{base, base, base.Add(time.Minute)}
	for i := range ids {
		record := validTestRecord(times[i])
		record.ObjectID = "sha256:" + ids[i]
		record.ArchiveName = "sherpa-" + ids[i]
		if err := ledger.Put(record); err != nil {
			t.Fatal(err)
		}
	}
	records, err := ledger.List()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(records))
	for i := range records {
		got[i] = records[i].ObjectID
	}
	want := []string{"sha256:" + strings.Repeat("a", 64), "sha256:" + strings.Repeat("b", 64), "sha256:" + strings.Repeat("c", 64)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v", got)
	}
}

func TestLedgerRejectsInvalidObjectIDFilenameMismatch(t *testing.T) {
	ledger := openTestLedger(t, defaultLedgerOps())
	record := validTestRecord(time.Now().UTC())
	for _, mutate := range []func(*ObjectRecord){
		func(r *ObjectRecord) { r.ObjectID = "sha256:" + strings.ToUpper(testDigestHex) },
		func(r *ObjectRecord) { r.ObjectID = "../private" },
		func(r *ObjectRecord) { r.ArchiveName = "sherpa-" + strings.Repeat("a", 64) },
		func(r *ObjectRecord) { r.CompressedSize = -1 },
		func(r *ObjectRecord) { r.EncryptedSize = -1 },
		func(r *ObjectRecord) { r.RetryCount = -1 },
		func(r *ObjectRecord) { r.LatestRetryClass = "../../private-token" },
		func(r *ObjectRecord) { r.ReceivedAt = time.Time{} },
	} {
		copy := record
		mutate(&copy)
		if err := ledger.Put(copy); err == nil {
			t.Fatalf("invalid record accepted: %#v", copy)
		}
	}
	if _, _, err := ledger.Get("../" + testDigestHex); err == nil {
		t.Fatal("traversal object ID accepted")
	}

	mismatch := validTestRecord(time.Now().UTC())
	mismatch.ObjectID = "sha256:" + strings.Repeat("a", 64)
	raw, _ := json.Marshal(mismatch)
	if err := os.WriteFile(filepath.Join(ledger.path, testDigestHex+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.List(); err == nil {
		t.Fatal("filename/object mismatch accepted")
	}
}

func TestLedgerFailureNeverExposesPrivateValues(t *testing.T) {
	ledger := openTestLedger(t, defaultLedgerOps())
	privateObject := "sha256:" + strings.Repeat("d", 64)
	path := filepath.Join(ledger.path, strings.Repeat("d", 64)+".json")
	if err := os.WriteFile(path, []byte(`{"object_id":"private-record-canary","unexpected":"secret-token-canary"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := ledger.Get(privateObject)
	if err == nil {
		t.Fatal("malformed record accepted")
	}
	for _, value := range []string{ledger.path, privateObject, "private-record-canary", "secret-token-canary", "unexpected"} {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("error exposed %q: %v", value, err)
		}
	}
}

func TestLedgerStrictJSONBoundsAndStaticEntries(t *testing.T) {
	ledger := openTestLedger(t, defaultLedgerOps())
	valid := validTestRecord(time.Now().UTC())
	raw, _ := json.Marshal(valid)
	canonical := filepath.Join(ledger.path, testDigestHex+".json")
	if err := os.WriteFile(canonical, append(raw, []byte(" {}")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Get(valid.ObjectID); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	if err := os.WriteFile(canonical, bytes.Repeat([]byte(" "), ledgerRecordMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Get(valid.ObjectID); err == nil {
		t.Fatal("oversized JSON accepted")
	}
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(newPrivateDir(t), "outside")
	if err := os.WriteFile(outside, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, canonical); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ledger.path, "notes.txt"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(ledger.path, strings.Repeat("a", 64)+".json"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := ledger.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("static entries discovered: %#v", records)
	}
}

func TestLedgerPostRenameSyncFailureIsTerminal(t *testing.T) {
	ops := defaultLedgerOps()
	calls := 0
	ops.fsync = func(fd int) error {
		calls++
		if calls == 2 {
			return errors.New("raw-private-sync")
		}
		return unix.Fsync(fd)
	}
	ledger := openTestLedger(t, ops)
	record := validTestRecord(time.Now().UTC())
	err := ledger.Put(record)
	if err == nil || err.Error() != "collector ledger synchronization failed" || strings.Contains(err.Error(), "raw-private") {
		t.Fatalf("error = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(ledger.path, testDigestHex+".json")); statErr != nil {
		t.Fatalf("replacement rolled back: %v", statErr)
	}
}

func TestLedgerRejectsUnsafeRoot(t *testing.T) {
	parent := newPrivateDir(t)
	outside := newPrivateDir(t)
	link := filepath.Join(parent, "state")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(link); err == nil || err.Error() != "collector ledger directory unsafe" {
		t.Fatalf("error = %v", err)
	}
}

func openTestLedger(t testing.TB, ops ledgerOps) *Ledger {
	t.Helper()
	ledger, err := openLedger(newPrivateDir(t), ops)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ledger.Close() })
	return ledger
}
func validTestRecord(received time.Time) ObjectRecord {
	return ObjectRecord{ObjectID: "sha256:" + testDigestHex, ArchiveName: "sherpa-" + testDigestHex, CompressedSize: 1, EncryptedSize: 2, ReceivedAt: received, RetryCount: 0}
}
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
