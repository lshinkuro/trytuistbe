package user

import (
	"context"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/catalog"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var (
	errUserNotFound    = httpx.Unauthorized("Akun tidak ditemukan. Silakan masuk lagi.")
	errAddressNotFound = httpx.NotFound("Alamat tidak ditemukan.")
)

type Service struct {
	pool *pgxpool.Pool
	repo Repository
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Me(ctx context.Context, userID string) (Profile, error) {
	p, err := s.repo.Profile(ctx, s.pool, userID)
	if database.IsNoRows(err) {
		return p, errUserNotFound
	}
	return p, err
}

func (s *Service) UpdateMe(ctx context.Context, userID string, patch ProfilePatch) (Profile, error) {
	if patch.Name != nil {
		n := strings.TrimSpace(*patch.Name)
		if n == "" {
			return Profile{}, httpx.Validation("Data yang dikirim tidak valid.", httpx.FieldDetail{Field: "name", Message: "wajib diisi"})
		}
		patch.Name = &n
	}
	if patch.Phone != nil && *patch.Phone != "" {
		// Nomor HP boleh dikosongkan (""), tapi bila diisi harus format Indonesia.
		phone := normalizePhone(*patch.Phone)
		if !ewalletNumberRe.MatchString(phone) {
			return Profile{}, httpx.Validation("Data yang dikirim tidak valid.",
				httpx.FieldDetail{Field: "phone", Message: "nomor HP tidak valid, contoh 081234567890"})
		}
		patch.Phone = &phone
	}
	if patch.AvatarURL != nil && *patch.AvatarURL != "" && !strings.HasPrefix(*patch.AvatarURL, "https://") {
		return Profile{}, httpx.Validation("Data yang dikirim tidak valid.",
			httpx.FieldDetail{Field: "avatar_url", Message: "harus URL https://"})
	}
	if err := s.repo.UpdateProfile(ctx, s.pool, userID, patch); err != nil {
		return Profile{}, err
	}
	return s.Me(ctx, userID)
}

func (s *Service) Addresses(ctx context.Context, userID string) ([]Address, error) {
	return s.repo.Addresses(ctx, s.pool, userID)
}

// CreateAddress: alamat pertama otomatis jadi default.
func (s *Service) CreateAddress(ctx context.Context, userID string, a Address) (Address, error) {
	var out Address
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		n, err := s.repo.CountAddresses(ctx, tx, userID)
		if err != nil {
			return err
		}
		if n == 0 {
			a.IsDefault = true
		}
		if n >= 20 {
			return httpx.Unprocessable("ADDRESS_LIMIT", "Maksimal 20 alamat tersimpan.")
		}
		if a.IsDefault {
			if err := s.repo.ClearDefault(ctx, tx, userID); err != nil {
				return err
			}
		}
		out, err = s.repo.InsertAddress(ctx, tx, userID, a)
		return err
	})
	return out, err
}

type AddressPatch struct {
	Label, Recipient, Phone, Address, Notes *string
	Lat, Lng                                *float64
	IsDefault                               *bool
}

func (s *Service) UpdateAddress(ctx context.Context, userID, id string, p AddressPatch) (Address, error) {
	if !uuidRe.MatchString(id) {
		return Address{}, errAddressNotFound
	}
	var out Address
	err := database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		a, err := s.repo.AddressForUpdate(ctx, tx, id, userID)
		if err != nil {
			if database.IsNoRows(err) {
				return errAddressNotFound
			}
			return err
		}
		var details []httpx.FieldDetail
		setText := func(field string, dst *string, v *string) {
			if v == nil {
				return
			}
			if t := strings.TrimSpace(*v); t == "" {
				details = append(details, httpx.FieldDetail{Field: field, Message: "wajib diisi"})
			} else {
				*dst = t
			}
		}
		setText("label", &a.Label, p.Label)
		setText("recipient", &a.Recipient, p.Recipient)
		setText("phone", &a.Phone, p.Phone)
		setText("address", &a.Address, p.Address)
		if len(details) > 0 {
			return httpx.Validation("Data yang dikirim tidak valid.", details...)
		}
		if p.Notes != nil {
			if t := strings.TrimSpace(*p.Notes); t == "" {
				a.Notes = nil
			} else {
				a.Notes = &t
			}
		}
		if p.Lat != nil {
			a.Lat = p.Lat
		}
		if p.Lng != nil {
			a.Lng = p.Lng
		}
		becameDefault := p.IsDefault != nil && *p.IsDefault && !a.IsDefault
		if p.IsDefault != nil {
			a.IsDefault = *p.IsDefault
		}
		if becameDefault {
			if err := s.repo.ClearDefault(ctx, tx, userID); err != nil {
				return err
			}
		}
		if out, err = s.repo.SaveAddress(ctx, tx, userID, a); err != nil {
			return err
		}
		if !out.IsDefault {
			// Selalu ada satu alamat default selama user punya alamat.
			if err := s.repo.PromoteNewestDefault(ctx, tx, userID); err != nil {
				return err
			}
			out, err = s.repo.AddressForUpdate(ctx, tx, id, userID)
		}
		return err
	})
	return out, err
}

func (s *Service) DeleteAddress(ctx context.Context, userID, id string) error {
	if !uuidRe.MatchString(id) {
		return errAddressNotFound
	}
	return database.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := s.repo.AddressForUpdate(ctx, tx, id, userID); err != nil {
			if database.IsNoRows(err) {
				return errAddressNotFound
			}
			return err
		}
		if err := s.repo.DeleteAddress(ctx, tx, id, userID); err != nil {
			return err
		}
		return s.repo.PromoteNewestDefault(ctx, tx, userID)
	})
}

// Favorites: harga & stok mengikuti storeID (opsional, §5.29).
func (s *Service) Favorites(ctx context.Context, userID, storeID string) ([]catalog.Product, error) {
	cat := catalog.NewRepository(s.pool)
	store, err := cat.StoreFromQuery(ctx, storeID)
	if err != nil {
		return nil, err
	}
	id := ""
	if store != nil {
		id = store.ID
	}
	return cat.FavoriteProducts(ctx, userID, id)
}

func (s *Service) AddFavorite(ctx context.Context, userID, productID string) error {
	ok, err := s.repo.ProductExists(ctx, s.pool, productID)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.NotFound("Produk tidak ditemukan.")
	}
	return s.repo.AddFavorite(ctx, s.pool, userID, productID)
}

func (s *Service) RemoveFavorite(ctx context.Context, userID, productID string) error {
	return s.repo.RemoveFavorite(ctx, s.pool, userID, productID)
}

func (s *Service) Vouchers(ctx context.Context, userID string) ([]Voucher, error) {
	return s.repo.Vouchers(ctx, s.pool, userID)
}
