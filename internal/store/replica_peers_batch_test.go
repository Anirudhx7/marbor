package store_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Anirudhx7/marbor/internal/store"
)

func seedNodes(t *testing.T, s store.Store, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := s.UpsertNode(store.NodeRecord{Name: n, URL: "http://" + n + ":8000", Runtime: "vllm"}); err != nil {
			t.Fatalf("UpsertNode(%s): %v", n, err)
		}
	}
}

func TestSetReplicaPeersBatch_WritesAllMembersInOneCall(t *testing.T) {
	s := openTestDB(t)
	seedNodes(t, s, "a", "b")
	rp := store.ReplicaPeers{Members: []string{"a", "b"}, Head: "a"}
	if err := s.SetReplicaPeersBatch(map[string]store.ReplicaPeers{"a": rp, "b": rp}); err != nil {
		t.Fatalf("SetReplicaPeersBatch: %v", err)
	}
	ov, err := s.NodeOverrides()
	if err != nil {
		t.Fatalf("NodeOverrides: %v", err)
	}
	for _, n := range []string{"a", "b"} {
		got := ov[n].ReplicaPeers
		if got == nil || !reflect.DeepEqual(*got, rp) {
			t.Errorf("%s replica_peers = %+v, want %+v", n, got, rp)
		}
	}
}

func TestSetReplicaPeersBatch_TouchesOnlyReplicaPeers(t *testing.T) {
	s := openTestDB(t)
	seedNodes(t, s, "a", "b")
	vram, gpu, rt := int64(24000), "RTX 4090", "vllm"
	idx, inflight := []int{0, 1}, 7
	if err := s.UpsertNodeOverride("a", &vram, &gpu, &rt, &idx, &inflight, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride: %v", err)
	}
	rp := store.ReplicaPeers{Members: []string{"a", "b"}, Head: "a"}
	if err := s.SetReplicaPeersBatch(map[string]store.ReplicaPeers{"a": rp, "b": rp}); err != nil {
		t.Fatalf("SetReplicaPeersBatch: %v", err)
	}
	ov, _ := s.NodeOverrides()
	a := ov["a"]
	if a.VRAMTotalMB == nil || *a.VRAMTotalMB != vram || a.GPUModel == nil || *a.GPUModel != gpu ||
		a.MaxInFlight == nil || *a.MaxInFlight != inflight || a.GPUIndices == nil || !reflect.DeepEqual(*a.GPUIndices, idx) {
		t.Errorf("other override columns were disturbed: %+v", a)
	}
	if a.ReplicaPeers == nil {
		t.Error("replica_peers not set")
	}
	// A later single-node write still preserves the declaration.
	if err := s.UpsertNodeOverride("b", &vram, nil, nil, nil, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatalf("UpsertNodeOverride: %v", err)
	}
	ov, _ = s.NodeOverrides()
	if ov["b"].ReplicaPeers == nil {
		t.Error("a later override write lost the replica declaration")
	}
}

func TestSetReplicaPeersBatch_MissingMemberWritesNothing(t *testing.T) {
	s := openTestDB(t)
	seedNodes(t, s, "a")
	rp := store.ReplicaPeers{Members: []string{"a", "gone"}, Head: "a"}
	err := s.SetReplicaPeersBatch(map[string]store.ReplicaPeers{"a": rp, "gone": rp})
	if !errors.Is(err, store.ErrNodeNotRegistered) {
		t.Fatalf("err = %v, want ErrNodeNotRegistered", err)
	}
	ov, _ := s.NodeOverrides()
	if len(ov) != 0 {
		t.Errorf("a failed batch must write nothing, found %+v", ov)
	}
}

func TestSetReplicaPeersBatch_SurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "batch.db")
	s1 := openTestDBAt(t, path)
	seedNodes(t, s1, "a", "b")
	rp := store.ReplicaPeers{Members: []string{"a", "b"}, Head: "b"}
	if err := s1.SetReplicaPeersBatch(map[string]store.ReplicaPeers{"a": rp, "b": rp}); err != nil {
		t.Fatal(err)
	}
	s1.Close()
	s2 := openTestDBAt(t, path)
	defer s2.Close()
	ov, _ := s2.NodeOverrides()
	if got := ov["a"].ReplicaPeers; got == nil || got.Head != "b" {
		t.Errorf("after reopen = %+v", got)
	}
}

func TestSetReplicaPeersBatch_EmptyIsNoop(t *testing.T) {
	s := openTestDB(t)
	if err := s.SetReplicaPeersBatch(nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
}

func TestNopStore_SetReplicaPeersBatch(t *testing.T) {
	var s store.Store = store.NopStore{}
	if err := s.SetReplicaPeersBatch(map[string]store.ReplicaPeers{"a": {}}); err != nil {
		t.Fatalf("NopStore: %v", err)
	}
}
