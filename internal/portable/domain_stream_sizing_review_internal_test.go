package portable

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	objectmemory "github.com/applyinnovations/endlessfs/internal/objectstore/memory"
	"github.com/applyinnovations/endlessfs/internal/storageformat"
)

func reviewStreamEntries(count, width int) []storageformat.DomainEntry {
	entries := make([]storageformat.DomainEntry, count)
	for index := range entries {
		entry := storageformat.DomainEntry{Key: fmt.Sprintf("item/%06d-\"\\\n<测试", index), LogicalVersion: "version-\"\\\n<&>测试"}
		switch index % 6 {
		case 0:
			entry.Value = nil
		case 1:
			entry.Value = []byte{}
		default:
			entry.Value = bytes.Repeat([]byte{byte(index)}, width+index%3)
		}
		entries[index] = entry
	}
	return entries
}

func TestConsistencyDomainStreamSizingReviewOwnsAdmittedValues(t *testing.T) {
	for _, packed := range []bool{false, true} {
		t.Run(fmt.Sprintf("packed-%t", packed), func(t *testing.T) {
			reviewStreamOwnsAdmittedValues(t, packed)
		})
	}
}

func reviewStreamOwnsAdmittedValues(t *testing.T, packed bool) {
	t.Helper()
	ctx := context.Background()
	base := objectmemory.New()
	reference := consistencyDomainRef{Kind: storageformat.DomainOwnerControl, ID: "stream-ownership-review"}
	store := newConsistencyDomainStore(base, nil)
	session := newConsistencyDomainTreeSession(store, reference)
	if packed {
		session.enablePackedWrites("ownership-source")
		if err := session.bindPackedMutation("ownership-mutation"); err != nil {
			t.Fatal(err)
		}
	}
	builder := newConsistencyDomainTreeBuilder(ctx, session)
	entries := reviewStreamEntries(300, 17)
	want := make([]storageformat.DomainEntry, len(entries))
	for index, entry := range entries {
		want[index] = entry
		want[index].Value = bytes.Clone(entry.Value)
		if err := builder.Add(entry); err != nil {
			t.Fatal(err)
		}
		// Mutation after admission must not alter a page whose byte size has
		// already been memoized, including a page retained for packed writes.
		for offset := range entry.Value {
			entry.Value[offset] ^= 0xff
		}
	}
	root, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if err := session.flushPack(ctx); err != nil {
		t.Fatal(err)
	}
	iterator, err := newConsistencyDomainTreeIterator(ctx, newConsistencyDomainTreeSession(store, reference), root)
	if err != nil {
		t.Fatal(err)
	}
	for index, expected := range want {
		entry, found, err := iterator.Next()
		if err != nil || !found || entry.Key != expected.Key || !bytes.Equal(entry.Value, expected.Value) {
			t.Fatalf("caller mutation escaped into entry %d: found=%t err=%v", index, found, err)
		}
	}
}
