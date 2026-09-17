package report

import (
	"encoding/csv"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	eventdomain "vozkot/domain/event"
	domain "vozkot/domain/report"
)

// The CSV an organiser actually opens.
//
// Three decisions here are about Excel in Portuguese rather than about CSV, and
// all three are the difference between a file that opens and a file that
// arrives as one column of garbage:
//
//   - SEMICOLON, not comma. Excel uses the list separator of the system locale,
//     and in pt-BR that is ";". A comma-separated file opens as a single column
//     and the organiser's first experience of the feature is Data > Text to
//     Columns.
//   - A UTF-8 BOM. Without it Excel reads the bytes as the legacy code page and
//     every "ã" and "ç" in a Brazilian name is mangled.
//   - COMMA as the decimal separator, because the file is read by a person in
//     pt-BR and 52.80 is eight and a half thousand reais to their spreadsheet.
//
// Money is written in reais rather than centavos for the same reason: this file
// is for a person with a calculator, not for a parser.
const (
	csvSeparator = ';'
	// utf8BOM is what tells Excel the file is Unicode.
	utf8BOM = "\uFEFF"
)

// csvWriter streams rows straight to the response.
type csvWriter struct {
	response http.ResponseWriter
	writer   *csv.Writer
	started  bool
	rows     int
}

func newCSVWriter(response http.ResponseWriter) *csvWriter {
	return &csvWriter{response: response}
}

// Started reports whether any bytes have gone out, which decides whether a
// failure can still be reported as an HTTP error.
func (w *csvWriter) Started() bool { return w.started }

// Write emits one batch, writing the headers and the column row first.
//
// The headers are deliberately written HERE, on the first batch, rather than
// before the query runs: an authorisation or query failure before any row
// exists can then still answer with a JSON error and the right status code.
func (w *csvWriter) Write(batch []domain.Attendee) error {
	if !w.started {
		w.begin()
	}
	for _, attendee := range batch {
		record := []string{
			attendee.OrderID,
			attendee.PurchasedAt.Format("2006-01-02 15:04:05"),
			statusLabel(attendee.Status),
			attendee.Name,
			attendee.Email,
			attendee.DocumentMask,
			genderLabel(attendee.Gender),
			ageLabel(attendee.AgeYears),
			bracketLabel(domain.BracketFor(attendee.AgeYears)),
			attendee.City,
			attendee.UF,
			attendee.TicketTitle,
			strconv.Itoa(attendee.Quantity),
			reais(attendee.UnitPriceCents),
			reais(attendee.NetCents),
			strconv.Itoa(attendee.AdmittedCount),
		}
		if err := w.writer.Write(record); err != nil {
			return err
		}
		w.rows++
	}
	// Flushed per batch so the browser starts receiving immediately and the
	// download does not look frozen while a large event is read.
	w.writer.Flush()
	if flusher, ok := w.response.(http.Flusher); ok {
		flusher.Flush()
	}
	return w.writer.Error()
}

// begin writes the headers and the column row.
func (w *csvWriter) begin() {
	w.started = true
	w.response.Header().Set("Content-Type", "text/csv; charset=utf-8")
	// The filename is set later, when the event is known, but a browser needs
	// the disposition before the body. A provisional name is written now and
	// replaced by Flush only when nothing has been sent yet; in the streaming
	// case the provisional one stands, which is why it is already meaningful.
	w.response.Header().Set("Content-Disposition", `attachment; filename="participantes.csv"`)
	w.response.Header().Set("X-Content-Type-Options", "nosniff")
	w.response.WriteHeader(http.StatusOK)

	_, _ = w.response.Write([]byte(utf8BOM))
	w.writer = csv.NewWriter(w.response)
	w.writer.Comma = csvSeparator

	// Portuguese, because the organiser reading this file is the same person
	// reading the dashboard, and a spreadsheet with English headers in a
	// Portuguese product is a translation somebody has to do by hand.
	_ = w.writer.Write([]string{
		"Pedido", "Data da compra", "Situação",
		"Nome", "E-mail", "Documento",
		"Sexo", "Idade", "Faixa etária",
		"Cidade", "Estado",
		"Ingresso", "Quantidade",
		// Face value and the organiser's earnings. Our commission and the
		// total the buyer paid are not columns here: this file is opened in a
		// spreadsheet, where a difference between two columns is one
		// subtraction away. See the money note in domain/report.
		"Valor unitário", "Valor do organizador",
		"Entradas usadas",
	})
}

// Flush finishes the file. The event is used only to name it.
func (w *csvWriter) Flush(happening *eventdomain.Event) error {
	if !w.started {
		// No rows at all. The file still has to be a valid, openable CSV with
		// its headers, or an organiser whose event has not sold anything gets a
		// zero-byte download and assumes the feature is broken.
		w.begin()
	}
	if happening != nil {
		w.response.Header().Set("Content-Disposition", fmt.Sprintf(
			`attachment; filename="participantes-%s.csv"`, filenameFor(happening),
		))
	}
	w.writer.Flush()
	return w.writer.Error()
}

// filenameFor is the event's slug, which is already URL-safe and lower-case.
func filenameFor(happening *eventdomain.Event) string {
	slug := strings.TrimSpace(happening.Slug)
	if slug == "" {
		slug = happening.ID
	}
	if len(slug) > 60 {
		slug = slug[:60]
	}
	return slug
}

// reais renders centavos the way a Brazilian spreadsheet expects: no thousands
// separator (Excel adds its own), comma for the decimal.
func reais(cents int64) string {
	negative := cents < 0
	if negative {
		cents = -cents
	}
	formatted := fmt.Sprintf("%d,%02d", cents/100, cents%100)
	if negative {
		return "-" + formatted
	}
	return formatted
}

// ageLabel leaves the cell EMPTY rather than writing a zero.
//
// A zero in an age column is a number a spreadsheet will happily average, and
// the average age of an audience computed with a pile of zeroes in it is worse
// than no answer.
func ageLabel(years int) string {
	if years <= 0 {
		return ""
	}
	return strconv.Itoa(years)
}

const notInformed = "Não informado"

func genderLabel(gender string) string {
	switch gender {
	case "female":
		return "Feminino"
	case "male":
		return "Masculino"
	case "non_binary":
		return "Não binário"
	case "other":
		return "Outro"
	default:
		// Covers both "undisclosed" and an empty value: to a spreadsheet of an
		// audience the two mean the same thing.
		return notInformed
	}
}

func bracketLabel(bracket domain.AgeBracket) string {
	switch bracket {
	case domain.AgeUpTo18:
		return "Até 18 anos"
	case domain.Age19To23:
		return "19 a 23 anos"
	case domain.Age24To28:
		return "24 a 28 anos"
	case domain.Age29To33:
		return "29 a 33 anos"
	case domain.Age34To38:
		return "34 a 38 anos"
	case domain.Age39To43:
		return "39 a 43 anos"
	case domain.Age44To48:
		return "44 a 48 anos"
	case domain.Age49To53:
		return "49 a 53 anos"
	case domain.Age54To58:
		return "54 a 58 anos"
	case domain.Age59Plus:
		return "59 anos ou mais"
	default:
		return notInformed
	}
}

func statusLabel(status string) string {
	switch status {
	case "paid":
		return "Pago"
	case "refunded":
		return "Estornado"
	case "refund_required":
		return "Estorno pendente"
	default:
		return status
	}
}
