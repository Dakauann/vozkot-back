package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	authdomain "vozkot/domain/auth"
	domainevent "vozkot/domain/event"
	"vozkot/domain/media"
	"vozkot/domain/ticket"
	userdomain "vozkot/domain/user"
	"vozkot/infra/config"
	"vozkot/infra/geocoding"
	"vozkot/infra/imaging"
	eventrepository "vozkot/infra/repositories/event"
	mediarepository "vozkot/infra/repositories/media"
	ticketrepository "vozkot/infra/repositories/ticket"
	"vozkot/infra/storage"
	eventusecase "vozkot/usecases/event"
	mediausecase "vozkot/usecases/media"
	ticketusecase "vozkot/usecases/ticket"

	"gorm.io/gorm"
)

const mockEventsPerCategory = 10

// The source covers stay with the seed executable so the same command works
// from a checkout, a compiled binary and the Docker build context.
//
//go:embed assets/events/*.png
var mockCovers embed.FS

type eventSeedSummary struct {
	EventsCreated  int
	EventsSkipped  int
	CoversAttached int
	CoversSkipped  int
	TiersCreated   int
	TiersUpdated   int
	TiersSkipped   int
}

type mockEvent struct {
	Name        string
	Description string
	Category    domainevent.Category
	Location    domainevent.Location
	StartsAt    time.Time
	EndsAt      time.Time
	CoverPath   string
	Tiers       []mockTier
}

type mockCategory struct {
	Category            domainevent.Category
	StartHour           int
	Duration            time.Duration
	BasePriceCents      int64
	HasFreeEvent        bool
	DescriptionTemplate string
	Titles              [mockEventsPerCategory]string
}

type mockTier struct {
	Title       string
	Description string
	PriceCents  int64
	Quantity    int
}

type mockVenue struct {
	Venue        string
	Address      string
	Neighborhood string
	City         string
	UF           string
	PostalCode   string
	Latitude     float64
	Longitude    float64
}

// seedMockEvents deliberately composes the same repositories, application
// services, image processor and storage adapter as the dashboard. A seeded
// cover therefore has the same object key, variants, blur placeholder,
// dominant colour and media row as a browser upload.
func seedMockEvents(
	ctx context.Context,
	db *gorm.DB,
	cfg config.Config,
	ownerID string,
	now time.Time,
) (eventSeedSummary, error) {
	var summary eventSeedSummary

	files, err := storage.New(ctx, cfg.Media)
	if err != nil {
		return summary, fmt.Errorf("open media storage: %w", err)
	}

	eventsRepository := eventrepository.NewEventRepository(db)
	ticketsRepository := ticketrepository.NewTicketRepository(db)
	mediaRepository := mediarepository.NewMediaRepository(db)
	mediaLibrary := mediausecase.NewService(
		mediaRepository,
		files,
		imaging.NewLibrary(cfg.MediaProcessors),
	)
	events := eventusecase.NewService(
		eventsRepository,
		ticketsRepository,
		mediaLibrary,
		geocoding.New(),
	)
	tiers := ticketusecase.NewService(ticketsRepository)

	coverData := make(map[string][]byte, len(domainevent.Categories()))
	currentCategory := domainevent.Category("")
	for _, draft := range mockEvents(now) {
		if draft.Category != currentCategory {
			currentCategory = draft.Category
			log.Printf("seeding mock category %s", currentCategory)
		}

		slug := domainevent.Slugify(draft.Name)
		item, findErr := eventsRepository.GetBySlug(ctx, slug)
		switch {
		case findErr == nil:
			// Slugs are the same durable addresses creation through the dashboard
			// produces. Matching both owner and name keeps a coincidental real
			// event from being claimed by this seed.
			if item.OwnerID != ownerID || item.Name != draft.Name {
				return summary, fmt.Errorf("slug %q is already used by another event", slug)
			}
			summary.EventsSkipped++
		case errors.Is(findErr, domainevent.ErrNotFound):
			endsAt := draft.EndsAt
			item, err = events.Create(ctx, eventusecase.CreateInput{
				OwnerID:     ownerID,
				Name:        draft.Name,
				Description: draft.Description,
				Category:    draft.Category,
				Location:    draft.Location,
				StartsAt:    draft.StartsAt,
				EndsAt:      &endsAt,
				Status:      domainevent.StatusPublished,
			})
			if err != nil {
				return summary, fmt.Errorf("create %q: %w", draft.Name, err)
			}
			summary.EventsCreated++
		default:
			return summary, fmt.Errorf("find %q: %w", draft.Name, findErr)
		}

		count, err := mediaRepository.CountByEventID(ctx, item.ID)
		if err != nil {
			return summary, fmt.Errorf("count covers for %q: %w", draft.Name, err)
		}
		if count > 0 {
			summary.CoversSkipped++
		} else {
			data := coverData[draft.CoverPath]
			if data == nil {
				data, err = mockCovers.ReadFile(draft.CoverPath)
				if err != nil {
					return summary, fmt.Errorf("read cover %q: %w", draft.CoverPath, err)
				}
				coverData[draft.CoverPath] = data
			}
			if _, err := events.AttachMedia(ctx, seedActor(ownerID), item.ID, media.Upload{
				FileName:    strings.TrimPrefix(draft.CoverPath, "assets/events/"),
				ContentType: "image/png",
				Data:        data,
			}); err != nil {
				return summary, fmt.Errorf("attach cover to %q: %w", draft.Name, err)
			}
			summary.CoversAttached++
		}

		existingTiers, err := ticketsRepository.List(ctx, ticket.Filter{EventID: item.ID})
		if err != nil {
			return summary, fmt.Errorf("list ticket tiers for %q: %w", draft.Name, err)
		}
		if err := reconcileMockTiers(ctx, tiers, ownerID, item.ID, draft.Tiers, existingTiers, &summary); err != nil {
			return summary, fmt.Errorf("seed ticket tiers for %q: %w", draft.Name, err)
		}
	}

	return summary, nil
}

// mockEvents returns dashboard-shaped drafts. Dates are relative to the next
// Saturday so a fresh development database always opens with an upcoming
// catalogue instead of fixtures that slowly age into the past.
func mockEvents(now time.Time) []mockEvent {
	zone := time.FixedZone("Brasilia", -3*60*60)
	localNow := now.In(zone)
	daysUntilSaturday := (int(time.Saturday) - int(localNow.Weekday()) + 7) % 7
	if daysUntilSaturday == 0 {
		daysUntilSaturday = 7
	}
	firstSaturday := time.Date(
		localNow.Year(),
		localNow.Month(),
		localNow.Day()+daysUntilSaturday,
		0,
		0,
		0,
		0,
		zone,
	)

	definitions := mockCategories()
	items := make([]mockEvent, 0, len(definitions)*mockEventsPerCategory)
	for categoryIndex, definition := range definitions {
		for eventIndex, title := range definition.Titles {
			venue := mockVenues[(categoryIndex+eventIndex)%len(mockVenues)]
			latitude, longitude := venue.Latitude, venue.Longitude
			day := firstSaturday.AddDate(0, 0, eventIndex*7+categoryIndex%7)
			startsAt := time.Date(
				day.Year(),
				day.Month(),
				day.Day(),
				definition.StartHour+eventIndex%2,
				0,
				0,
				0,
				zone,
			)
			items = append(items, mockEvent{
				Name:        title,
				Description: fmt.Sprintf(definition.DescriptionTemplate, title),
				Category:    definition.Category,
				Location: domainevent.Location{
					Venue:        venue.Venue,
					Address:      venue.Address,
					Neighborhood: venue.Neighborhood,
					City:         venue.City,
					UF:           venue.UF,
					PostalCode:   venue.PostalCode,
					Latitude:     &latitude,
					Longitude:    &longitude,
				},
				StartsAt:  startsAt,
				EndsAt:    startsAt.Add(definition.Duration),
				CoverPath: coverPath(definition.Category),
				Tiers:     mockTiers(definition, eventIndex),
			})
		}
	}
	return items
}

func mockTiers(category mockCategory, eventIndex int) []mockTier {
	quantity := 150 + eventIndex*25
	if category.HasFreeEvent && eventIndex == 0 {
		return []mockTier{
			{
				Title:       "Entrada gratuita",
				Description: "Reserva gratuita de acesso individual, sujeita à capacidade do espaço.",
				PriceCents:  0,
				Quantity:    quantity,
			},
			{
				Title:       "Ingresso solidário",
				Description: "Acesso individual com contribuição para apoiar a programação e os projetos parceiros.",
				PriceCents:  2000,
				Quantity:    100,
			},
			{
				Title:       "Acesso apoiador",
				Description: "Acesso individual para quem deseja contribuir com a continuidade do evento.",
				PriceCents:  4500,
				Quantity:    50,
			},
		}
	}

	fullPrice := category.BasePriceCents + int64(eventIndex*500)
	return []mockTier{
		{
			Title:       "Ingresso inteira: 1º lote",
			Description: "Acesso individual pelo primeiro lote de vendas. Apresente o ingresso digital na entrada.",
			PriceCents:  fullPrice,
			Quantity:    quantity,
		},
		{
			Title:       "Meia-entrada",
			Description: "Ingresso de meia-entrada mediante apresentação de comprovante válido na entrada.",
			PriceCents:  fullPrice / 2,
			Quantity:    80 + eventIndex*10,
		},
		{
			Title:       "Experiência premium",
			Description: "Acesso individual com entrada prioritária e área reservada do evento.",
			PriceCents:  fullPrice*2 + 2000,
			Quantity:    50 + eventIndex*5,
		},
	}
}

// seedActor is the identity the seeder acts under: the owner of the events it
// is seeding, and deliberately NOT an administrator.
//
// The seeder is the caller that proves the ownership check belongs in the use
// case rather than in the HTTP handler: it never touches one. Giving it an
// operator actor would have let it edit anybody's tiers, so it gets exactly the
// rights of the account it is seeding for.
func seedActor(ownerID string) authdomain.Actor {
	return authdomain.Actor{ID: ownerID, Role: userdomain.RoleUser}
}

func reconcileMockTiers(
	ctx context.Context,
	service *ticketusecase.Service,
	ownerID string,
	eventID string,
	desired []mockTier,
	existing []ticket.Ticket,
	summary *eventSeedSummary,
) error {
	// The earlier development seed wrote one generic tier. It is safe to
	// migrate only when it is the event's sole tier and carries one of those
	// exact seed titles; operator-created tiers are otherwise left alone.
	if len(existing) == 1 && isLegacyMockTier(existing[0].Title) && !hasDesiredTitle(desired, existing[0].Title) {
		updated, err := service.Update(ctx, seedActor(ownerID), existing[0].ID, ticketusecase.UpdateInput{
			Title:       desired[0].Title,
			Description: desired[0].Description,
			PriceCents:  desired[0].PriceCents,
			Quantity:    desired[0].Quantity,
			Status:      ticket.StatusOnSale,
		})
		if err != nil {
			return err
		}
		existing[0] = *updated
		summary.TiersUpdated++
	}

	byTitle := make(map[string]ticket.Ticket, len(existing))
	for _, tier := range existing {
		byTitle[tier.Title] = tier
	}
	for _, wanted := range desired {
		current, found := byTitle[wanted.Title]
		if !found {
			if _, err := service.Create(ctx, ticketusecase.CreateInput{
				OwnerID:     ownerID,
				EventID:     eventID,
				Title:       wanted.Title,
				Description: wanted.Description,
				PriceCents:  wanted.PriceCents,
				Quantity:    wanted.Quantity,
				Status:      ticket.StatusOnSale,
			}); err != nil {
				return err
			}
			summary.TiersCreated++
			continue
		}
		if current.Description == wanted.Description &&
			current.PriceCents == wanted.PriceCents &&
			current.Quantity == wanted.Quantity &&
			current.Status == ticket.StatusOnSale {
			summary.TiersSkipped++
			continue
		}
		if _, err := service.Update(ctx, seedActor(ownerID), current.ID, ticketusecase.UpdateInput{
			Title:       wanted.Title,
			Description: wanted.Description,
			PriceCents:  wanted.PriceCents,
			Quantity:    wanted.Quantity,
			Status:      ticket.StatusOnSale,
		}); err != nil {
			return err
		}
		summary.TiersUpdated++
	}
	return nil
}

func isLegacyMockTier(title string) bool {
	return title == "Entrada gratuita" || title == "Ingresso inteira"
}

func hasDesiredTitle(tiers []mockTier, title string) bool {
	for _, tier := range tiers {
		if tier.Title == title {
			return true
		}
	}
	return false
}

func coverPath(category domainevent.Category) string {
	return "assets/events/" + strings.ReplaceAll(string(category), "_", "-") + ".png"
}

var mockVenues = [...]mockVenue{
	{
		Venue: "Centro Cultural Beira-Mar", Address: "Av. Beira Mar, 2450",
		Neighborhood: "Meireles", City: "Fortaleza", UF: "CE", PostalCode: "60165-121",
		Latitude: -3.72555, Longitude: -38.49237,
	},
	{
		Venue: "Espaço Aurora", Address: "Rua Harmonia, 150",
		Neighborhood: "Vila Madalena", City: "São Paulo", UF: "SP", PostalCode: "05435-000",
		Latitude: -23.55434, Longitude: -46.68954,
	},
	{
		Venue: "Galpão da Marina", Address: "Av. Infante Dom Henrique, 85",
		Neighborhood: "Glória", City: "Rio de Janeiro", UF: "RJ", PostalCode: "20021-140",
		Latitude: -22.91747, Longitude: -43.17162,
	},
	{
		Venue: "Casa do Carmo", Address: "Rua do Carmo, 42",
		Neighborhood: "Santo Antônio Além do Carmo", City: "Salvador", UF: "BA", PostalCode: "40301-380",
		Latitude: -12.96793, Longitude: -38.50565,
	},
	{
		Venue: "Cais Criativo", Address: "Av. Alfredo Lisboa, 132",
		Neighborhood: "Recife Antigo", City: "Recife", UF: "PE", PostalCode: "50030-150",
		Latitude: -8.06238, Longitude: -34.87111,
	},
	{
		Venue: "Arena Horizonte", Address: "Av. dos Andradas, 3000",
		Neighborhood: "Santa Efigênia", City: "Belo Horizonte", UF: "MG", PostalCode: "30260-070",
		Latitude: -19.92147, Longitude: -43.91930,
	},
	{
		Venue: "Estação Cultural Sul", Address: "Rua Engenheiros Rebouças, 1000",
		Neighborhood: "Rebouças", City: "Curitiba", UF: "PR", PostalCode: "80215-100",
		Latitude: -25.44212, Longitude: -49.26719,
	},
	{
		Venue: "Armazém Guaíba", Address: "Av. Mauá, 1050",
		Neighborhood: "Centro Histórico", City: "Porto Alegre", UF: "RS", PostalCode: "90010-110",
		Latitude: -30.02770, Longitude: -51.22873,
	},
	{
		Venue: "Pavilhão do Cerrado", Address: "Eixo Monumental, lote 12",
		Neighborhood: "Zona Cívico-Administrativa", City: "Brasília", UF: "DF", PostalCode: "70070-350",
		Latitude: -15.79423, Longitude: -47.88217,
	},
	{
		Venue: "Parque Cultural das Mangabeiras", Address: "Rua Caraça, 900",
		Neighborhood: "Serra", City: "Belo Horizonte", UF: "MG", PostalCode: "30220-260",
		Latitude: -19.95038, Longitude: -43.90622,
	},
}

func mockCategories() []mockCategory {
	return []mockCategory{
		{
			Category: domainevent.CategoryFestasShows, StartHour: 20, Duration: 5 * time.Hour, BasePriceCents: 7500, HasFreeEvent: true,
			DescriptionTemplate: "%s traz uma noite vibrante de música brasileira, encontro e pista cheia. A programação combina artistas convidados, experiências visuais e estrutura completa para curtir do começo ao fim.",
			Titles: [mockEventsPerCategory]string{
				"Festival Brisa do Atlântico", "Noite Solar", "Sons da Praia", "Virada Tropical", "Forró na Lua",
				"Ritmos do Nordeste", "Baile das Cores", "Encontro do Mar", "Circuito Música da Cidade", "Festa Horizonte",
			},
		},
		{
			Category: domainevent.CategoryTeatrosEspetacs, StartHour: 19, Duration: 2*time.Hour + 30*time.Minute, BasePriceCents: 6500,
			DescriptionTemplate: "%s é um espetáculo cênico que aproxima histórias brasileiras do público com atuações intensas, direção cuidadosa e uma montagem envolvente. A sessão inclui abertura da casa com uma hora de antecedência.",
			Titles: [mockEventsPerCategory]string{
				"A Casa das Janelas Acesas", "Cartas para o Amanhã", "O Vento Entre Nós", "Memórias de um Quintal", "Depois da Chuva",
				"A Última Estação", "Entre Pontes e Silêncios", "Maré de Dentro", "O Tempo das Coisas", "Céu de Lona",
			},
		},
		{
			Category: domainevent.CategoryStandUp, StartHour: 20, Duration: 2 * time.Hour, BasePriceCents: 4500,
			DescriptionTemplate: "%s reúne humoristas da nova cena brasileira para uma noite de histórias, observações do cotidiano e muita improvisação. Chegue cedo para escolher um bom lugar e aproveitar a abertura da casa.",
			Titles: [mockEventsPerCategory]string{
				"Rindo à Toa", "Manual do Adulto Cansado", "Quase Tudo Sob Controle", "Plantão do Riso", "Café, Boleto e Terapia",
				"Foi Sem Querer", "Notificações Desativadas", "Só Mais Cinco Minutos", "Crônicas de Elevador", "A Culpa é do Wi-Fi",
			},
		},
		{
			Category: domainevent.CategoryCursosWorkshops, StartHour: 9, Duration: 4 * time.Hour, BasePriceCents: 9000, HasFreeEvent: true,
			DescriptionTemplate: "%s é uma experiência prática para aprender fazendo, trocar repertório e sair com ferramentas aplicáveis ao dia a dia. Materiais essenciais, acompanhamento de facilitadores e certificado digital estão incluídos.",
			Titles: [mockEventsPerCategory]string{
				"Oficina de Cerâmica para Iniciantes", "Fotografia de Rua na Prática", "Introdução à Marcenaria Criativa", "Escrita que Aproxima", "Horta Urbana em Pequenos Espaços",
				"Aquarela Botânica", "Finanças para Projetos Criativos", "Café Especial: do Grão à Xícara", "Costura Livre e Afetiva", "Design de Experiências Presenciais",
			},
		},
		{
			Category: domainevent.CategoryCongressos, StartHour: 9, Duration: 9 * time.Hour, BasePriceCents: 15000,
			DescriptionTemplate: "%s conecta especialistas, lideranças e profissionais em uma programação de palestras, painéis e conversas práticas. O encontro foi pensado para gerar repertório, relacionamento e novas possibilidades de colaboração.",
			Titles: [mockEventsPerCategory]string{
				"Fórum Brasileiro de Cidades Criativas", "Congresso de Inovação Humana", "Encontro Nacional de Economia Verde", "Conexões que Transformam", "Seminário Futuro do Trabalho",
				"Jornada de Liderança Inclusiva", "Fórum Nordeste de Tecnologia", "Simpósio de Comunicação Contemporânea", "Conferência Negócios com Propósito", "Encontro Brasileiro de Educação Digital",
			},
		},
		{
			Category: domainevent.CategoryEsportivo, StartHour: 6, Duration: 4 * time.Hour, BasePriceCents: 5000, HasFreeEvent: true,
			DescriptionTemplate: "%s convida atletas, iniciantes e famílias para uma manhã de movimento com percurso sinalizado, hidratação e apoio de equipe especializada. Uma celebração esportiva acessível para diferentes ritmos.",
			Titles: [mockEventsPerCategory]string{
				"Corrida Orla em Movimento", "Pedal das Pontes", "Circuito Praia Ativa", "Desafio Trilhas Urbanas", "Copa Comunidade de Vôlei",
				"Travessia Águas Abertas", "Festival de Skate da Cidade", "Caminhada Parque Vivo", "Encontro de Basquete de Rua", "Desafio Amanhecer 10K",
			},
		},
		{
			Category: domainevent.CategoryGastronomia, StartHour: 17, Duration: 5 * time.Hour, BasePriceCents: 8500,
			DescriptionTemplate: "%s celebra sabores brasileiros em uma rota de pratos autorais, ingredientes regionais e encontros com quem cozinha. O público poderá provar diferentes criações em um ambiente aberto e acolhedor.",
			Titles: [mockEventsPerCategory]string{
				"Sabores do Brasil", "Festival Fogo e Brasa", "Mesa do Mar", "Rota dos Queijos Artesanais", "Comida de Quintal",
				"Mercado das Especiarias", "Doce Encontro", "Cozinha das Águas", "Brunch Tropical", "Noite dos Botecos Criativos",
			},
		},
		{
			Category: domainevent.CategoryReligiao, StartHour: 8, Duration: 3 * time.Hour, BasePriceCents: 3000, HasFreeEvent: true,
			DescriptionTemplate: "%s oferece um tempo de pausa, escuta e conexão em uma vivência aberta a diferentes trajetórias espirituais. A programação reúne práticas contemplativas, música e conversas guiadas com respeito e acolhimento.",
			Titles: [mockEventsPerCategory]string{
				"Encontro Caminhos de Luz", "Manhã de Presença", "Círculo de Paz e Escuta", "Jornada Essência", "Retiro Urbano Respira",
				"Vozes da Esperança", "Celebração do Cuidado", "Silêncio que Acolhe", "Conexão e Propósito", "Festival da Harmonia",
			},
		},
		{
			Category: domainevent.CategoryPasseiosTours, StartHour: 8, Duration: 3*time.Hour + 30*time.Minute, BasePriceCents: 6000, HasFreeEvent: true,
			DescriptionTemplate: "%s revela histórias, sabores e detalhes da cidade em um percurso acompanhado por guia local. O passeio acontece em grupo reduzido, com paradas planejadas e ritmo confortável.",
			Titles: [mockEventsPerCategory]string{
				"Caminhos do Centro Histórico", "Rota das Artes Urbanas", "Passeio Sabores e Memórias", "Arquitetura ao Entardecer", "Jardins Secretos da Cidade",
				"Circuito Fotográfico da Orla", "Histórias do Mercado Antigo", "Pedal Patrimônio e Paisagem", "Rota dos Ateliês", "Caminhada Lendas e Casarões",
			},
		},
		{
			Category: domainevent.CategoryInfantil, StartHour: 10, Duration: 4 * time.Hour, BasePriceCents: 4000, HasFreeEvent: true,
			DescriptionTemplate: "%s é uma programação para crianças e pessoas cuidadoras brincarem juntas com conforto e segurança. Histórias, música, oficinas e espaços livres formam uma experiência leve para toda a família.",
			Titles: [mockEventsPerCategory]string{
				"Festival Pequenos Inventores", "Manhã no Reino das Histórias", "Brincadeira de Quintal", "Circo das Descobertas", "Clube dos Exploradores",
				"Música para Gente Pequena", "Oficina Monstros de Papel", "Piquenique das Cores", "Teatro no Jardim", "Férias no Mundo da Imaginação",
			},
		},
		{
			Category: domainevent.CategoryGamesGeek, StartHour: 11, Duration: 9 * time.Hour, BasePriceCents: 5500,
			DescriptionTemplate: "%s reúne comunidades de jogos, tecnologia e cultura geek em um dia de experiências interativas. A programação inclui mesas livres, campeonatos amistosos, criadores independentes e espaços para jogar em grupo.",
			Titles: [mockEventsPerCategory]string{
				"Arena Pixel Brasil", "Festival Dados e Dragões", "Conexão Indie Games", "Batalha dos Controles", "Universo Cosplay Criativo",
				"Maratona Tabuleiro Aberto", "Encontro Ficção e Fantasia", "Liga Retrô Arcade", "Campus Criadores de Jogos", "Noite RPG na Cidade",
			},
		},
		{
			Category: domainevent.CategoryModaBeleza, StartHour: 18, Duration: 4 * time.Hour, BasePriceCents: 8000,
			DescriptionTemplate: "%s apresenta novos olhares para moda, beleza e expressão pessoal com criadores independentes e propostas brasileiras. Desfiles, demonstrações e conversas aproximam o público dos processos por trás de cada trabalho.",
			Titles: [mockEventsPerCategory]string{
				"Passarela Brasil Autoral", "Beleza em Todas as Peles", "Encontro Moda Circular", "Novos Talentos da Costura", "Festival Cabelos e Identidade",
				"Mercado de Design Independente", "Semana Criativa de Estilo", "Laboratório de Maquiagem Natural", "Tramas do Brasil", "Conexão Moda e Futuro",
			},
		},
		{
			Category: domainevent.CategorySaudeBemEstar, StartHour: 7, Duration: 4 * time.Hour, BasePriceCents: 4500,
			DescriptionTemplate: "%s propõe uma manhã de cuidado integral com práticas acessíveis de movimento, respiração e bem-estar. Profissionais convidados orientam as atividades em um ambiente tranquilo, inclusivo e próximo da natureza.",
			Titles: [mockEventsPerCategory]string{
				"Manhã Equilíbrio no Parque", "Yoga ao Nascer do Sol", "Festival Vida em Movimento", "Respira Cidade", "Encontro Sono e Bem-Estar",
				"Jornada Alimentação Consciente", "Meditação para Começar", "Circuito Corpo Presente", "Saúde Integral na Prática", "Caminhos do Autocuidado",
			},
		},
		{
			Category: domainevent.CategoryArteCultura, StartHour: 16, Duration: 6 * time.Hour, BasePriceCents: 3500, HasFreeEvent: true,
			DescriptionTemplate: "%s ocupa espaços de convivência com artes visuais, música e experiências criadas por artistas brasileiros. A visita é livre, com mediações em horários selecionados e atividades para diferentes públicos.",
			Titles: [mockEventsPerCategory]string{
				"Mostra Horizontes Brasileiros", "Ocupação Arte em Trânsito", "Festival Luz de Dentro", "Circuito Ateliês Abertos", "Encontro Cultura de Rua",
				"Bienal das Pequenas Coisas", "Noite nos Museus", "Mostra Corpo e Território", "Festival Cinema de Bairro", "Arte, Som e Cidade",
			},
		},
		{
			Category: domainevent.CategoryPride, StartHour: 16, Duration: 7 * time.Hour, BasePriceCents: 4500, HasFreeEvent: true,
			DescriptionTemplate: "%s celebra orgulho, afeto e liberdade com uma programação feita para acolher a diversidade. Música, arte e encontros comunitários ocupam o espaço em um ambiente seguro, respeitoso e cheio de cor.",
			Titles: [mockEventsPerCategory]string{
				"Festival Orgulho de Ser", "Baile Todas as Cores", "Encontro Afeto Livre", "Parada Cultural Diversa", "Noite Brilha Juntes",
				"Mostra Vozes do Orgulho", "Piquenique Arco-Íris", "Circuito Arte sem Armários", "Celebração Amor em Movimento", "Festival Plural Brasil",
			},
		},
		{
			Category: domainevent.CategoryOutros, StartHour: 17, Duration: 5 * time.Hour, BasePriceCents: 3000,
			DescriptionTemplate: "%s mistura descobertas, encontros e experiências que escapam das categorias tradicionais. Uma programação diversa ocupa o espaço com projetos independentes, atividades participativas e boas surpresas.",
			Titles: [mockEventsPerCategory]string{
				"Mercado Criativo ao Entardecer", "Feira Trocas e Descobertas", "Encontro Fazedores da Cidade", "Festival Ideias Improváveis", "Clube das Pequenas Aventuras",
				"Noite de Experiências Secretas", "Laboratório Aberto da Cidade", "Domingo Fora da Caixa", "Encontro Coleções e Memórias", "Circuito Novos Hobbies",
			},
		},
	}
}
