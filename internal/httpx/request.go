package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-playground/validator/v10"
)

// MaxBodyBytes: batas ukuran body request (1 MB).
const MaxBodyBytes = 1 << 20

var validate = newValidator()

func newValidator() *validator.Validate {
	v := validator.New(validator.WithRequiredStructEnabled())
	// Pakai nama field JSON (snake_case) di pesan error.
	v.RegisterTagNameFunc(func(f reflect.StructField) string {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			return ""
		}
		return name
	})
	return v
}

// Decode membaca body JSON ke dst lalu menjalankan validasi struct tag `validate`.
func Decode(r *http.Request, dst any) error {
	body := http.MaxBytesReader(nil, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(body)
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		var typeErr *json.UnmarshalTypeError
		switch {
		case errors.Is(err, io.EOF):
			return Validation("Body request kosong.")
		case errors.As(err, &maxErr):
			return NewError(http.StatusRequestEntityTooLarge, "VALIDATION_ERROR", "Body request terlalu besar (maks. 1 MB).")
		case errors.As(err, &typeErr):
			return Validation("Format data tidak valid.", FieldDetail{Field: typeErr.Field, Message: "tipe data salah"})
		default:
			return Validation("Format JSON tidak valid.")
		}
	}
	return Validate(dst)
}

// Validate menjalankan validator pada struct dan mengubah hasilnya jadi AppError.
func Validate(v any) error {
	err := validate.Struct(v)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return err
	}
	details := make([]FieldDetail, 0, len(verrs))
	for _, fe := range verrs {
		details = append(details, FieldDetail{Field: fieldPath(fe), Message: messageFor(fe)})
	}
	return Validation("Data yang dikirim tidak valid.", details...)
}

// fieldPath: "items[0].quantity" (tanpa nama struct root).
func fieldPath(fe validator.FieldError) string {
	ns := fe.Namespace()
	if _, rest, ok := strings.Cut(ns, "."); ok {
		return rest
	}
	return fe.Field()
}

func messageFor(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "wajib diisi"
	case "email":
		return "format email tidak valid"
	case "min":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("minimal %s karakter", fe.Param())
		}
		if fe.Kind() == reflect.Slice {
			return fmt.Sprintf("minimal %s item", fe.Param())
		}
		return fmt.Sprintf("minimal %s", fe.Param())
	case "max":
		if fe.Kind() == reflect.String {
			return fmt.Sprintf("maksimal %s karakter", fe.Param())
		}
		return fmt.Sprintf("maksimal %s", fe.Param())
	case "oneof":
		return "harus salah satu dari: " + strings.ReplaceAll(fe.Param(), " ", ", ")
	case "uuid":
		return "format id tidak valid"
	case "latitude", "longitude":
		return "koordinat tidak valid"
	case "url", "http_url":
		return "format URL tidak valid"
	default:
		return "tidak valid"
	}
}

// Page adalah parameter paginasi (?page=1&limit=20).
type Page struct {
	Page  int
	Limit int
}

func (p Page) Offset() int { return (p.Page - 1) * p.Limit }

func (p Page) Meta(total int) Meta { return Meta{Page: p.Page, Limit: p.Limit, Total: total} }

// ParsePage: default page=1, limit=defLimit, maksimal 100.
func ParsePage(q url.Values, defLimit int) (Page, error) {
	p := Page{Page: 1, Limit: defLimit}
	var details []FieldDetail
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			details = append(details, FieldDetail{Field: "page", Message: "harus angka ≥ 1"})
		} else {
			p.Page = n
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			details = append(details, FieldDetail{Field: "limit", Message: "harus angka 1–100"})
		} else {
			p.Limit = n
		}
	}
	if len(details) > 0 {
		return p, Validation("Parameter paginasi tidak valid.", details...)
	}
	return p, nil
}
