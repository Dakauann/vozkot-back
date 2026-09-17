package main

import (
	"context"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
	"vozkot/infra/database/schema"
	seatingRepository "vozkot/infra/repositories/seating"
)

// A targeted development fixture upgrade. Never guess where an existing buyer
// meant to sit: the old pooled ticket must have no orders, sales or holds.
func splitSeededConcertAreas(ctx context.Context, db *gorm.DB, slug string) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var event schema.Event
		if err := tx.Where("slug = ?", slug).First(&event).Error; err != nil {
			return err
		}
		var manifest schema.EventSeating
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("event_id = ?", event.ID).First(&manifest).Error; err != nil {
			return err
		}
		if len(manifest.Areas) > 2 {
			return fmt.Errorf("areas already configured; use the organiser's area settings")
		}
		var sections []schema.LayoutSection
		if err := tx.Where("layout_id = ? AND kind IN ?", manifest.LayoutID, []string{"standing", "booth"}).Find(&sections).Error; err != nil {
			return err
		}
		byName := map[string]schema.LayoutSection{}
		for _, section := range sections {
			byName[section.Name] = section
		}
		if len(byName) != 3 || byName["Camarote esquerdo"].Capacity != 10 || byName["Camarote direito"].Capacity != 10 || byName["Pista"].Capacity != 600 {
			return fmt.Errorf("event does not match the seeded concert layout")
		}
		var pooled, pista schema.Ticket
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("event_id = ? AND title = ?", event.ID, "Camarote").First(&pooled).Error; err != nil {
			return err
		}
		var orders int64
		if err := tx.Model(&schema.OrderItem{}).Where("ticket_id = ?", pooled.ID).Count(&orders).Error; err != nil {
			return err
		}
		if orders > 0 || pooled.Sold > 0 || pooled.Reserved > 0 || pooled.Quantity != 20 {
			return fmt.Errorf("pooled Camarote has purchase history or changed capacity; refusing to reassign it")
		}
		if err := tx.Where("event_id = ? AND title = ?", event.ID, "Pista").First(&pista).Error; err != nil {
			return err
		}
		if err := tx.Model(&pooled).Updates(map[string]any{"title": "Camarote esquerdo", "description": "Entrada individual no camarote esquerdo, sem assento numerado. Não reserva o camarote inteiro.", "quantity": 10}).Error; err != nil {
			return err
		}
		right := pooled
		right.ID = newID("tkt")
		right.Title = "Camarote direito"
		right.Description = "Entrada individual no camarote direito, sem assento numerado. Não reserva o camarote inteiro."
		right.Quantity = 10
		right.CreatedAt = time.Now().UTC()
		right.UpdatedAt = right.CreatedAt
		if err := tx.Create(&right).Error; err != nil {
			return err
		}
		return seatingRepository.NewSeatRepository(tx).BindAreas(ctx, event.ID, map[string]string{
			byName["Pista"].ID:             pista.ID,
			byName["Camarote esquerdo"].ID: pooled.ID,
			byName["Camarote direito"].ID:  right.ID,
		})
	})
}
