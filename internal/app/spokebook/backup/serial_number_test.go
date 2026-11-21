package backup_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spokebook/spokebook/internal/app/spokebook/backup"
)

func TestSerialNumbersExported(t *testing.T) {
	ctx := context.Background()
	src := open(t, "alice")
	fill(t, src, "alice")
	f := export(t, src, "alice")
	got := map[string]string{}
	for _, b := range f.Bikes {
		got[b.ID] = b.SerialNumber
	}
	for _, c := range f.Components {
		got[c.ID] = c.SerialNumber
	}
	for id, want := range map[string]string{"b1": "WTU123", "b2": "", "c1": "SN-1", "c2": ""} {
		if got[id] != want {
			t.Errorf("%s: %q, want %q", id, got[id], want)
		}
	}

	var buf bytes.Buffer
	must(t, backup.Write(ctx, src, "alice", &buf))
	r, err := backup.Read(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	must(t, err)
	dst := open(t, "bob")
	must(t, backup.Import(ctx, dst, "bob", r))
	b, err := dst.Bikes().GetBike(ctx, "bob", "b1")
	must(t, err)
	c, err := dst.Components().GetComponent(ctx, "bob", "c1")
	must(t, err)
	if b.SerialNumber != "WTU123" || c.SerialNumber != "SN-1" {
		t.Errorf("imported bike %q, component %q", b.SerialNumber, c.SerialNumber)
	}
}

// A backup made before serial numbers existed imports with empty ones.
func TestImportWithoutSerialNumbers(t *testing.T) {
	ctx := context.Background()
	const old = `{"format":"spokebook-backup","version":2,
		"bikes":[{"id":"b","name":"Gravel","created_at":"2025-01-01T00:00:00Z"}],
		"components":[{"id":"c","type":"chain","part_number":"P1","created_at":"2025-01-01T00:00:00Z"}]}`
	f, err := backup.Read(strings.NewReader(old), int64(len(old)))
	must(t, err)
	st := open(t, "o")
	must(t, backup.Import(ctx, st, "o", f))
	b, err := st.Bikes().GetBike(ctx, "o", "b")
	must(t, err)
	c, err := st.Components().GetComponent(ctx, "o", "c")
	must(t, err)
	if b.SerialNumber != "" || c.SerialNumber != "" || c.PartNumber != "P1" {
		t.Errorf("bike %q, component %q %q", b.SerialNumber, c.SerialNumber, c.PartNumber)
	}
}
