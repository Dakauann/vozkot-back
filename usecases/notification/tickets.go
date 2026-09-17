package notification

import (
	"context"
	"fmt"
	"log"

	admissiondomain "vozkot/domain/admission"
	domain "vozkot/domain/notification"
)

// Tickets supplies the admissions a receipt carries.
//
// An interface declared HERE, by the consumer, rather than a dependency on the
// admission use case: this package needs exactly one question answered and
// should not be able to ask any others.
type Tickets interface {
	ForReceipt(ctx context.Context, orderID string) ([]Ticket, error)
}

// Ticket is one admission as a receipt renders it.
type Ticket struct {
	Title string
	// Sequence and Total read as "2 de 3" on the ticket.
	Sequence int
	Total    int
	// Code is the printed form, grouped for typing.
	Code string
	// ContentID is what the template's img src points at with cid:.
	ContentID string
	// QR is the PNG, travelling with the message as an inline part.
	QR []byte
}

// AdmissionTickets loads admissions and renders their codes.
//
// It exists so the codes are fetched at SEND time and never at enqueue time.
// The job payload is plaintext jsonb in a table whose completed rows live for
// a week and whose dead rows live forever; putting twelve-character working
// tickets in there would undo the encryption the admissions table bothers
// with. The payload carries an order id, and the codes are read, rendered and
// attached in the moments before the message leaves.
type AdmissionTickets struct {
	admissions admissiondomain.Repository
	renderer   admissiondomain.CodeRenderer
}

func NewAdmissionTickets(
	admissions admissiondomain.Repository,
	renderer admissiondomain.CodeRenderer,
) *AdmissionTickets {
	return &AdmissionTickets{admissions: admissions, renderer: renderer}
}

var _ Tickets = (*AdmissionTickets)(nil)

// ForReceipt is every admission on an order, with its QR rendered.
func (a *AdmissionTickets) ForReceipt(ctx context.Context, orderID string) ([]Ticket, error) {
	if a == nil || a.admissions == nil {
		return nil, nil
	}
	issued, err := a.admissions.ListByOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}

	tickets := make([]Ticket, 0, len(issued))
	for index := range issued {
		item := issued[index]
		if item.Status == admissiondomain.StatusVoid {
			// A refunded ticket is not sent. The receipt for the sale was sent
			// when the sale happened; a resend after the refund must not hand
			// over a code the door will turn away.
			continue
		}

		ticket := Ticket{
			Title:     item.TicketTitle,
			Sequence:  item.Sequence,
			Total:     len(issued),
			Code:      item.Code.Formatted(),
			ContentID: fmt.Sprintf("ticket-%s", item.ID),
		}
		if a.renderer != nil {
			png, renderErr := a.renderer.PNG(item.Code)
			if renderErr != nil {
				// The printed code is still on the ticket, so a receipt
				// without its image is degraded rather than useless. Failing
				// the whole send over a missing picture would cost the buyer
				// the code as well.
				log.Printf("notification: could not render the QR for admission %s: %v", item.ID, renderErr)
			} else {
				ticket.QR = png
			}
		}
		tickets = append(tickets, ticket)
	}
	return tickets, nil
}

// inlineFor is the attachment set for a batch of tickets.
func inlineFor(tickets []Ticket) []domain.Inline {
	parts := make([]domain.Inline, 0, len(tickets))
	for _, ticket := range tickets {
		if len(ticket.QR) == 0 {
			continue
		}
		parts = append(parts, domain.Inline{
			ContentID:   ticket.ContentID,
			Filename:    ticket.ContentID + ".png",
			ContentType: "image/png",
			Content:     ticket.QR,
		})
	}
	return parts
}

// templateData is the tickets as the template sees them.
//
// The PNG is deliberately NOT in here. It travels as an attachment, and a
// template that could reach the bytes is a template one edit away from
// base64-ing them into an img src, which is the thing that does not render in
// Gmail.
func templateData(tickets []Ticket) []map[string]any {
	rows := make([]map[string]any, 0, len(tickets))
	for _, ticket := range tickets {
		rows = append(rows, map[string]any{
			"Title":     ticket.Title,
			"Sequence":  ticket.Sequence,
			"Total":     ticket.Total,
			"Code":      ticket.Code,
			"ContentID": ticket.ContentID,
			"HasQR":     len(ticket.QR) > 0,
		})
	}
	return rows
}
