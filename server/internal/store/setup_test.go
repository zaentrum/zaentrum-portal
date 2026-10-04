package store

import (
	"context"
	"testing"

	"github.com/zaentrum/zaentrum-portal/server/db"
)

// Setup is marked done once — the first admin to mark it is the one the
// record names, whoever marks it again — and reopened by removing the record.
func TestSetupCompletion(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ { // every boot re-applies every migration
		if err := st.Migrate(ctx, db.Migrations, "zaentrum-admin"); err != nil {
			t.Fatal(err)
		}
	}
	if c, err := st.SetupCompletion(ctx); err != nil || c != nil {
		t.Fatalf("a fresh instance: %+v, %v", c, err)
	}
	first, err := st.CompleteSetup(ctx, "admin")
	if err != nil || first.By != "admin" || first.At.IsZero() {
		t.Fatalf("complete = %+v, %v", first, err)
	}
	again, err := st.CompleteSetup(ctx, "other")
	if err != nil || again != first {
		t.Errorf("completed again = %+v, %v — want the first record, %+v", again, err, first)
	}
	if c, err := st.SetupCompletion(ctx); err != nil || c == nil || *c != first {
		t.Errorf("read = %+v, %v", c, err)
	}
	// A boot keeps it.
	if err := st.Migrate(ctx, db.Migrations, "zaentrum-admin"); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.SetupCompletion(ctx); c == nil {
		t.Error("a boot dropped the record")
	}
	for i := 0; i < 2; i++ { // reopening what is open changes nothing
		if err := st.ReopenSetup(ctx); err != nil {
			t.Fatal(err)
		}
		if c, err := st.SetupCompletion(ctx); err != nil || c != nil {
			t.Fatalf("reopened: %+v, %v", c, err)
		}
	}
	if c, err := st.CompleteSetup(ctx, "other"); err != nil || c.By != "other" {
		t.Errorf("completed after a reopen = %+v, %v", c, err)
	}
	// One row, whatever is tried.
	if _, err := st.pool.Exec(ctx, `INSERT INTO setup_completion (id) VALUES (false)`); err == nil {
		t.Error("a second row was taken")
	}
}
