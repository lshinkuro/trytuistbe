// Package content: konten statis yang diatur dari backend —
// Pusat Bantuan (GET /v1/help, §5.22) dan dokumen legal (GET /v1/legal/{slug}, §5.23).
// Keduanya publik (cukup API key) dan boleh di-cache.
package content

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// ===== Model =====

type Article struct {
	ID        string
	TopicID   string
	Question  string
	Answer    string
	UpdatedAt time.Time
}

type Topic struct {
	ID       string
	Title    string
	Icon     string
	Articles []Article
}

type Section struct {
	Heading string `json:"heading"`
	Body    string `json:"body"`
}

type LegalDocument struct {
	Slug        string
	Title       string
	Version     string
	Summary     *string
	Sections    []Section
	EffectiveAt time.Time
	UpdatedAt   time.Time
}

// ===== Repository =====

type Repository struct {
	db database.DBTX
}

func NewRepository(db database.DBTX) *Repository { return &Repository{db: db} }

// Topics: topik aktif beserta artikelnya. query kosong = semua; selain itu hanya
// artikel yang pertanyaan/jawabannya mengandung query (topik tanpa hasil dibuang).
func (r *Repository) Topics(ctx context.Context, query string) ([]Topic, error) {
	rows, err := r.db.Query(ctx, `
		SELECT t.id, t.title, t.icon, a.id, a.question, a.answer, a.updated_at
		FROM help_topics t
		JOIN help_articles a ON a.topic_id = t.id AND a.is_active
		WHERE t.is_active
		  AND ($1 = '' OR a.question ILIKE '%' || $1 || '%' OR a.answer ILIKE '%' || $1 || '%')
		ORDER BY t.sort_order, t.id, a.sort_order, a.id`, escapeLike(query))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	topics := []Topic{}
	index := map[string]int{}
	for rows.Next() {
		var t Topic
		var a Article
		if err := rows.Scan(&t.ID, &t.Title, &t.Icon, &a.ID, &a.Question, &a.Answer, &a.UpdatedAt); err != nil {
			return nil, err
		}
		a.TopicID = t.ID
		i, ok := index[t.ID]
		if !ok {
			i = len(topics)
			index[t.ID] = i
			topics = append(topics, t)
		}
		topics[i].Articles = append(topics[i].Articles, a)
	}
	return topics, rows.Err()
}

func (r *Repository) Legal(ctx context.Context, slug string) (LegalDocument, error) {
	var d LegalDocument
	var raw []byte
	err := r.db.QueryRow(ctx, `
		SELECT slug, title, version, summary, sections, effective_at, updated_at
		FROM legal_documents WHERE slug = $1`, slug).
		Scan(&d.Slug, &d.Title, &d.Version, &d.Summary, &raw, &d.EffectiveAt, &d.UpdatedAt)
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(raw, &d.Sections); err != nil {
		return d, err
	}
	return d, nil
}

// escapeLike: % dan _ dari user dianggap huruf biasa, bukan wildcard.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ===== DTO =====

type ArticleDTO struct {
	ID        string `json:"id"`
	Question  string `json:"question"`
	Answer    string `json:"answer"`
	UpdatedAt string `json:"updated_at"`
}

type TopicDTO struct {
	ID       string       `json:"id"`
	Title    string       `json:"title"`
	Icon     string       `json:"icon"`
	Articles []ArticleDTO `json:"articles"`
}

type ContactDTO struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	Value string `json:"value"`
	Icon  string `json:"icon"`
	URL   string `json:"url"`
}

type HelpCenterDTO struct {
	Query          *string      `json:"query"`
	Topics         []TopicDTO   `json:"topics"`
	Contacts       []ContactDTO `json:"contacts"`
	OperatingHours string       `json:"operating_hours"`
}

type LegalDocumentDTO struct {
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Version     string    `json:"version"`
	Summary     *string   `json:"summary"`
	Sections    []Section `json:"sections"`
	EffectiveAt string    `json:"effective_at"` // YYYY-MM-DD
	UpdatedAt   string    `json:"updated_at"`
}

// ===== Handler =====

type Handler struct {
	repo      *Repository
	appConfig *appconfig.Repository
}

func NewHandler(repo *Repository, appConfig *appconfig.Repository) *Handler {
	return &Handler{repo: repo, appConfig: appConfig}
}

// MaxQueryLength: batas panjang kata kunci pencarian bantuan.
const MaxQueryLength = 100

// GET /v1/help?q= — topik + FAQ + kontak. q opsional untuk mencari pertanyaan.
func (h *Handler) Help(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) > MaxQueryLength {
		httpx.FromError(w, r, httpx.Validation("Parameter tidak valid.",
			httpx.FieldDetail{Field: "q", Message: "maksimal 100 karakter"}))
		return
	}
	topics, err := h.repo.Topics(r.Context(), q)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	contacts, err := h.appConfig.HelpContacts(r.Context())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}

	out := HelpCenterDTO{Topics: make([]TopicDTO, len(topics)), Contacts: make([]ContactDTO, len(contacts.Channels)),
		OperatingHours: contacts.OperatingHours}
	if q != "" {
		out.Query = &q
	}
	for i, t := range topics {
		dto := TopicDTO{ID: t.ID, Title: t.Title, Icon: t.Icon, Articles: make([]ArticleDTO, len(t.Articles))}
		for j, a := range t.Articles {
			dto.Articles[j] = ArticleDTO{ID: a.ID, Question: a.Question, Answer: a.Answer, UpdatedAt: httpx.ISO(a.UpdatedAt)}
		}
		out.Topics[i] = dto
	}
	for i, c := range contacts.Channels {
		out.Contacts[i] = ContactDTO(c)
	}
	if q == "" {
		w.Header().Set("Cache-Control", "max-age=300")
	}
	httpx.JSON(w, http.StatusOK, out)
}

// GET /v1/legal/{slug} — 'privacy-policy' atau 'terms'.
func (h *Handler) Legal(w http.ResponseWriter, r *http.Request) {
	doc, err := h.repo.Legal(r.Context(), chi.URLParam(r, "slug"))
	if err == pgx.ErrNoRows || database.IsNoRows(err) {
		httpx.FromError(w, r, httpx.NotFound("Dokumen tidak ditemukan."))
		return
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "max-age=3600")
	httpx.JSON(w, http.StatusOK, LegalDocumentDTO{
		Slug: doc.Slug, Title: doc.Title, Version: doc.Version, Summary: doc.Summary, Sections: doc.Sections,
		EffectiveAt: doc.EffectiveAt.Format("2006-01-02"), UpdatedAt: httpx.ISO(doc.UpdatedAt),
	})
}
