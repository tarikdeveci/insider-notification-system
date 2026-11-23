package contract_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/tarikdeveci/insider-notification-system/internal/api"
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return doc
}

func TestSpecIsValid(t *testing.T) {
	if err := loadSpec(t).Validate(context.Background()); err != nil {
		t.Fatalf("invalid spec: %v", err)
	}
}

func TestSpecMatchesRegisteredRoutes(t *testing.T) {
	doc := loadSpec(t)
	var inSpec []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			inSpec = append(inSpec, strings.ToUpper(method)+" "+path)
		}
	}
	inRouter := api.RoutePatterns()
	sort.Strings(inSpec)
	sort.Strings(inRouter)
	if strings.Join(inSpec, "\n") != strings.Join(inRouter, "\n") {
		t.Fatalf("spec and router differ\nspec:\n%s\nrouter:\n%s", strings.Join(inSpec, "\n"), strings.Join(inRouter, "\n"))
	}
}

func TestEveryOperationDocumentsProblemErrors(t *testing.T) {
	doc := loadSpec(t)
	for path, item := range doc.Paths.Map() {
		if strings.HasPrefix(path, "/v1/") {
			for method, op := range item.Operations() {
				if op.Responses.Status(400) == nil && op.Responses.Status(404) == nil && op.Responses.Status(422) == nil {
					t.Errorf("%s %s documents no error response", method, path)
				}
			}
		}
	}
}
