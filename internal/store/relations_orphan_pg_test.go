package store

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// TestPutResourceRelationIfOrphan pins the adoption primitive behind the board
// channel repair path: an empty resource is claimed exactly once, and a
// resource with any relation is never taken over.
func TestPutResourceRelationIfOrphan(t *testing.T) {
	dsn := os.Getenv("KEEL_TEST_DSN")
	if dsn == "" {
		t.Skip("KEEL_TEST_DSN not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, Config{DSN: dsn})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	tenant := uuid.New()
	if err := st.CreateTenant(ctx, tenant, "orphan-"+tenant.String()[:8]); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	accounts := []uuid.UUID{uuid.New(), uuid.New()}
	for _, id := range accounts {
		if _, err := st.CreateAccount(ctx, AccountRecord{
			TenantID: tenant, ID: id, Username: "orphan-" + id.String()[:8],
			DisplayName: "Orphan", Kind: "human",
		}); err != nil {
			t.Fatalf("account: %v", err)
		}
	}
	relation := func(account uuid.UUID) ResourceRelation {
		return ResourceRelation{
			TenantID: tenant, ResourceType: "board_channel", ResourceID: "support",
			Relation: RelationOwner, SubjectType: RelationSubjectAccount,
			SubjectID: account.String(), CreatedBy: account,
		}
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		wins  int
		loser bool
	)
	for _, account := range accounts {
		wg.Add(1)
		go func(account uuid.UUID) {
			defer wg.Done()
			created, err := st.PutResourceRelationIfOrphan(ctx, relation(account))
			if err != nil {
				t.Errorf("adopt: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if created {
				wins++
			} else {
				loser = true
			}
		}(account)
	}
	wg.Wait()
	if wins != 1 || !loser {
		t.Fatalf("concurrent adoption wins=%d loser=%v", wins, loser)
	}
	relations, err := st.ListResourceRelations(ctx, tenant, "board_channel", "support", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(relations) != 1 || relations[0].Relation != RelationOwner {
		t.Fatalf("relations = %+v", relations)
	}
	// A second adoption never changes an existing ACL.
	created, err := st.PutResourceRelationIfOrphan(ctx, relation(accounts[0]))
	if err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	if created {
		t.Fatal("a populated resource was adopted again")
	}
}
