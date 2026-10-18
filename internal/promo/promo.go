// Package promo: kartu promo langganan di Profil (§5.20).
package promo

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

type Plan struct {
	ID       string
	Eyebrow  string
	Title    string
	Price    int64
	Period   string // DAILY | WEEKLY | MONTHLY
	ImageURL *string
}

// FeaturedPromotionDTO = FeaturedPromotionDTO di iOS.
type FeaturedPromotionDTO struct {
	PlanID      string  `json:"plan_id"`
	Eyebrow     string  `json:"eyebrow"`
	Title       string  `json:"title"`
	Price       int64   `json:"price"`
	Period      string  `json:"period"`
	ImageURL    *string `json:"image_url"`
	IsActive    bool    `json:"is_active"`
	ActiveUntil *string `json:"active_until"`
}

func toDTO(p Plan, activeUntil *time.Time) FeaturedPromotionDTO {
	return FeaturedPromotionDTO{
		PlanID: p.ID, Eyebrow: p.Eyebrow, Title: p.Title, Price: p.Price, Period: p.Period, ImageURL: p.ImageURL,
		IsActive: activeUntil != nil, ActiveUntil: httpx.ISOPtr(activeUntil),
	}
}

// NextActiveUntil: masa aktif satu periode dari t.
func NextActiveUntil(t time.Time, period string) time.Time {
	switch period {
	case "WEEKLY":
		return t.AddDate(0, 0, 7)
	case "MONTHLY":
		return t.AddDate(0, 1, 0)
	default: // DAILY
		return t.AddDate(0, 0, 1)
	}
}

const planColumns = `id, eyebrow, title, price, period, image_url`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	err := row.Scan(&p.ID, &p.Eyebrow, &p.Title, &p.Price, &p.Period, &p.ImageURL)
	return p, err
}

// activeUntil: akhir langganan aktif user untuk plan, nil bila tidak aktif.
func activeUntil(ctx context.Context, db database.DBTX, userID, planID string, now time.Time) (*time.Time, error) {
	if userID == "" {
		return nil, nil
	}
	var until time.Time
	err := db.QueryRow(ctx, `SELECT active_until FROM user_subscriptions WHERE user_id = $1 AND plan_id = $2`, userID, planID).Scan(&until)
	if database.IsNoRows(err) || (err == nil && !until.After(now)) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &until, nil
}

// ===== Handler =====

type Handler struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

func NewHandler(pool *pgxpool.Pool) *Handler { return &Handler{pool: pool, now: time.Now} }

// GET /v1/promotions/featured 🔒* — data: null bila tidak ada promo.
func (h *Handler) Featured(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := scanPlan(h.pool.QueryRow(ctx, `SELECT `+planColumns+` FROM subscription_plans
		WHERE is_featured AND is_active ORDER BY id LIMIT 1`))
	if database.IsNoRows(err) {
		httpx.JSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	until, err := activeUntil(ctx, h.pool, httpx.UserID(ctx), p.ID, h.now())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toDTO(p, until))
}

// POST /v1/subscriptions/{plan_id}/activate 🔒 → 201, 409 ALREADY_SUBSCRIBED bila masih aktif.
// Pembayaran belum ada; aktivasi langsung berhasil.
func (h *Handler) Activate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := httpx.UserID(ctx)
	planID := chi.URLParam(r, "plan_id")
	now := h.now()

	var plan Plan
	var until time.Time
	err := database.InTx(ctx, h.pool, func(tx pgx.Tx) error {
		var err error
		plan, err = scanPlan(tx.QueryRow(ctx, `SELECT `+planColumns+` FROM subscription_plans WHERE id = $1 AND is_active`, planID))
		if database.IsNoRows(err) {
			return httpx.NotFound("Paket langganan tidak ditemukan.")
		}
		if err != nil {
			return err
		}
		// Kunci baris user supaya dua tap "Aktifkan" bersamaan tidak sama-sama lolos.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, uid); err != nil {
			return err
		}
		current, err := activeUntil(ctx, tx, uid, plan.ID, now)
		if err != nil {
			return err
		}
		if current != nil {
			return httpx.Conflict("ALREADY_SUBSCRIBED", "Langganan ini masih aktif sampai "+current.In(httpx.Loc).Format("02/01/2006 15:04")+".")
		}
		until = NextActiveUntil(now, plan.Period)
		_, err = tx.Exec(ctx, `
			INSERT INTO user_subscriptions (user_id, plan_id, started_at, active_until) VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, plan_id) DO UPDATE SET started_at = EXCLUDED.started_at, active_until = EXCLUDED.active_until`,
			uid, plan.ID, now, until)
		return err
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusCreated, "Langganan aktif", toDTO(plan, &until))
}
