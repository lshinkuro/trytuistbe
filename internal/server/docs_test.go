package server_test

import (
	"bufio"
	"bytes"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"trytuist-api/docs"
)

// TestOpenAPICoversRoutes: setiap route di router harus ada di docs/openapi.yaml (dan sebaliknya),
// supaya dokumentasi Swagger tidak tertinggal saat endpoint ditambah / dihapus.
func TestOpenAPICoversRoutes(t *testing.T) {
	e := setup(t) // APP_ENV=dev → route /v1/dev/* ikut terdaftar
	routes, ok := e.app.Handler.(chi.Routes)
	if !ok {
		t.Fatal("handler bukan chi.Routes")
	}
	inRouter := map[string]bool{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(strings.ReplaceAll(route, "/*/", "/"), "/")
		if strings.HasPrefix(route, "/docs") {
			return nil
		}
		inRouter[method+" "+route] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	pathRe := regexp.MustCompile(`^  (/\S*):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|post|put|patch|delete):\s*$`)
	inSpec := map[string]bool{}
	path := ""
	sc := bufio.NewScanner(bytes.NewReader(docs.Spec))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "components:") {
			break
		}
		if m := pathRe.FindStringSubmatch(line); m != nil {
			path = m[1]
		} else if m := methodRe.FindStringSubmatch(line); m != nil && path != "" {
			inSpec[strings.ToUpper(m[1])+" "+path] = true
		}
	}

	var missing, stale []string
	for r := range inRouter {
		if !inSpec[r] {
			missing = append(missing, r)
		}
	}
	for s := range inSpec {
		if !inRouter[s] {
			stale = append(stale, s)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing) > 0 {
		t.Errorf("route belum ada di docs/openapi.yaml:\n  %s", strings.Join(missing, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("path di docs/openapi.yaml tidak ada di router:\n  %s", strings.Join(stale, "\n  "))
	}
	if len(inSpec) < 60 {
		t.Fatalf("parser spec hanya menemukan %d operasi; format openapi.yaml berubah?", len(inSpec))
	}

	// /docs tersaji di dev.
	res := e.do("GET", "/docs/openapi.yaml", "", nil)
	if res.Status != 200 {
		t.Fatalf("/docs/openapi.yaml = %d", res.Status)
	}
}
