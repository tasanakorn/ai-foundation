package sqlite

import (
	"path/filepath"
	"sync"
	"testing"
)

// Several gateway processes may start at once on the same database file.
func TestConcurrentOpenMigratesOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			s, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			s.Close()
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n, max int
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_migrations`).Scan(&n, &max); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) || max != len(migrations) {
		t.Fatalf("schema_migrations has %d rows, max %d; want %d", n, max, len(migrations))
	}
}
