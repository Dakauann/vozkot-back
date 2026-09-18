package schema

import (
	"time"

	"vozkot/infra/crypto/piigorm"
)

// AdmissionCodeBlindScope separates admission codes from every other blind
// index in the system. See the note on the scopes in user_schema.go.
const AdmissionCodeBlindScope = "admission.code.v1"

// Admission is one person's right to enter, issued against a paid order.
//
// The row a doorperson's scan resolves, and the only table in this system that
// is read on the critical path of a queue of people waiting in the cold. Two
// consequences run through the whole shape of it:
//
//   - the lookup is ONE indexed equality on code_blind, with no join. An event
//     with forty thousand admissions is scanned at a few hundred per minute in
//     bursts, and a scan that needed the order and the event beside it would
//     be three reads under a turnstile.
//   - the state transition is a conditional UPDATE, not a read and a write.
//     See the Admit method on the repository for why that is not a
//     micro-optimisation.
type Admission struct {
	ID string `gorm:"primaryKey;type:varchar(32)"`

	// OrderID is what was paid for. RESTRICTed rather than cascaded for the
	// same reason an order's event is: deleting the order would destroy the
	// record of who was let in on it.
	OrderID string `gorm:"not null;type:varchar(32);index:idx_admissions_order_id;uniqueIndex:idx_admissions_order_line_sequence,priority:1"`
	// EventID is the door this admits to, denormalised onto the row on
	// purpose. It is checked on every scan, a valid ticket for the wrong
	// stage is a real and frequent case, and reaching the order to find it
	// would put a join on the turnstile.
	EventID string `gorm:"not null;type:varchar(32);index:idx_admissions_event_id;index:idx_admissions_event_status,priority:1"`
	// TicketID and TicketTitle are the tier, the title frozen at issue so
	// renaming a tier cannot rewrite a ticket somebody is holding.
	TicketID    string `gorm:"not null;type:varchar(32);index:idx_admissions_ticket_id;uniqueIndex:idx_admissions_order_line_sequence,priority:2"`
	TicketTitle string `gorm:"not null;type:varchar(255);default:''"`
	// Sequence numbers the admissions within one order line, from 1, so a
	// ticket can print "2 de 3".
	//
	// Part of a UNIQUE index with the order and the tier, and that index is
	// what makes issuing idempotent: a redelivered webhook that tries to mint
	// a second set for the same order collides instead of doubling it.
	Sequence int `gorm:"not null;default:1;uniqueIndex:idx_admissions_order_line_sequence,priority:3"`
	// SeatID and the labels beside it are the reserved chair, frozen at issue
	// and empty for a general-admission ticket.
	//
	// No foreign key to event_seats, deliberately: these columns are a
	// SNAPSHOT, and the whole point of one is that it outlives the row it came
	// from. A RESTRICT would stop an organiser ever retiring a layout, and a
	// CASCADE would delete somebody's ticket.
	SeatID      string `gorm:"type:varchar(32);not null;default:'';index:idx_admissions_seat_id"`
	SeatSection string `gorm:"not null;type:varchar(120);default:''"`
	SeatRow     string `gorm:"not null;type:varchar(16);default:''"`
	SeatLabel   string `gorm:"not null;type:varchar(16);default:''"`

	// Code is the secret, sealed at rest. CodeBlind is its searchable
	// companion and is UNIQUE, which is the guarantee that no two admissions
	// ever share a code: the generator makes that unlikely, this makes it
	// impossible.
	//
	// Sealed rather than hashed, because the holder has to be able to see it
	// again: a buyer who lost the email opens their order and needs the same
	// QR back. A one-way hash would make that impossible and push everyone to
	// support. Sealed-plus-blind-index is the same trade the buyer's document
	// makes, for the same reason.
	Code      piigorm.EncryptedString `gorm:"type:bytea"`
	CodeBlind piigorm.BlindIndex      `gorm:"type:bytea;uniqueIndex:idx_admissions_code_blind"`

	// Status is issued, admitted or void. Indexed together with the event
	// because the door's counter asks exactly that: how many of this event's
	// admissions are still unspent.
	Status string `gorm:"not null;type:varchar(16);default:'issued';index:idx_admissions_event_status,priority:2"`

	AdmittedAt *time.Time `gorm:"index:idx_admissions_admitted_at"`
	// AdmittedBy is the account that scanned it, for the audit an operator
	// needs when a holder disputes being turned away.
	AdmittedBy string     `gorm:"not null;type:varchar(32);default:''"`
	VoidedAt   *time.Time `gorm:""`

	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

// TableName keeps the table out of GORM's pluralisation guesswork.
func (Admission) TableName() string { return "admissions" }
