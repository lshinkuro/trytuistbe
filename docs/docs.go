// Package docs: spesifikasi OpenAPI (openapi.yaml) + Swagger UI di /docs.
// Ubah openapi.yaml setiap menambah / mengubah endpoint; TestOpenAPICoversRoutes gagal bila ada route yang belum tertulis.
package docs

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.yaml
var Spec []byte

// Swagger UI dari CDN (versi dikunci), membaca spec dari /docs/openapi.yaml.
const page = `<!doctype html>
<html lang="id">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TryTuist API</title>
<link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui-bundle.js" crossorigin></script>
<script>
window.ui = SwaggerUIBundle({
  url: "/docs/openapi.yaml",
  dom_id: "#swagger-ui",
  persistAuthorization: true,
  tryItOutEnabled: true,
  displayRequestDuration: true,
  docExpansion: "none",
  filter: true
});
</script>
</body>
</html>`

func UI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(page))
}

func SpecYAML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(Spec)
}
