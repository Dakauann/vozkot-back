package checkout

import (
	"context"
	"errors"
	"sync"
	"testing"

	orderdomain "vozkot/domain/order"
	seatingdomain "vozkot/domain/seating"
	"vozkot/infra/database/schema"
	seatingRepository "vozkot/infra/repositories/seating"
	"vozkot/infra/testsupport"
	seatingUsecase "vozkot/usecases/seating"
)

func areaFixture(t *testing.T, h *seatedHarness, name string) (string, string) {
	t.Helper()
	area := schema.LayoutSection{ID: testsupport.Unique("sec"), LayoutID: h.layoutID, Name: name, Kind: "booth", Capacity: 10, Width: 180, Height: 116}
	if err := h.db.Create(&area).Error; err != nil {
		t.Fatal(err)
	}
	ticket := seedSecondTier(t, h.harness, 60000)
	if err := h.db.Exec("UPDATE tickets SET quantity = 10 WHERE id = ?", ticket).Error; err != nil {
		t.Fatal(err)
	}
	return area.ID, ticket
}

func TestAreaChoiceReachesMapOrderAndIndependentInventory(t *testing.T) {
	h := newSeatedHarness(t, 2, 4)
	ctx := context.Background()
	leftArea, left := areaFixture(t, h, "Camarote esquerdo")
	rightArea, right := areaFixture(t, h, "Camarote direito")
	repo := seatingRepository.NewSeatRepository(h.db)
	binding := map[string]string{leftArea: left, rightArea: right}
	if err := repo.BindAreas(ctx, h.eventID, binding); err != nil {
		t.Fatal(err)
	}
	service := seatingUsecase.NewService(seatingRepository.NewLayoutRepository(h.db), repo, nil, testsupport.Unique)
	view, err := service.Map(ctx, h.eventID, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, marker := range view.Markers {
		if marker.ID == rightArea && marker.TicketID == right {
			found = true
		}
	}
	if !found {
		t.Fatal("map did not identify right-side ticket inventory")
	}
	placed, err := h.service.Start(ctx, StartInput{Items: []orderdomain.DraftItem{{TicketID: right, Quantity: 2}}, BuyerID: h.ownerID, IdempotencyKey: testsupport.Unique("right")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.db.Exec("DELETE FROM jobs WHERE payload->>'orderId' = ?", placed.ID)
		h.db.Exec("DELETE FROM orders WHERE id = ?", placed.ID)
	})
	if placed.Items[0].TicketTitle != "Camarote direito" {
		t.Fatalf("receipt lost chosen area: %+v", placed.Items[0])
	}
	leftStock, _ := h.tickets.GetByID(ctx, left)
	rightStock, _ := h.tickets.GetByID(ctx, right)
	if leftStock.Reserved != 0 || rightStock.Reserved != 2 {
		t.Fatal("area inventories were mixed")
	}
	// Existing purchases must never acquire a different side through configuration.
	if err := repo.BindAreas(ctx, h.eventID, map[string]string{leftArea: right, rightArea: left}); !errors.Is(err, seatingdomain.ErrSeatsSold) {
		t.Fatalf("reassignment error = %v", err)
	}
}

func TestAreaBindingRejectsSharedInventoryAndOversizedTiers(t *testing.T) {
	h := newSeatedHarness(t, 2, 4)
	leftArea, left := areaFixture(t, h, "Left")
	rightArea, _ := areaFixture(t, h, "Right")
	repo := seatingRepository.NewSeatRepository(h.db)
	ctx := context.Background()
	if err := repo.BindAreas(ctx, h.eventID, map[string]string{leftArea: left, rightArea: left}); !errors.Is(err, seatingdomain.ErrInvalidTicket) {
		t.Fatalf("shared tier error = %v", err)
	}
	if err := h.db.Exec("UPDATE tickets SET quantity = 11 WHERE id = ?", left).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.BindAreas(ctx, h.eventID, map[string]string{leftArea: left}); !errors.Is(err, seatingdomain.ErrInvalidTicket) {
		t.Fatalf("capacity error = %v", err)
	}
	if err := repo.BindAreas(ctx, h.eventID, map[string]string{leftArea: h.ticketID}); !errors.Is(err, seatingdomain.ErrInvalidTicket) {
		t.Fatalf("seated tier error = %v", err)
	}
}

func TestAreaCapacityCannotBeOversoldAfterTierQuantityChanges(t *testing.T) {
	h := newSeatedHarness(t, 2, 4)
	area, ticket := areaFixture(t, h, "Right")
	ctx := context.Background()
	if err := seatingRepository.NewSeatRepository(h.db).BindAreas(ctx, h.eventID, map[string]string{area: ticket}); err != nil {
		t.Fatal(err)
	}
	if err := h.db.Exec("UPDATE tickets SET quantity = 100 WHERE id = ?", ticket).Error; err != nil {
		t.Fatal(err)
	}
	// The atomic reservation enforces the physical area's ten places even if an
	// operator later increases the tier's generic quantity.
	var wg sync.WaitGroup
	result := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := h.tickets.Reserve(ctx, ticket, 7)
			if err != nil {
				t.Error(err)
			}
			result <- ok
		}()
	}
	wg.Wait()
	close(result)
	winners := 0
	for ok := range result {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
	stock, err := h.tickets.GetByID(ctx, ticket)
	if err != nil || stock.Reserved != 7 {
		t.Fatalf("stock = %+v, %v", stock, err)
	}
}
