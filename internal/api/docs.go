package api

import (
	_ "embed"
	"net/http"
)

// openapiSpec is the hand-maintained API contract, served as-is and rendered
// by the Swagger UI page below. Keeping it embedded means the binary is the
// single source of truth — no separate file to deploy or drift.
//
//go:embed openapi.yaml
var openapiSpec []byte

// swaggerUIPage renders Swagger UI from the jsDelivr CDN against our embedded
// spec. The page is tiny; the UI assets load from the CDN at view time.
const swaggerUIPage = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>cuanto_cuesta API</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: "/openapi.yaml",
        dom_id: "#swagger-ui",
      });
    };
  </script>
</body>
</html>`

// openapiYAML serves the raw OpenAPI document.
func (h *handlers) openapiYAML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	_, _ = w.Write(openapiSpec)
}

// docs serves the Swagger UI page.
func (h *handlers) docs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(swaggerUIPage))
}
