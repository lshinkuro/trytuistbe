// Package server merangkai semua modul menjadi satu http.Handler.
//
// (Di plan, router ada di httpx/router.go; dipindah ke sini supaya httpx tidak
// meng-import modul auth/catalog/order/user yang juga meng-import httpx.)
package server

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/admin"
	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/auth"
	"trytuist-api/internal/catalog"
	"trytuist-api/internal/config"
	"trytuist-api/internal/content"
	"trytuist-api/internal/home"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
	"trytuist-api/internal/payment"
	"trytuist-api/internal/promo"
	"trytuist-api/internal/push"
	"trytuist-api/internal/token"
	"trytuist-api/internal/user"
)

// RequestTimeout: batas waktu per request.
const RequestTimeout = 30 * time.Second

// Option mengubah dependency router (dipakai tes).
type Option func(*options)

type options struct {
	gateway    payment.Gateway
	pushSender push.Sender
}

// WithPushSender: pakai sender ini (mis. fake APNs di tes).
func WithPushSender(s push.Sender) Option {
	return func(o *options) { o.pushSender = s }
}

// WithPaymentGateway: pakai gateway ini (mis. fake Midtrans di tes) walau MIDTRANS_SERVER_KEY kosong.
func WithPaymentGateway(gw payment.Gateway) Option {
	return func(o *options) { o.gateway = gw }
}

// App: router + service yang perlu jalan di background (sweeper pembayaran).
type App struct {
	Handler  http.Handler
	Payments *payment.Service
	Push     *push.Service
}

func NewRouter(cfg config.Config, pool *pgxpool.Pool, opts ...Option) http.Handler {
	return NewApp(cfg, pool, opts...).Handler
}

func NewApp(cfg config.Config, pool *pgxpool.Pool, opts ...Option) *App {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.gateway == nil && cfg.MidtransServerKey != "" {
		o.gateway = payment.NewHTTPGateway(cfg.MidtransServerKey, cfg.MidtransIsProduction)
	}
	online := o.gateway != nil
	if o.pushSender == nil && cfg.APNsKeyFile != "" {
		if p8, err := os.ReadFile(cfg.APNsKeyFile); err != nil {
			slog.Error("push: file APNS_KEY_FILE tidak bisa dibaca, push nonaktif", "err", err)
		} else if apns, err := push.NewAPNs(cfg.APNsKeyID, cfg.APNsTeamID, p8); err != nil {
			slog.Error("push: APNs tidak bisa dipakai, push nonaktif", "err", err)
		} else {
			o.pushSender = apns
		}
	}
	pushSvc := push.NewService(pool, o.pushSender)
	pushH := push.NewHandler(pushSvc)

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
	orderSvc := order.NewService(pool, order.Config{AppConfig: appConfig, Location: cfg.Timezone,
		OnlinePayments: online, CODDisabled: cfg.CODDisabled, Notifier: pushSvc})
	orderH := order.NewHandler(orderSvc)
	var gw payment.Gateway
	if online {
		gw = o.gateway
	}
	paymentSvc := payment.NewService(pool, orderSvc, gw, payment.Config{
		Env: cfg.Env, Expiry: cfg.PaymentExpiry, PublicBaseURL: cfg.PublicBaseURL,
		EnabledPayments: cfg.MidtransEnabledPayments, Location: cfg.Timezone, Notifier: pushSvc,
	})
	checkoutH := payment.NewHandler(paymentSvc)
	adminH := admin.NewHandler(pool, orderSvc, cfg.Timezone)
	homeH := home.NewHandler(pool, cfg.Timezone)
	promoH := promo.NewHandler(pool)
	userH := user.NewHandler(user.NewService(pool))
	paymentH := user.NewPaymentHandler(user.NewPaymentService(pool, appConfig,
		user.PaymentAvailability{Online: online, CODDisabled: cfg.CODDisabled}))
	contentH := content.NewHandler(content.NewRepository(pool), appConfig)

	r := chi.NewRouter()
	r.Use(httpx.RequestID, httpx.Logger, httpx.Recover, chimw.Timeout(RequestTimeout))
	r.NotFound(httpx.NotFoundHandler)
	r.MethodNotAllowed(httpx.MethodNotAllowedHandler)

	r.Get("/healthz", health(cfg, pool))

	// Midtrans (§5.27): publik, tanpa X-API-Key. Webhook divalidasi signature_key.
	r.Post("/v1/payments/midtrans/notification", checkoutH.Notification)
	r.Get("/v1/payments/finish", checkoutH.Finish)

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
		r.Get("/promotions/popup", catalogH.PopupDeal)
		r.Get("/app-config", appConfigH.Get)
		r.Get("/help", contentH.Help)
		r.Get("/legal/{slug}", contentH.Legal)
		// Tanpa login: dipanggil app setelah sesi dihapus (logout / sesi habis / cold start).
		r.Post("/devices/unregister", pushH.Unregister)

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

			r.Post("/checkout", checkoutH.Checkout)
			r.Get("/payments", checkoutH.List)
			r.Get("/payments/{id}", checkoutH.Get)
			r.Post("/payments/{id}/cancel", checkoutH.Cancel)

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
			r.Put("/me/devices", pushH.Register)
			r.Post("/subscriptions/{plan_id}/activate", promoH.Activate)

			if cfg.IsDev() {
				// Hanya dev: simulasi perubahan status pesanan oleh outlet/kurir.
				r.Post("/dev/orders/{id}/status", orderH.DevSetStatus)
				r.Post("/dev/push-test", pushH.DevTest)
			}
		})

		// Web admin (§5.25): login biasa + role ADMIN.
		r.Route("/admin", func(r chi.Router) {
			r.Use(httpx.Auth(tokens), admin.RequireAdmin(pool))
			r.Get("/me", adminH.Me)
			r.Get("/dashboard", adminH.Dashboard)
			r.Get("/orders", adminH.Orders)
			r.Get("/orders/{id}", adminH.Order)
			r.Patch("/orders/{id}/status", adminH.UpdateOrderStatus)
			r.Get("/products", adminH.Products)
			r.Post("/products", adminH.CreateProduct)
			r.Get("/products/{id}", adminH.Product)
			r.Patch("/products/{id}", adminH.UpdateProduct)
			r.Get("/deals", adminH.Deals)
			r.Post("/deals", adminH.CreateDeal)
			r.Patch("/deals/{id}", adminH.UpdateDeal)
			r.Delete("/deals/{id}", adminH.DeleteDeal)
			r.Get("/stores", adminH.Stores)
			r.Post("/stores", adminH.CreateStore)
			r.Patch("/stores/{id}", adminH.UpdateStore)
			r.Put("/stores/{id}/products/{product_id}", adminH.SetStoreStock)
			r.Get("/customers", adminH.Customers)
			r.Get("/audit-logs", adminH.AuditLogs)
		})
	})
	return &App{Handler: r, Payments: paymentSvc, Push: pushSvc}
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
