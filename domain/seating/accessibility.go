package seating

import "sort"

// Where the accessible seats go.
//
// The studio used to tell an organiser "mark the seats in the section form"
// while giving them no way to mark anything. This is the other half of that
// sentence: given a room and what it is short of, choose the chairs.
//
// It is a suggestion and not a decision. A real house knows things this cannot
//: which end has the ramp, where the platform is, which row the lift opens
// onto, so the organiser can move any of them afterwards. What it removes is
// the blank page.

// SuggestAccessibleSeats picks chairs to satisfy the quotas a room is missing.
//
// The choice follows how houses are actually built:
//
//   - The BACK row of each section, because that is where a wheelchair reaches
//     without crossing anybody's knees and where a platform usually is. Front
//     rows are also the expensive ones, and quietly converting them would cost
//     an organiser money they did not agree to spend.
//   - From the OUTSIDE IN, because an aisle end is reachable.
//   - Wheelchair spaces PAIRED with a companion seat beside them, because the
//     law requires the pair and a space with nobody next to it is not usable.
//
// Returns the kinds keyed by SeatKindKey, ready to merge into a RowSpec's
// KindByLabel. Only what is missing is suggested: a room already carrying half
// its wheelchair spaces is not asked to double them.
func SuggestAccessibleSeats(seats []Seat, report Compliance) map[string]SeatKind {
	suggestion := map[string]SeatKind{}
	if len(seats) == 0 {
		return suggestion
	}

	// What is owed, over what is already there.
	needWheelchair := report.RequiredWheelchair - report.HaveWheelchair
	needCompanion := report.RequiredWheelchair - report.HaveCompanion
	needReduced := report.RequiredReducedMobility - report.HaveReducedMobility
	needObese := report.RequiredObese - report.HaveObese
	// An obese seat also counts towards reduced mobility, so satisfying the
	// smaller quota first means the larger one needs that many fewer.
	if needObese > 0 {
		needReduced -= needObese
	}

	if needWheelchair <= 0 && needCompanion <= 0 && needReduced <= 0 && needObese <= 0 {
		return suggestion
	}

	// Candidates: the back row of each section, outermost seats first.
	//
	// Sorted by section, then by the LAST row, then outward from the middle, so
	// taking from the front of this list takes the most reachable chairs of the
	// least valuable row.
	back := backRowSeats(seats)
	if len(back) == 0 {
		return suggestion
	}

	taken := 0
	claim := func(kind SeatKind, count int) {
		for count > 0 && taken < len(back) {
			seat := back[taken]
			taken++
			// Never overwrite a chair the organiser already marked.
			if seat.Kind != SeatStandard {
				continue
			}
			suggestion[SeatKindKey(seat.RowLabel, seat.SeatLabel)] = kind
			count--
		}
	}

	// Wheelchair spaces and their companions, alternating, so each space gets a
	// neighbour rather than ending up with every space at one end and every
	// companion at the other.
	pairs := needWheelchair
	if needCompanion > pairs {
		pairs = needCompanion
	}
	for index := 0; index < pairs; index++ {
		if index < needWheelchair {
			claim(SeatWheelchair, 1)
		}
		if index < needCompanion {
			claim(SeatCompanion, 1)
		}
	}
	if needObese > 0 {
		claim(SeatObese, needObese)
	}
	if needReduced > 0 {
		claim(SeatReducedMobility, needReduced)
	}
	return suggestion
}

// backRowSeats is each section's last row, ordered outward from its centre.
func backRowSeats(seats []Seat) []Seat {
	lastRow := map[string]int{}
	for _, seat := range seats {
		if seat.RowOrder > lastRow[seat.SectionID] {
			lastRow[seat.SectionID] = seat.RowOrder
		}
	}

	back := make([]Seat, 0, 32)
	for _, seat := range seats {
		if seat.RowOrder == lastRow[seat.SectionID] {
			back = append(back, seat)
		}
	}

	// Outward from the middle of the row: the ends are the reachable ones.
	middle := map[string]float64{}
	counts := map[string]float64{}
	for _, seat := range back {
		middle[seat.SectionID] += float64(seat.SeatOrder)
		counts[seat.SectionID]++
	}
	for section := range middle {
		middle[section] /= counts[section]
	}

	sort.SliceStable(back, func(a, b int) bool {
		if back[a].SectionID != back[b].SectionID {
			return back[a].SectionID < back[b].SectionID
		}
		distanceA := float64(back[a].SeatOrder) - middle[back[a].SectionID]
		distanceB := float64(back[b].SeatOrder) - middle[back[b].SectionID]
		if distanceA < 0 {
			distanceA = -distanceA
		}
		if distanceB < 0 {
			distanceB = -distanceB
		}
		// Furthest from the middle first.
		return distanceA > distanceB
	})
	return back
}
