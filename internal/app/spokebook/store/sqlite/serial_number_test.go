package sqlite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

func TestSerialNumbers(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	b, err := s.Bikes().CreateBike(ctx, store.Bike{
		ID: newID(), OwnerID: "o1", Name: "Gravel", SerialNumber: "WTU123", CreatedAt: time.Now().UTC(),
	})
	if err != nil || b.SerialNumber != "WTU123" {
		t.Fatalf("create bike: %q, %v", b.SerialNumber, err)
	}
	c, err := s.Components().CreateComponent(ctx, store.Component{
		ID: newID(), OwnerID: "o1", Type: "fork", PartNumber: "P1", SerialNumber: "SN1", CreatedAt: time.Now().UTC(),
	})
	if err != nil || c.SerialNumber != "SN1" || c.PartNumber != "P1" {
		t.Fatalf("create component: %+v, %v", c, err)
	}

	for _, sn := range []string{"WTU456", ""} {
		b.SerialNumber = sn
		if err := s.Bikes().UpdateBike(ctx, b); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Bikes().GetBike(ctx, "o1", b.ID); err != nil || got.SerialNumber != sn {
			t.Errorf("bike: got %q, %v; want %q", got.SerialNumber, err, sn)
		}
		c.SerialNumber = sn
		if err := s.Components().UpdateComponent(ctx, c); err != nil {
			t.Fatal(err)
		}
		if got, err := s.Components().GetComponent(ctx, "o1", c.ID); err != nil || got.SerialNumber != sn || got.PartNumber != "P1" {
			t.Errorf("component: got %+v, %v; want %q", got, err, sn)
		}
	}
	if list, err := s.Components().ListComponents(ctx, "o1"); err != nil || len(list) != 1 || list[0].SerialNumber != "" {
		t.Errorf("list: %+v, %v", list, err)
	}

	// Another owner can neither change nor see them.
	b.SerialNumber, b.OwnerID = "HACK", "o2"
	if err := s.Bikes().UpdateBike(ctx, b); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update bike of other owner: %v", err)
	}
	c.SerialNumber, c.OwnerID = "HACK", "o2"
	if err := s.Components().UpdateComponent(ctx, c); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update component of other owner: %v", err)
	}
	if _, err := s.Components().GetComponent(ctx, "o2", c.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get component of other owner: %v", err)
	}
	if got, _ := s.Bikes().GetBike(ctx, "o1", b.ID); got.SerialNumber != "" {
		t.Errorf("bike changed by other owner: %q", got.SerialNumber)
	}
	if got, _ := s.Components().GetComponent(ctx, "o1", c.ID); got.SerialNumber != "" {
		t.Errorf("component changed by other owner: %q", got.SerialNumber)
	}
}
