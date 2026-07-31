package store

import (
	"context"
	"testing"

	"github.com/stayradiated/mosscap/internal/doctor"
)

func TestDoctorRollbackProbeLeavesNoDurableRowsOrObjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "doctor-create", "current task"); err != nil {
		t.Fatal(err)
	}
	before, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checks := s.Doctor(ctx)
	for _, check := range checks {
		if check.Status != doctor.Pass {
			t.Fatalf("%s = %s: %s", check.Name, check.Status, check.Message)
		}
	}
	after, err := s.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("counts after doctor = %+v, want %+v", after, before)
	}
	var durable int
	if err := s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE name='mosscap_doctor_probe'").Scan(&durable); err != nil {
		t.Fatal(err)
	}
	if durable != 0 {
		t.Fatalf("doctor probe was durable: %d objects", durable)
	}
}
