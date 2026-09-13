package main

import (
	"bytes"
	"image"
	_ "image/png"
	"strings"
	"testing"
	"time"

	domainevent "vozkot/domain/event"
)

func TestMockEventsCoverEveryCategory(t *testing.T) {
	now := time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC)
	items := mockEvents(now)
	wantTotal := len(domainevent.Categories()) * mockEventsPerCategory
	if len(items) != wantTotal {
		t.Fatalf("mockEvents() returned %d events, want %d", len(items), wantTotal)
	}

	counts := make(map[domainevent.Category]int)
	slugs := make(map[string]string, len(items))
	checkedCovers := make(map[string]bool)
	freeEvents := 0
	for _, item := range items {
		counts[item.Category]++
		if !item.Category.Valid() {
			t.Errorf("%q has invalid category %q", item.Name, item.Category)
		}
		if !strings.Contains(item.Description, item.Name) {
			t.Errorf("description for %q does not contain its title", item.Name)
		}
		if !item.StartsAt.After(now) {
			t.Errorf("%q starts at %s, not after %s", item.Name, item.StartsAt, now)
		}
		if !item.EndsAt.After(item.StartsAt) {
			t.Errorf("%q ends at %s, not after %s", item.Name, item.EndsAt, item.StartsAt)
		}
		if !item.Location.HasCoordinates() || item.Location.City == "" || item.Location.UF == "" {
			t.Errorf("%q has an incomplete location: %+v", item.Name, item.Location)
		}
		if len(item.Tiers) != 3 {
			t.Errorf("%q has %d ticket tiers, want 3", item.Name, len(item.Tiers))
		}
		tierTitles := make(map[string]bool, len(item.Tiers))
		hasFreeTier := false
		for _, tier := range item.Tiers {
			if tier.Title == "" || tier.Description == "" || tier.Quantity <= 0 || tier.PriceCents < 0 {
				t.Errorf("%q has an invalid ticket tier: %+v", item.Name, tier)
			}
			if tierTitles[tier.Title] {
				t.Errorf("%q has duplicate ticket title %q", item.Name, tier.Title)
			}
			tierTitles[tier.Title] = true
			hasFreeTier = hasFreeTier || tier.PriceCents == 0
		}
		if hasFreeTier {
			freeEvents++
		}

		slug := domainevent.Slugify(item.Name)
		if previous := slugs[slug]; previous != "" {
			t.Errorf("%q and %q share slug %q", previous, item.Name, slug)
		}
		slugs[slug] = item.Name

		if checkedCovers[item.CoverPath] {
			continue
		}
		checkedCovers[item.CoverPath] = true
		data, err := mockCovers.ReadFile(item.CoverPath)
		if err != nil {
			t.Errorf("read %s: %v", item.CoverPath, err)
			continue
		}
		configuration, format, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			t.Errorf("decode %s: %v", item.CoverPath, err)
			continue
		}
		if format != "png" {
			t.Errorf("%s format = %q, want png", item.CoverPath, format)
		}
		if configuration.Width != 2*configuration.Height {
			t.Errorf("%s dimensions = %dx%d, want a 2:1 cover", item.CoverPath, configuration.Width, configuration.Height)
		}
	}

	for _, category := range domainevent.Categories() {
		if counts[category] != mockEventsPerCategory {
			t.Errorf("category %q has %d events, want %d", category, counts[category], mockEventsPerCategory)
		}
	}
	if len(checkedCovers) != len(domainevent.Categories()) {
		t.Errorf("checked %d distinct covers, want %d", len(checkedCovers), len(domainevent.Categories()))
	}
	// Five percent keeps the free rail populated without making free admission
	// dominate the first page of this otherwise paid ticketing catalogue.
	wantFree := 8
	if freeEvents != wantFree {
		t.Errorf("free event count = %d, want %d", freeEvents, wantFree)
	}
}
