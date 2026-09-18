// Package seating is the transport for reserved seating: an organiser drawing
// a room, and a buyer reading the map of one.
//
// Two audiences, and the split between them is the reason the routes are where
// they are. Everything under /venues and /layouts is an organiser editing, and
// authorization for it lives in usecases/seating behind one owned() per
// aggregate. Everything under /events/{id}/seating is a buyer LOOKING, is
// public, and is deliberately served by a view type that carries no order id
// and no hold deadline: a picker that told every visitor which account holds
// which chair would be telling them exactly when to race for it.
//
// This package decides nothing about access. It turns a session into an actor,
// a body into a spec, and an error into a status.
package seating

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"vozkot/delivery/http/httpx"
	authdomain "vozkot/domain/auth"
	eventdomain "vozkot/domain/event"
	domain "vozkot/domain/seating"
	usecase "vozkot/usecases/seating"
)

type Handler struct {
	seating *usecase.Service
}

func NewHandler(seating *usecase.Service) *Handler { return &Handler{seating: seating} }

// Register mounts the organiser's editor.
func (h *Handler) Register(router *http.ServeMux) {
	router.HandleFunc("POST /api/v1/venues", h.createVenue)
	router.HandleFunc("GET /api/v1/venues", h.listVenues)
	router.HandleFunc("POST /api/v1/venues/{id}/layouts", h.createLayout)
	router.HandleFunc("GET /api/v1/venues/{id}/layouts", h.listLayouts)
	router.HandleFunc("GET /api/v1/layouts/{id}", h.layout)
	router.HandleFunc("PUT /api/v1/layouts/{id}/sections", h.generate)
	router.HandleFunc("POST /api/v1/layouts/{id}/preview", h.preview)
	router.HandleFunc("GET /api/v1/layouts/{id}/compliance", h.compliance)
	router.HandleFunc("POST /api/v1/layouts/{id}/publish", h.publish)
	router.HandleFunc("POST /api/v1/events/{id}/seating", h.bind)
	router.HandleFunc("PUT /api/v1/events/{id}/seating/areas", h.bindAreas)
	router.HandleFunc("POST /api/v1/events/{id}/seats/block", h.block)
	router.HandleFunc("POST /api/v1/events/{id}/seats/unblock", h.unblock)
}

// RegisterPublicRoutes mounts what a buyer reads. No session required.
func (h *Handler) RegisterPublicRoutes(router *http.ServeMux) {
	router.HandleFunc("GET /api/v1/events/{id}/seating/map", h.seatMap)
	router.HandleFunc("GET /api/v1/events/{id}/seating/availability", h.availability)
	router.HandleFunc("GET /api/v1/events/{id}/seating/best", h.bestAvailable)
}

// --- the organiser ----------------------------------------------------------

type VenueRequest struct {
	Name string `json:"name" example:"Teatro Municipal"`
}

type VenueResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Capacity int    `json:"capacity"`
}

type VenueEnvelope struct {
	Data VenueResponse `json:"data"`
}

type VenueListEnvelope struct {
	Data  []VenueResponse `json:"data"`
	Total int64           `json:"total"`
}

// @Summary		Criar local
// @Description	Cria um local físico reutilizável. Um teatro desenha sua planta uma vez e vende duzentas noites a partir dela.
// @Tags			Assentos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		request body VenueRequest true "Dados do local"
// @Success		201 {object} VenueEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/venues [post]
func (h *Handler) createVenue(response http.ResponseWriter, request *http.Request) {
	var payload VenueRequest
	if !decode(response, request, &payload) {
		return
	}
	venue, err := h.seating.CreateVenue(request.Context(), actor(request), usecase.CreateVenueInput{
		Name: payload.Name,
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusCreated, VenueEnvelope{Data: toVenue(venue)})
}

// @Summary		Listar locais
// @Description	Os locais do operador autenticado. Administradores veem todos.
// @Tags			Assentos
// @Produce		json
// @Security		BearerAuth
// @Param		limit query int false "Itens por página (padrão 20)"
// @Param		offset query int false "Deslocamento da paginação"
// @Success		200 {object} VenueListEnvelope
// @Failure		401 {object} ErrorResponse
// @Router		/api/v1/venues [get]
func (h *Handler) listVenues(response http.ResponseWriter, request *http.Request) {
	limit := intQuery(request, "limit", 20)
	offset := intQuery(request, "offset", 0)
	venues, total, err := h.seating.ListVenues(request.Context(), actor(request), limit, offset)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	data := make([]VenueResponse, 0, len(venues))
	for index := range venues {
		data = append(data, toVenue(&venues[index]))
	}
	httpx.WriteJSON(response, http.StatusOK, VenueListEnvelope{Data: data, Total: total})
}

type LayoutRequest struct {
	Name          string `json:"name" example:"Configuração padrão"`
	ViewBoxWidth  int    `json:"viewBoxWidth" example:"1000"`
	ViewBoxHeight int    `json:"viewBoxHeight" example:"800"`
}

type LayoutResponse struct {
	ID            string `json:"id"`
	VenueID       string `json:"venueId"`
	Name          string `json:"name"`
	Version       int    `json:"version"`
	Status        string `json:"status"`
	Frozen        bool   `json:"frozen"`
	ViewBoxWidth  int    `json:"viewBoxWidth"`
	ViewBoxHeight int    `json:"viewBoxHeight"`
	// SeatCount is how many named chairs the plan holds. Present on a listing,
	// so plans can be told apart without opening each one.
	SeatCount int `json:"seatCount,omitempty"`
}

type LayoutEnvelope struct {
	Data LayoutResponse `json:"data"`
}

type LayoutListEnvelope struct {
	Data []LayoutResponse `json:"data"`
}

// @Summary		Criar planta
// @Description	Cria uma planta do local. Um mesmo local pode ter mais de uma: a configuração padrão e a acústica, com o palco adiantado, têm assentos diferentes em lugares diferentes.
// @Tags			Assentos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do local"
// @Param		request body LayoutRequest true "Dados da planta"
// @Success		201 {object} LayoutEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/venues/{id}/layouts [post]
func (h *Handler) createLayout(response http.ResponseWriter, request *http.Request) {
	var payload LayoutRequest
	if !decode(response, request, &payload) {
		return
	}
	layout, err := h.seating.CreateLayout(request.Context(), actor(request), usecase.CreateLayoutInput{
		VenueID:       pathID(request),
		Name:          payload.Name,
		ViewBoxWidth:  payload.ViewBoxWidth,
		ViewBoxHeight: payload.ViewBoxHeight,
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusCreated, LayoutEnvelope{Data: toLayout(layout)})
}

// @Summary		Listar plantas
// @Tags			Assentos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do local"
// @Success		200 {object} LayoutListEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Router		/api/v1/venues/{id}/layouts [get]
func (h *Handler) listLayouts(response http.ResponseWriter, request *http.Request) {
	layouts, err := h.seating.ListLayouts(request.Context(), actor(request), pathID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	data := make([]LayoutResponse, 0, len(layouts))
	for index := range layouts {
		data = append(data, toLayout(&layouts[index]))
	}
	httpx.WriteJSON(response, http.StatusOK, LayoutListEnvelope{Data: data})
}

// SectionRequest is one block of the room, as the generator's form describes it.
type SectionRequest struct {
	Name string `json:"name" example:"Plateia A"`
	// Kind is what this object is. `stage` and `arena` hold no seats: they are
	// scenery a buyer orients by, placed and sized like anything else, which is
	// why they are sections and not a mode on the layout.
	Kind     string `json:"kind" example:"seated" enums:"seated,standing,booth,stage,arena"`
	Capacity int    `json:"capacity" example:"0"`
	// OffsetX and OffsetY are the object's TOP-LEFT corner, and what the canvas
	// sets by dragging. Width and Height size a marker.
	OffsetX float64 `json:"offsetX" example:"0"`
	OffsetY float64 `json:"offsetY" example:"0"`
	Width   float64 `json:"width" example:"0"`
	Height  float64 `json:"height" example:"0"`
	// Rotation turns a block of seats, in degrees clockwise about its own
	// centre. It is how rows are made to run down the SIDE of a room, which
	// dragging and resizing cannot express. Meaningless for a marker or a
	// counted area, which are resized instead.
	Rotation float64 `json:"rotation" example:"0"`
	// Category is the price band this section's seats belong to by default.
	// Empty means the section's own name, which is what an ordinary room wants.
	Category     string    `json:"category" example:"Plateia"`
	DisplayOrder int       `json:"displayOrder" example:"1"`
	Shape        []float64 `json:"shape"`
	// Definition is this same object as the editor described it, echoed back on
	// a read so a saved room can be reopened in the builder rather than only
	// looked at. Ignored on the way in: the server records what it received.
	Definition json.RawMessage `json:"definition,omitempty" swaggertype:"object"`

	// The row generator. Ignored for a standing or booth section, which lays
	// out no individual chairs.
	//
	// `rowShape` and not `shape`: this section already has a `shape`, which is
	// the polygon painted behind it. This one is how the ROWS run.
	//
	// `rowShape` chooses the geometry: `linear` is a theatre, rows along a line;
	// `arc` is a rodeo, a stadium or a gymnasium, rows as concentric arcs
	// around an arena in the middle.
	RowShape       string  `json:"rowShape" example:"linear" enums:"linear,arc"`
	Rows           int     `json:"rows" example:"12"`
	SeatsPerRow    int     `json:"seatsPerRow" example:"20"`
	FirstRowLetter string  `json:"firstRowLetter" example:"A"`
	RowLabels      string  `json:"rowLabels" example:"letters" enums:"letters,numbers"`
	Numbering      string  `json:"numbering" example:"sequential" enums:"sequential,odd_even"`
	Skips          []int   `json:"skips"`
	Curve          float64 `json:"curve" example:"0"`
	SeatGap        float64 `json:"seatGap" example:"24"`
	RowGap         float64 `json:"rowGap" example:"28"`
	// Arc only. Radius is how far the first row sits from the centre of the
	// arena; startAngle and sweepAngle are degrees clockwise from the top of
	// the map, so a stand at twelve o clock starts at zero.
	//
	// The block's PLACEMENT is not here. It belongs to the section above, which
	// is what the canvas drags; holding it in two places is one place too many.
	//
	// SeatPitch holds the spacing along an arc, so rows gain seats as they get
	// longer. That is what a real stand does; a fixed count per row fans out.
	SeatPitch  float64 `json:"seatPitch" example:"26"`
	Radius     float64 `json:"radius" example:"160"`
	StartAngle float64 `json:"startAngle" example:"0"`
	SweepAngle float64 `json:"sweepAngle" example:"90"`
	// Round tables instead of rows, when `tables` is set. A camarote at a
	// rodeo, a gala floor. Each table is a ring of seats and reads as its own
	// row, so four seats together resolves to four seats at one table.
	Tables        int     `json:"tables" example:"12"`
	SeatsPerTable int     `json:"seatsPerTable" example:"8"`
	TableRadius   float64 `json:"tableRadius" example:"34"`
	TablesPerRow  int     `json:"tablesPerRow" example:"4"`
	TableGap      float64 `json:"tableGap" example:"110"`
	FirstTable    int     `json:"firstTable" example:"1"`
	// SeatCategories puts individual chairs in a different price band, keyed
	// "FILA/ASSENTO". It is what prices the front three rows above the rest, and
	// the partial-view chair behind a pillar below it, neither of which is a
	// contiguous block that could be a sector of its own.
	SeatCategories map[string]string `json:"seatCategories,omitempty"`
	// SeatKinds marks individual chairs, keyed "FILA/ASSENTO", as in "K/12".
	//
	// This is where the accessibility seats the law requires get set, and the
	// compliance report reads what lands here.
	SeatKinds map[string]string `json:"seatKinds"`
}

type GenerateRequest struct {
	Sections []SectionRequest `json:"sections"`
}

type SeatResponse struct {
	ID        string  `json:"id"`
	SectionID string  `json:"sectionId,omitempty"`
	Row       string  `json:"row"`
	Seat      string  `json:"seat"`
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	Kind      string  `json:"kind"`
	// Category is the price band this chair sells in, already resolved from the
	// seat's own band, its section's, and the section's name. The pricing screen
	// groups by this and needs nothing else.
	Category  string `json:"category,omitempty"`
	RowOrder  int    `json:"rowOrder"`
	SeatOrder int    `json:"seatOrder"`
}

type SectionResponse struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Kind     string  `json:"kind"`
	Capacity int     `json:"capacity"`
	OffsetX  float64 `json:"offsetX"`
	OffsetY  float64 `json:"offsetY"`
	Width    float64 `json:"width"`
	Height   float64 `json:"height"`
	// Category is the band this section's seats belong to by default, as it was
	// SET, empty when the section's own name is doing the work. The resolved
	// band travels on each seat.
	Category     string    `json:"category,omitempty"`
	Rotation     float64   `json:"rotation,omitempty"`
	DisplayOrder int       `json:"displayOrder"`
	Shape        []float64 `json:"shape,omitempty"`
	// Definition is the editor form this block was generated from. It is what
	// makes a saved room editable instead of only viewable.
	Definition json.RawMessage `json:"definition,omitempty" swaggertype:"object"`
}

type LayoutDetailEnvelope struct {
	Data     LayoutResponse    `json:"data"`
	Sections []SectionResponse `json:"sections"`
	Seats    []SeatResponse    `json:"seats"`
	// Compliance is present on a PREVIEW, computed from the draft being drawn.
	//
	// It travels with the preview rather than being fetched separately so the
	// number and the room on screen can never describe different things, which
	// they did: "against 0 places, the quotas are met" beside a 192-seat
	// sector, because the report came from the saved layout.
	Compliance *ComplianceResponse `json:"compliance,omitempty"`
	// SuggestedKinds names chairs that would satisfy the quotas the room is
	// short of, keyed "FILA/ASSENTO". Present on a preview when something is
	// missing, so the studio can offer to apply it in one action instead of
	// telling the organiser to find them by hand.
	SuggestedKinds map[string]string `json:"suggestedKinds,omitempty"`
	// Bands are the room's price bands, in the order their colour is assigned.
	//
	// Slot one is the first hue of a fixed categorical palette, slot two the
	// second, and so on, so the ORDER is the colour, and it is computed here
	// rather than in each client. Two answers to "what colour is Plateia" is a
	// room that changes colour when somebody walks between screens.
	Bands []string `json:"bands,omitempty"`
	// Collisions are the sections drawn on top of each other, by id.
	//
	// Reported on a preview and REFUSED on a save, and computed by the same rule
	// both times. The preview has to be able to draw a collision: that is how
	// an organiser sees the one they are making, so the editor marks these and
	// disables its own save, and the server refuses the request regardless.
	Collisions []string `json:"collisions,omitempty"`
}

// @Summary		Gerar assentos da planta
// @Description	Substitui os setores e assentos de uma planta a partir de um formulário de fileiras: quantas fileiras, quantos assentos, onde ficam os corredores, qual a numeração. É o que descreve uma casa brasileira em uma requisição. Recusado quando a planta já está em uso por um evento.
// @Tags			Assentos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID da planta"
// @Param		request body GenerateRequest true "Setores"
// @Success		200 {object} LayoutDetailEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse "A planta está em uso"
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/layouts/{id}/sections [put]
func (h *Handler) generate(response http.ResponseWriter, request *http.Request) {
	var payload GenerateRequest
	if !decode(response, request, &payload) {
		return
	}
	layoutID := pathID(request)
	if _, _, err := h.seating.GenerateLayout(
		request.Context(), actor(request), layoutID, toSpecs(payload.Sections)); err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	// Read back rather than echo. The generator assigns ids and positions, and
	// a client that drew the room from its own request would be drawing what it
	// asked for instead of what exists.
	h.writeLayout(response, request, layoutID)
}

// toSpecs maps the request's section forms onto the use case's specs.
//
// One mapper, used by both the save and the preview, so the two cannot read a
// form differently.
func toSpecs(sections []SectionRequest) []usecase.SectionSpec {
	specs := make([]usecase.SectionSpec, 0, len(sections))
	for _, section := range sections {
		kinds := make(map[string]domain.SeatKind, len(section.SeatKinds))
		for key, kind := range section.SeatKinds {
			kinds[key] = domain.SeatKind(strings.TrimSpace(kind))
		}
		// The request IS the definition, re-marshalled from the decoded struct
		// rather than captured from the body: it is then normalised, complete,
		// and in this endpoint's own documented shape, which is exactly what the
		// editor reads back. A marshal of a plain struct cannot fail, and a room
		// saved without its form is still a sellable room, so a failure here
		// would cost the editor and not the sale.
		section.Definition = nil
		definition, _ := json.Marshal(section)
		specs = append(specs, usecase.SectionSpec{
			Definition:   definition,
			Rotation:     section.Rotation,
			Category:     strings.TrimSpace(section.Category),
			Name:         section.Name,
			Kind:         domain.SectionKind(strings.TrimSpace(section.Kind)),
			Capacity:     section.Capacity,
			OffsetX:      section.OffsetX,
			OffsetY:      section.OffsetY,
			Width:        section.Width,
			Height:       section.Height,
			DisplayOrder: section.DisplayOrder,
			Shape:        section.Shape,
			Rows: domain.RowSpec{
				Shape:           domain.RowShape(strings.TrimSpace(section.RowShape)),
				Rows:            section.Rows,
				SeatsPerRow:     section.SeatsPerRow,
				FirstRowLetter:  section.FirstRowLetter,
				RowLabels:       domain.RowLabelStyle(strings.TrimSpace(section.RowLabels)),
				Numbering:       domain.Numbering(strings.TrimSpace(section.Numbering)),
				Skips:           section.Skips,
				Curve:           section.Curve,
				SeatGap:         section.SeatGap,
				RowGap:          section.RowGap,
				SeatPitch:       section.SeatPitch,
				Radius:          section.Radius,
				StartAngle:      section.StartAngle,
				SweepAngle:      section.SweepAngle,
				KindByLabel:     kinds,
				CategoryByLabel: section.SeatCategories,
			},
			Tables: domain.TableSpec{
				Tables:        section.Tables,
				SeatsPerTable: section.SeatsPerTable,
				TableRadius:   section.TableRadius,
				PerRow:        section.TablesPerRow,
				Gap:           section.TableGap,
				FirstTable:    section.FirstTable,
			},
		})
	}
	return specs
}

// @Summary		Prévia da planta
// @Description	Gera os assentos de um formulário e NÃO grava nada. É o que o editor desenha enquanto o organizador digita, e usa o mesmo gerador que a gravação vai usar: uma prévia construída por outro caminho é uma prévia que pode mentir. Funciona também em planta congelada: olhar não é editar.
// @Tags			Assentos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID da planta"
// @Param		request body GenerateRequest true "Setores"
// @Success		200 {object} LayoutDetailEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/layouts/{id}/preview [post]
func (h *Handler) preview(response http.ResponseWriter, request *http.Request) {
	var payload GenerateRequest
	if !decode(response, request, &payload) {
		return
	}
	layoutID := pathID(request)
	sections, seats, err := h.seating.PreviewLayout(
		request.Context(), actor(request), layoutID, toSpecs(payload.Sections))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	measured := h.seating.PreviewCompliance(toSpecs(payload.Sections), seats)
	report := toComplianceResponse(measured)

	var suggested map[string]string
	if !measured.Compliant() {
		suggested = map[string]string{}
		for key, kind := range h.seating.SuggestAccessibleSeats(seats, measured) {
			suggested[key] = string(kind)
		}
	}

	httpx.WriteJSON(response, http.StatusOK, LayoutDetailEnvelope{
		Data:           LayoutResponse{ID: layoutID},
		Sections:       toSectionResponses(sections),
		Seats:          toSeatResponses(sections, seats),
		Compliance:     &report,
		SuggestedKinds: suggested,
		Bands:          domain.BandsOf(sections, seats),
		Collisions:     h.seating.Collisions(sections, seats),
	})
}

// @Summary		Planta completa
// @Description	A planta com seus setores e assentos, para o editor desenhar.
// @Tags			Assentos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID da planta"
// @Success		200 {object} LayoutDetailEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		404 {object} ErrorResponse
// @Router		/api/v1/layouts/{id} [get]
func (h *Handler) layout(response http.ResponseWriter, request *http.Request) {
	h.writeLayout(response, request, pathID(request))
}

func (h *Handler) writeLayout(response http.ResponseWriter, request *http.Request, layoutID string) {
	detail, err := h.seating.Layout(request.Context(), actor(request), layoutID)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, LayoutDetailEnvelope{
		Data:     toLayout(&detail.Layout),
		Sections: toSectionResponses(detail.Sections),
		Seats:    toSeatResponses(detail.Sections, detail.Seats),
		Bands:    domain.BandsOf(detail.Sections, detail.Seats),
	})
}

// toSectionResponses and toSeatResponses are shared by the stored read and the
// preview, so a previewed room and a saved one are described identically.
func toSectionResponses(sections []domain.Section) []SectionResponse {
	out := make([]SectionResponse, 0, len(sections))
	for _, section := range sections {
		out = append(out, SectionResponse{
			ID:           section.ID,
			Name:         section.Name,
			Kind:         string(section.Kind),
			Capacity:     section.Capacity,
			OffsetX:      section.OffsetX,
			OffsetY:      section.OffsetY,
			Width:        section.Width,
			Height:       section.Height,
			Category:     section.Category,
			Rotation:     section.Rotation,
			DisplayOrder: section.DisplayOrder,
			Shape:        section.Shape,
			Definition:   json.RawMessage(section.Definition),
		})
	}
	return out
}

// toSeatResponses describes a room's chairs, each with its price band already
// RESOLVED.
//
// Resolved here rather than handed over as three fields to combine, because the
// fallback, the seat's band, then its section's, then the section's name, is
// a rule, and a rule repeated in a browser is a rule with two answers. The
// editor and the pricing screen both just read `category`.
func toSeatResponses(sections []domain.Section, seats []domain.Seat) []SeatResponse {
	bands := make(map[string]domain.Section, len(sections))
	for _, section := range sections {
		bands[section.ID] = section
	}
	out := make([]SeatResponse, 0, len(seats))
	for _, seat := range seats {
		section := bands[seat.SectionID]
		out = append(out, SeatResponse{
			ID:        seat.ID,
			SectionID: seat.SectionID,
			Row:       seat.RowLabel,
			Seat:      seat.SeatLabel,
			X:         seat.X,
			Y:         seat.Y,
			Kind:      string(seat.Kind),
			Category:  domain.Category(seat.Category, section.Category, section.Name),
			RowOrder:  seat.RowOrder,
			SeatOrder: seat.SeatOrder,
		})
	}
	return out
}

type ComplianceResponse struct {
	Capacity                int  `json:"capacity"`
	RequiredWheelchair      int  `json:"requiredWheelchair"`
	RequiredReducedMobility int  `json:"requiredReducedMobility"`
	RequiredObese           int  `json:"requiredObese"`
	HaveWheelchair          int  `json:"haveWheelchair"`
	HaveReducedMobility     int  `json:"haveReducedMobility"`
	HaveObese               int  `json:"haveObese"`
	HaveCompanion           int  `json:"haveCompanion"`
	Compliant               bool `json:"compliant"`
}

// @Summary		Acessibilidade da planta
// @Description	Quantos espaços para cadeira de rodas, assentos de mobilidade reduzida e assentos para obesos a planta deve ter pelo Decreto 5.296/2004 (art. 23, com a redação do Decreto 9.404/2018), e quantos ela tem. É um relatório, não um bloqueio: a plataforma não sabe se uma sala específica é legalmente casa de espetáculo, e a responsabilidade é do organizador, que precisa ver o número.
// @Tags			Assentos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID da planta"
// @Success		200 {object} ComplianceResponse
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Router		/api/v1/layouts/{id}/compliance [get]
func (h *Handler) compliance(response http.ResponseWriter, request *http.Request) {
	report, err := h.seating.Compliance(request.Context(), actor(request), pathID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, toComplianceResponse(report))
}

// toComplianceResponse is shared by the stored report and the preview, so the
// two cannot describe the same quotas differently.
func toComplianceResponse(report domain.Compliance) ComplianceResponse {
	return ComplianceResponse{
		Capacity:                report.Capacity,
		RequiredWheelchair:      report.RequiredWheelchair,
		RequiredReducedMobility: report.RequiredReducedMobility,
		RequiredObese:           report.RequiredObese,
		HaveWheelchair:          report.HaveWheelchair,
		HaveReducedMobility:     report.HaveReducedMobility,
		HaveObese:               report.HaveObese,
		HaveCompanion:           report.HaveCompanion,
		Compliant:               report.Compliant(),
	}
}

// @Summary		Publicar planta
// @Description	Torna a planta vinculável a um evento.
// @Tags			Assentos
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID da planta"
// @Success		204
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		422 {object} ErrorResponse
// @Router		/api/v1/layouts/{id}/publish [post]
func (h *Handler) publish(response http.ResponseWriter, request *http.Request) {
	if err := h.seating.PublishLayout(request.Context(), actor(request), pathID(request)); err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

type BindRequest struct {
	LayoutID string `json:"layoutId"`
	// TicketByCategory prices each price band: the band's NAME mapped to the
	// tier its seats sell at.
	//
	// A band and not a section, because where a seat is and what it costs change
	// on different clocks: the room is fixed for years and the price list
	// changes every night. A band defaults to its section's name, so an ordinary
	// room is priced exactly as it was before bands existed; setting one lets
	// two wings share a price, or the front three rows carry their own without
	// the room being redrawn to say so.
	//
	// A band left out is not sold at all, which is how a balcony is closed for
	// one night without editing the room. A band named here that no seat is in
	// is refused, because a typo would otherwise materialise half a house and
	// look like it worked.
	TicketByCategory map[string]string `json:"ticketByCategory"`
}

type SeatingEnvelope struct {
	EventID       string `json:"eventId"`
	LayoutID      string `json:"layoutId"`
	LayoutVersion int    `json:"layoutVersion"`
	SeatCount     int    `json:"seatCount"`
}

// @Summary		Colocar planta à venda
// @Description	Materializa os assentos de um evento a partir de uma planta publicada, com um lote por setor. Recusado se o evento já tem mapa: um segundo cria cadeiras duplicadas ou divergentes, e os dois são piores do que exigir que o operador diga qual queria.
// @Tags			Assentos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		request body BindRequest true "Planta e preços"
// @Success		201 {object} SeatingEnvelope
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Failure		409 {object} ErrorResponse "O evento já tem mapa de assentos"
// @Router		/api/v1/events/{id}/seating [post]
func (h *Handler) bind(response http.ResponseWriter, request *http.Request) {
	var payload BindRequest
	if !decode(response, request, &payload) {
		return
	}
	manifest, err := h.seating.Bind(request.Context(), actor(request), usecase.BindInput{
		EventID:          pathID(request),
		LayoutID:         payload.LayoutID,
		TicketByCategory: payload.TicketByCategory,
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusCreated, SeatingEnvelope{
		EventID:       manifest.EventID,
		LayoutID:      manifest.LayoutID,
		LayoutVersion: manifest.LayoutVersion,
		SeatCount:     manifest.SeatCount,
	})
}

type BlockRequest struct {
	SeatIDs []string `json:"seatIds"`
	Reason  string   `json:"reason" example:"broken" enums:"house,production,broken,distancing"`
}

type BlockResponse struct {
	// Moved is how many seats actually changed, which can be fewer than asked:
	// a held or sold seat is never taken from the person holding it, and the
	// difference is what lets a screen say "3 de 4 bloqueados; um está vendido".
	Moved int `json:"moved"`
}

// @Summary		Bloquear assentos
// @Description	Retira assentos da venda: cadeira quebrada, lugar da produção, visão obstruída. Nunca toca um assento reservado ou vendido: bloquear não cancela o ingresso de ninguém.
// @Tags			Assentos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		request body BlockRequest true "Assentos"
// @Success		200 {object} BlockResponse
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Router		/api/v1/events/{id}/seats/block [post]
func (h *Handler) block(response http.ResponseWriter, request *http.Request) {
	var payload BlockRequest
	if !decode(response, request, &payload) {
		return
	}
	moved, err := h.seating.Block(request.Context(), actor(request), pathID(request),
		payload.SeatIDs, domain.BlockReason(strings.TrimSpace(payload.Reason)))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, BlockResponse{Moved: moved})
}

// @Summary		Desbloquear assentos
// @Tags			Assentos
// @Accept		json
// @Produce		json
// @Security		BearerAuth
// @Param		id path string true "ID do evento"
// @Param		request body BlockRequest true "Assentos"
// @Success		200 {object} BlockResponse
// @Failure		401 {object} ErrorResponse
// @Failure		403 {object} ErrorResponse
// @Router		/api/v1/events/{id}/seats/unblock [post]
func (h *Handler) unblock(response http.ResponseWriter, request *http.Request) {
	var payload BlockRequest
	if !decode(response, request, &payload) {
		return
	}
	moved, err := h.seating.Unblock(request.Context(), actor(request), pathID(request), payload.SeatIDs)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, BlockResponse{Moved: moved})
}

// --- the buyer --------------------------------------------------------------

// MapSeatResponse is one chair on a picker.
//
// Note what is absent: the order holding it and when that hold expires. Both
// are on the row and neither belongs here: a picker that published them would
// tell every visitor which account to race and exactly when.
type MapSeatResponse struct {
	ID       string `json:"id"`
	TicketID string `json:"ticketId"`
	Section  string `json:"section"`
	Row      string `json:"row"`
	Seat     string `json:"seat"`
	Kind     string `json:"kind"`
	Status   string `json:"status" enums:"available,held,sold,blocked"`
	// X and Y are where the seat sits in the layout coordinate space. A picker
	// draws from these, which is what makes a rodeo stand render as an arc
	// around an arena rather than as a straight row.
	X         float64 `json:"x"`
	Y         float64 `json:"y"`
	RowOrder  int     `json:"rowOrder"`
	SeatOrder int     `json:"seatOrder"`
}

// MapMarkerResponse is a block of the room that holds no individual chairs.
//
// Two sorts, and the client draws them differently. `stage` and `arena` are
// scenery: they sell nothing and exist so a buyer can tell which end of the
// room they are looking at. `standing` and `booth` are floor a buyer can
// actually be on, sold by the head through their own tier rather than chair by
// chair, and they carry a capacity.
type MapMarkerResponse struct {
	TicketID string `json:"ticketId,omitempty"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind" enums:"stage,arena,standing,booth"`
	// Capacity is how many people it holds, for the counted sections. Omitted
	// for scenery, which holds nobody.
	Capacity int `json:"capacity,omitempty"`
	// X and Y are the marker's TOP-LEFT corner, in the same coordinate space as
	// the seats. Width and Height are its size.
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type MapEnvelope struct {
	Data []MapSeatResponse `json:"data"`
	// Markers are every block of the room that holds no individual chair: the
	// scenery, and the standing or boxed floor that sells by the head. Sent
	// with a complete map and omitted from a delta, because none of it moves
	// between two polls.
	Markers []MapMarkerResponse `json:"markers,omitempty"`
	// The price bands are deliberately NOT here. By the time a room is on sale
	// a band has become a TIER: the seat carries its ticket id and the tier
	// carries the money, so the buyer's map colours by tier, in the order the
	// price list is given, and the legend it already has is the relief the
	// palette requires. Sending a band list too would be a second answer.
	// Version is the cursor to send back as `since` on the next poll.
	Version int64 `json:"version"`
	// Complete distinguishes a whole map from a delta, so a client knows
	// whether to replace its state or patch it.
	Complete bool `json:"complete"`
}

// @Summary		Mapa de assentos
// @Description	Os assentos de um evento e a situação de cada um. Com `since`, devolve apenas o que mudou desde aquele cursor, que é o que torna barato consultar durante uma venda movimentada. Público: um mapa é o que alguém olha antes de ter conta.
// @Tags			Assentos
// @Produce		json
// @Param		id path string true "ID do evento"
// @Param		since query int false "Cursor devolvido na consulta anterior"
// @Success		200 {object} MapEnvelope
// @Router		/api/v1/events/{id}/seating/map [get]
func (h *Handler) seatMap(response http.ResponseWriter, request *http.Request) {
	since := int64(intQuery(request, "since", 0))
	view, err := h.seating.Map(request.Context(), pathID(request), since)
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, MapEnvelope{
		Data:     toMapSeats(view.Seats),
		Markers:  toMapMarkers(view.Markers),
		Version:  view.Version,
		Complete: view.Complete,
	})
}

type AvailabilityResponse struct {
	TicketID  string `json:"ticketId"`
	Available int    `json:"available"`
	Total     int    `json:"total"`
}

type AvailabilityEnvelope struct {
	Data []AvailabilityResponse `json:"data"`
}

// @Summary		Disponibilidade por setor
// @Description	Quantos assentos de cada lote estão livres, que é o número mostrado acima de cada setor na escolha.
// @Tags			Assentos
// @Produce		json
// @Param		id path string true "ID do evento"
// @Success		200 {object} AvailabilityEnvelope
// @Router		/api/v1/events/{id}/seating/availability [get]
func (h *Handler) availability(response http.ResponseWriter, request *http.Request) {
	tallies, err := h.seating.Availability(request.Context(), pathID(request))
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	data := make([]AvailabilityResponse, 0, len(tallies))
	for _, tally := range tallies {
		data = append(data, AvailabilityResponse{
			TicketID:  tally.TicketID,
			Available: tally.Available,
			Total:     tally.Total,
		})
	}
	httpx.WriteJSON(response, http.StatusOK, AvailabilityEnvelope{Data: data})
}

// @Summary		Melhor disponível
// @Description	Escolhe a melhor sequência de assentos vizinhos que couber no pedido. É o caminho principal e não um atalho: a maioria não quer estudar um mapa, quer quatro lugares juntos, perto da frente, agora, e é também o que dá a quem usa leitor de tela uma forma real de comprar. Devolve vazio em vez de separar o grupo em fileiras diferentes.
// @Tags			Assentos
// @Produce		json
// @Param		id path string true "ID do evento"
// @Param		ticketId query string false "Restringir a um lote"
// @Param		quantity query int true "Quantos assentos"
// @Param		accessible query bool false "Pedir assentos acessíveis"
// @Success		200 {object} MapEnvelope
// @Router		/api/v1/events/{id}/seating/best [get]
func (h *Handler) bestAvailable(response http.ResponseWriter, request *http.Request) {
	accessible, _ := strconv.ParseBool(request.URL.Query().Get("accessible"))
	seats, err := h.seating.BestAvailable(request.Context(), usecase.BestAvailableInput{
		EventID:    pathID(request),
		TicketID:   strings.TrimSpace(request.URL.Query().Get("ticketId")),
		Quantity:   intQuery(request, "quantity", 0),
		Accessible: accessible,
	})
	if err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	httpx.WriteJSON(response, http.StatusOK, MapEnvelope{Data: toMapSeats(seats), Complete: true})
}

// --- plumbing ---------------------------------------------------------------

func toMapSeats(seats []usecase.SeatView) []MapSeatResponse {
	out := make([]MapSeatResponse, 0, len(seats))
	for _, seat := range seats {
		out = append(out, MapSeatResponse{
			ID:        seat.ID,
			TicketID:  seat.TicketID,
			Section:   seat.Section,
			Row:       seat.Row,
			Seat:      seat.Seat,
			Kind:      string(seat.Kind),
			Status:    string(seat.Status),
			X:         seat.X,
			Y:         seat.Y,
			RowOrder:  seat.RowOrder,
			SeatOrder: seat.SeatOrder,
		})
	}
	return out
}

func toMapMarkers(markers []usecase.MarkerView) []MapMarkerResponse {
	if len(markers) == 0 {
		return nil
	}
	out := make([]MapMarkerResponse, 0, len(markers))
	for index := range markers {
		marker := &markers[index]
		out = append(out, MapMarkerResponse{
			TicketID: marker.TicketID,
			ID:       marker.ID,
			Name:     marker.Name,
			Kind:     string(marker.Kind),
			Capacity: marker.Capacity,
			X:        marker.X,
			Y:        marker.Y,
			Width:    marker.Width,
			Height:   marker.Height,
		})
	}
	return out
}

func toVenue(venue *domain.Venue) VenueResponse {
	return VenueResponse{ID: venue.ID, Name: venue.Name, Capacity: venue.Capacity}
}

func toLayout(layout *domain.Layout) LayoutResponse {
	return LayoutResponse{
		ID:            layout.ID,
		VenueID:       layout.VenueID,
		Name:          layout.Name,
		Version:       layout.Version,
		Status:        string(layout.Status),
		Frozen:        layout.Frozen,
		ViewBoxWidth:  layout.ViewBoxWidth,
		ViewBoxHeight: layout.ViewBoxHeight,
		SeatCount:     layout.SeatCount,
	}
}

// actor is who is asking, as the use case needs it. This package decides
// nothing about access.
func actor(request *http.Request) authdomain.Actor {
	return authdomain.ActorFromContext(request.Context())
}

func pathID(request *http.Request) string {
	return strings.TrimSpace(request.PathValue("id"))
}

func intQuery(request *http.Request, name string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(request.URL.Query().Get(name)))
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func decode(response http.ResponseWriter, request *http.Request, into any) bool {
	if err := json.NewDecoder(request.Body).Decode(into); err != nil {
		httpx.WriteError(response, http.StatusBadRequest,
			errors.New("request body must be a JSON object"))
		return false
	}
	return true
}

// ErrorResponse is the shape every failure here takes.
type ErrorResponse struct {
	Error string `json:"error"`
}

// statusFor maps failures onto HTTP.
func statusFor(err error) int {
	switch {
	case errors.Is(err, authdomain.ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, authdomain.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, eventdomain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrAlreadyMaterialised), errors.Is(err, domain.ErrSeatsSold):
		// A conflict with the state the room is already in, not a bad request:
		// the caller asked for something reasonable about a room that has moved
		// on, and retrying the same body will keep failing until they look.
		return http.StatusConflict
	case errors.Is(err, domain.ErrSeatsUnavailable):
		return http.StatusConflict
	case errors.Is(err, domain.ErrInvalidLabel),
		errors.Is(err, domain.ErrInvalidSection),
		errors.Is(err, domain.ErrInvalidSectionKnd),
		errors.Is(err, domain.ErrInvalidSeatKind),
		errors.Is(err, domain.ErrInvalidEvent),
		errors.Is(err, domain.ErrInvalidTicket),
		errors.Is(err, domain.ErrNoSeats),
		// A room with two sectors on the same floor. The request is well formed
		// and the room it describes is not one, which is what 422 is for.
		errors.As(err, &domain.ErrSectionsOverlap{}):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// AreaTicketsRequest assigns a dedicated ticket tier to each counted section ID.
type AreaTicketsRequest struct {
	TicketBySection map[string]string `json:"ticketBySection"`
}

// @Summary Configure individual admission for each standing area or box
// @Tags Assentos
// @Accept json
// @Produce json
// @Security BearerAuth
// @Param id path string true "Event ID"
// @Param request body AreaTicketsRequest true "Area ticket bindings"
// @Success 204
// @Failure 422 {object} ErrorResponse
// @Router /api/v1/events/{id}/seating/areas [put]
func (h *Handler) bindAreas(response http.ResponseWriter, request *http.Request) {
	var payload AreaTicketsRequest
	if !decode(response, request, &payload) {
		return
	}
	if payload.TicketBySection == nil {
		httpx.WriteError(response, http.StatusUnprocessableEntity, domain.ErrInvalidTicket)
		return
	}
	if err := h.seating.BindAreas(request.Context(), actor(request), pathID(request), payload.TicketBySection); err != nil {
		httpx.WriteError(response, statusFor(err), err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}
