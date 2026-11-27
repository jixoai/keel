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
			created, err := st.PutResourceRelationIfOrphan(ctx, relation(account), nil)
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
	created, err := st.PutResourceRelationIfOrphan(ctx, relation(accounts[0]), nil)
	if err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	if created {
		t.Fatal("a populated resource was adopted again")
	}

	// An inherited relation is an existing ACL: the descendant is not an
	// orphan, so a tenant manager cannot bypass the ancestor.
	if _, err := st.PutResourceRelation(ctx, ResourceRelation{
		TenantID: tenant, ResourceType: "board_channel", ResourceID: "work",
		Relation: RelationEditor, SubjectType: RelationSubjectAccount,
		SubjectID: accounts[0].String(), CreatedBy: accounts[0],
	}); err != nil {
		t.Fatalf("ancestor relation: %v", err)
	}
	descendant := relation(accounts[1])
	descendant.ResourceID = "work/production"
	created, err = st.PutResourceRelationIfOrphan(ctx, descendant, []string{"work"})
	if err != nil {
		t.Fatalf("descendant adopt: %v", err)
	}
	if created {
		t.Fatal("an inherited ACL did not block adoption")
	}
	if relations, err := st.ListResourceRelations(ctx, tenant, "board_channel", "work/production", 0); err != nil || len(relations) != 0 {
		t.Fatalf("descendant relations = %+v/%v", relations, err)
	}

	// A descendant's exact relation blocks adopting its parent: the child is
	// already authorized, and parent ownership would inherit over it.
	if err := st.DeleteResourceRelation(ctx, tenant, "board_channel", "work",
		RelationEditor, RelationSubjectAccount, accounts[0].String()); err != nil {
		t.Fatalf("delete ancestor relation: %v", err)
	}
	if _, err := st.PutResourceRelation(ctx, ResourceRelation{
		TenantID: tenant, ResourceType: "board_channel", ResourceID: "work/production",
		Relation: RelationViewer, SubjectType: RelationSubjectAccount,
		SubjectID: accounts[1].String(), CreatedBy: accounts[1],
	}); err != nil {
		t.Fatalf("descendant relation: %v", err)
	}
	parent := relation(accounts[0])
	parent.ResourceID = "work"
	created, err = st.PutResourceRelationIfOrphan(ctx, parent, nil)
	if err != nil {
		t.Fatalf("parent adopt: %v", err)
	}
	if created {
		t.Fatal("a descendant ACL did not block parent adoption")
	}
	if relations, err := st.ListResourceRelations(ctx, tenant, "board_channel", "work", 0); err != nil || len(relations) != 0 {
		t.Fatalf("work relations = %+v/%v", relations, err)
	}
	if err := st.DeleteResourceRelation(ctx, tenant, "board_channel", "work/production",
		RelationViewer, RelationSubjectAccount, accounts[1].String()); err != nil {
		t.Fatalf("delete descendant relation: %v", err)
	}

	// A fully relation-free leaf is still adoptable, and that relation then
	// blocks adopting its parent.
	leaf := relation(accounts[1])
	leaf.ResourceID = "work/leaf"
	created, err = st.PutResourceRelationIfOrphan(ctx, leaf, []string{"work"})
	if err != nil {
		t.Fatalf("leaf adopt: %v", err)
	}
	if !created {
		t.Fatal("a fully relation-free leaf was not adopted")
	}
	created, err = st.PutResourceRelationIfOrphan(ctx, parent, nil)
	if err != nil {
		t.Fatalf("parent adopt after leaf: %v", err)
	}
	if created {
		t.Fatal("the adopted leaf did not block parent adoption")
	}
}
