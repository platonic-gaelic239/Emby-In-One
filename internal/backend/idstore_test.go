package backend

import (
	"path/filepath"
	"testing"
)

func TestIDStorePersistsAdditionalInstances(t *testing.T) {
	dir := t.TempDir()
	logger := NewLogger(LogConfig{Level: "error", FileLevel: "error", DataDir: dir})
	t.Cleanup(func() { _ = logger.Close() })

	store1, err := NewIDStore(dir, logger)
	if err != nil {
		t.Fatalf("create store1: %v", err)
	}
	virtualID := store1.GetOrCreateVirtualID("series-a", 0)
	store1.AssociateAdditionalInstance(virtualID, "series-b", 1)
	_ = store1.Close()

	store2, err := NewIDStore(dir, logger)
	if err != nil {
		t.Fatalf("create store2: %v", err)
	}
	t.Cleanup(func() { _ = store2.Close() })

	resolved := store2.ResolveVirtualID(virtualID)
	if resolved == nil || len(resolved.OtherInstances) != 1 {
		t.Fatalf("resolved = %#v, want one additional instance", resolved)
	}
	if resolved.OtherInstances[0].OriginalID != "series-b" || resolved.OtherInstances[0].ServerIndex != 1 {
		t.Fatalf("additional instance = %#v", resolved.OtherInstances[0])
	}

	if _, err := filepath.Abs(filepath.Join(dir, "mappings.db")); err != nil {
		t.Fatalf("db path err: %v", err)
	}
}

func TestIDStoreUpdatesAdditionalInstancesOnShiftAndDelete(t *testing.T) {
	dir := t.TempDir()
	logger := NewLogger(LogConfig{Level: "error", FileLevel: "error", DataDir: dir})
	t.Cleanup(func() { _ = logger.Close() })

	store, err := NewIDStore(dir, logger)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	virtualID := store.GetOrCreateVirtualID("series-a", 0)
	store.AssociateAdditionalInstance(virtualID, "series-c", 2)
	store.ShiftServerIndices(1)

	resolved := store.ResolveVirtualID(virtualID)
	if resolved.OtherInstances[0].ServerIndex != 1 {
		t.Fatalf("shifted server index = %d, want 1", resolved.OtherInstances[0].ServerIndex)
	}

	store.RemoveByServerIndex(1)
	resolved = store.ResolveVirtualID(virtualID)
	if len(resolved.OtherInstances) != 0 {
		t.Fatalf("other instances = %#v, want empty", resolved.OtherInstances)
	}
}
