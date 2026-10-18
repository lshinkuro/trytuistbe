// Package server merangkai semua modul menjadi satu http.Handler.
//
// (Di plan, router ada di httpx/router.go; dipindah ke sini supaya httpx tidak
// meng-import modul auth/catalog/order/user yang juga meng-import httpx.)
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/auth"
	"trytuist-api/internal/catalog"
	"trytuist-api/internal/config"
	"trytuist-api/internal/content"
	"trytuist-api/internal/home"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
	"trytuist-api/internal/promo"
	"trytuist-api/internal/token"
	"trytuist-api/internal/user"
)

// RequestTimeout: batas waktu per request.
const RequestTimeout = 30 * time.Second

func NewRouter(cfg config.Config, pool *pgxpool.Pool) http.Handler {
	tokens := token.NewManager(cfg.JWTSecret, cfg.JWTAccessTTL, cfg.JWTRefreshTTL)

	google := auth.NewGoogleVerifier(cfg.GoogleClientIDs, cfg.GoogleJWKSURL)
	authH := auth.NewHandler(auth.NewService(pool, tokens, auth.WithGoogle(google)),
		httpx.NewRateLimiter(cfg.LoginRateLimit, time.Minute))
	catalogH := catalog.NewHandler(catalog.NewRepository(pool))
	// order_rules di tabel app_config; nilai env hanya cadangan bila key belum ada.
	appConfig := appconfig.NewRepository(pool, appconfig.OrderRules{
		MinQuantity: order.DefaultRules.MinQuantity, MaxQuantity: order.DefaultRules.MaxQuantity,
		MaxNoteLength: order.DefaultRules.NoteMaxChars,
		DeliveryFee:   cfg.DeliveryFee, FreeDeliveryMinSubtotal: cfg.FreeDeliveryMinSubtotal,
	})
	appConfigH := appconfig.NewHandler(appConfig)
	orderH := order.NewHandler(order.NewService(pool, order.Config{AppConfig: appConfig, Location: cfg.Timezone}))
	homeH := home.NewHandler(pool, cfg.Timezone)
	promoH := promo.NewHandler(pool)
	userH := user.NewHandler(user.NewService(pool))
	paymentH := user.NewPaymentHandler(user.NewPaymentService(pool, appConfig))
	contentH := content.NewHandler(content.NewRepository(pool), appConfig)

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Logger, httpx.Recover, chimw.Timeout(RequestTimeout))
	r.NotFound(httpx.NotFoundHandler)
	r.MethodNotAllowed(httpx.MethodNotAllowedHandler)

	r.Get("/healthz", health(cfg, pool))

	r.Route("/v1", func(r chi.Router) {
		r.Use(httpx.APIKey(cfg.APIKeys))

		r.Post("/auth/login", authH.Login) // rate limit di handler (per IP + email)
		r.Post("/auth/register", authH.Register)
		r.Post("/auth/refresh", authH.Refresh)
		r.Post("/auth/google", authH.Google) // daftar & masuk dengan Google (rate limit per IP)

		r.Get("/banners", catalogH.Banners)
		r.Get("/categories", catalogH.Categories)
		r.Get("/products", catalogH.ListProducts)
		r.Get("/products/{id}", catalogH.GetProduct)
		r.Get("/stores", catalogH.Stores)
		r.Get("/app-config", appConfigH.Get)
		r.Get("/help", contentH.Help)
		r.Get("/legal/{slug}", contentH.Legal)

		// 🔒* token opsional: tanpa token tetap 200, bagian personal kosong.
		r.Group(func(r chi.Router) {
			r.Use(httpx.OptionalAuth(tokens))
			r.Get("/home", homeH.Get)
			r.Get("/promotions/featured", promoH.Featured)
		})

		r.Group(func(r chi.Router) {
			r.Use(httpx.Auth(tokens)) // 🔒
			r.Post("/auth/logout", authH.Logout)

			r.Post("/orders/quote", orderH.Quote)
			r.Post("/orders", orderH.Create)
			r.Get("/orders", orderH.List)
			r.Get("/orders/{id}", orderH.Get)
			r.Post("/orders/{id}/cancel", orderH.Cancel)
			r.Post("/orders/{id}/rating", orderH.Rate)

			r.Get("/me", userH.Me)
			r.Patch("/me", userH.UpdateMe)
			r.Get("/me/addresses", userH.Addresses)
			r.Post("/me/addresses", userH.CreateAddress)
			r.Patch("/me/addresses/{id}", userH.UpdateAddress)
			r.Delete("/me/addresses/{id}", userH.DeleteAddress)
			r.Get("/me/favorites", userH.Favorites)
			r.Put("/me/favorites/{product_id}", userH.AddFavorite)
			r.Delete("/me/favorites/{product_id}", userH.RemoveFavorite)
			r.Get("/me/vouchers", userH.Vouchers)
			r.Get("/me/payment-methods", paymentH.List)
			r.Post("/me/payment-methods", paymentH.Add)
			r.Put("/me/payment-methods/default", paymentH.SetDefault)
			r.Delete("/me/payment-methods/{id}", paymentH.Remove)
			r.Post("/subscriptions/{plan_id}/activate", promoH.Activate)

			if cfg.IsDev() {
				// Hanya dev: simulasi perubahan status pesanan oleh outlet/kurir.
				r.Post("/dev/orders/{id}/status", orderH.DevSetStatus)
			}
		})
	})
	return r
}

func health(cfg config.Config, pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		status, db, code := "ok", "ok", http.StatusOK
		if err := pool.Ping(ctx); err != nil {
			status, db, code = "degraded", "error", http.StatusServiceUnavailable
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"status":"` + status + `","env":"` + cfg.Env + `","db":"` + db + `"}` + "\n"))
	}
}
