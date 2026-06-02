// Package apidocs embeds the OpenAPI specification into the binary so it can
// be served by Swagger UI without depending on the working directory.
package apidocs

import _ "embed"

//go:embed swagger.yaml
var SwaggerYAML []byte
