package seating

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	domain "vozkot/domain/seating"
	"vozkot/infra/database/schema"
)

// BindAreas serializes with checkout's shared manifest lock. A sold area's
// meaning cannot be changed, and each area has a dedicated stock counter.
func (r *SeatRepository) BindAreas(ctx context.Context, eventID string, tickets map[string]string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var manifest schema.EventSeating
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("event_id = ?", eventID).First(&manifest).Error; err != nil {
			return err
		}
		current := map[string]domain.AreaBinding{}
		if len(manifest.Areas) > 0 {
			if err := json.Unmarshal(manifest.Areas, &current); err != nil {
				return err
			}
		}
		var sections []schema.LayoutSection
		if err := tx.Where("layout_id = ?", manifest.LayoutID).Find(&sections).Error; err != nil {
			return err
		}
		byID := map[string]schema.LayoutSection{}
		for _, section := range sections {
			byID[section.ID] = section
		}
		next := map[string]domain.AreaBinding{}
		seen := map[string]bool{}
		for sectionID, ticketID := range tickets {
			section, exists := byID[sectionID]
			if !exists || (section.Kind != "standing" && section.Kind != "booth") || section.Capacity <= 0 || ticketID == "" || seen[ticketID] {
				return fmt.Errorf("%w: each admission area needs its own ticket", domain.ErrInvalidTicket)
			}
			seen[ticketID] = true
			next[sectionID] = domain.AreaBinding{TicketID: ticketID, Name: section.Name, Capacity: section.Capacity}
		}
		// Lock in the same order as checkout to avoid deadlocks across ticket tiers.
		ids := make([]string, 0, len(seen)+len(current))
		for id := range seen {
			ids = append(ids, id)
		}
		for _, area := range current {
			if !seen[area.TicketID] {
				ids = append(ids, area.TicketID)
			}
		}
		sort.Strings(ids)
		locked := map[string]schema.Ticket{}
		for _, id := range ids {
			var ticket schema.Ticket
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND event_id = ?", id, eventID).First(&ticket).Error; err != nil {
				return fmt.Errorf("%w: ticket must belong to this event", domain.ErrInvalidTicket)
			}
			locked[id] = ticket
		}
		for sectionID, area := range next {
			ticket := locked[area.TicketID]
			if ticket.Quantity > area.Capacity {
				return fmt.Errorf("%w: ticket quantity exceeds area capacity", domain.ErrInvalidTicket)
			}
			var seats int64
			if err := tx.Model(&schema.EventSeat{}).Where("ticket_id = ?", area.TicketID).Count(&seats).Error; err != nil {
				return err
			}
			if seats > 0 {
				return fmt.Errorf("%w: numbered seats cannot price a counted area", domain.ErrInvalidTicket)
			}
			if current[sectionID] == area {
				continue
			}
			if err := untouchedAreaTicket(tx, ticket); err != nil {
				return err
			}
		}
		for sectionID, area := range current {
			if next[sectionID] != area {
				if err := untouchedAreaTicket(tx, locked[area.TicketID]); err != nil {
					return err
				}
			}
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		return tx.Model(&schema.EventSeating{}).Where("event_id = ?", eventID).Update("areas", encoded).Error
	})
}

func untouchedAreaTicket(tx *gorm.DB, ticket schema.Ticket) error {
	if ticket.Sold > 0 || ticket.Reserved > 0 {
		return domain.ErrSeatsSold
	}
	var orders int64
	if err := tx.Model(&schema.OrderItem{}).Where("ticket_id = ?", ticket.ID).Count(&orders).Error; err != nil {
		return err
	}
	if orders > 0 {
		return domain.ErrSeatsSold
	}
	return nil
}
