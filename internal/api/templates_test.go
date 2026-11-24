package api_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mkTemplate(t *testing.T, h http.Handler, name, channel, body string) map[string]any {
	t.Helper()
	req := fmt.Sprintf(`{"name":%q,"channel":%q,"body":%q}`, name, channel, body)
	rr := call(t, h, "POST", "/v1/templates", req, nil)
	if rr.Code != 201 {
		t.Fatalf("create template: %d %s", rr.Code, rr.Body)
	}
	return obj(t, rr)
}

func tplNotification(id string, vars string) string {
	return fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","template_id":%q,"template_vars":%s}`, id, vars)
}

// errFields returns "[index] field: message" strings of a 422 problem.
func errFields(t *testing.T, rr *httptest.ResponseRecorder) []string {
	t.Helper()
	var out []string
	errs, _ := obj(t, rr)["errors"].([]any)
	for _, e := range errs {
		m := e.(map[string]any)
		s := fmt.Sprintf("%v: %v", m["field"], m["message"])
		if m["index"] != nil {
			s = fmt.Sprintf("[%v] %s", m["index"], s)
		}
		out = append(out, s)
	}
	return out
}

func hasError(errs []string, want string) bool {
	for _, e := range errs {
		if strings.Contains(e, want) {
			return true
		}
	}
	return false
}

func TestTemplateCRUDEndpoints(t *testing.T) {
	h := newAPI(t)
	b := mkTemplate(t, h, "b-second", "email", "Hello {{ name }}")
	if b["id"] == "" || b["name"] != "b-second" || b["channel"] != "email" || b["body"] != "Hello {{ name }}" || b["created_at"] == nil {
		t.Fatalf("created: %v", b)
	}
	a := mkTemplate(t, h, "a-first", "sms", "Hi {{name}}")

	if rr := call(t, h, "POST", "/v1/templates", `{"name":"a-first","channel":"push","body":"x"}`, nil); rr.Code != 409 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
		t.Fatalf("duplicate name: %d %s", rr.Code, rr.Body)
	}

	rr := call(t, h, "GET", "/v1/templates/"+a["id"].(string), "", nil)
	if rr.Code != 200 || obj(t, rr)["name"] != "a-first" {
		t.Fatalf("get: %d %s", rr.Code, rr.Body)
	}
	for name, path := range map[string]string{"missing": "/v1/templates/3f2b8c1e-5a4d-4c6e-9b7a-1d2e3f4a5b6c", "malformed": "/v1/templates/not-a-uuid"} {
		if rr := call(t, h, "GET", path, "", nil); rr.Code != 404 || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/problem+json") {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}

	items := obj(t, call(t, h, "GET", "/v1/templates", "", nil))["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["name"] != "a-first" || items[1].(map[string]any)["name"] != "b-second" {
		t.Fatalf("list order: %v", items)
	}
	items = obj(t, call(t, h, "GET", "/v1/templates?limit=1", "", nil))["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "a-first" {
		t.Fatalf("limit=1: %v", items)
	}
	for _, q := range []string{"limit=0", "limit=101", "limit=abc", "limit=-1"} {
		if rr := call(t, h, "GET", "/v1/templates?"+q, "", nil); rr.Code != 422 {
			t.Errorf("%s: %d", q, rr.Code)
		}
	}
}

func TestTemplateCreateValidation(t *testing.T) {
	h := newAPI(t)
	long := strings.Repeat("a", 101)
	for name, c := range map[string]struct{ body, field string }{
		"blank name":       {`{"name":"  ","channel":"sms","body":"x"}`, "name"},
		"long name":        {fmt.Sprintf(`{"name":%q,"channel":"sms","body":"x"}`, long), "name"},
		"bad channel":      {`{"name":"n","channel":"fax","body":"x"}`, "channel"},
		"empty body":       {`{"name":"n","channel":"sms","body":""}`, "body"},
		"long body":        {fmt.Sprintf(`{"name":"n","channel":"sms","body":%q}`, strings.Repeat("a", 10001)), "body"},
		"unclosed":         {`{"name":"n","channel":"sms","body":"hi {{name"}`, "body"},
		"invalid name":     {`{"name":"n","channel":"sms","body":"hi {{a-b}}"}`, "body"},
		"all fields wrong": {`{"name":"","channel":"","body":""}`, "channel"},
	} {
		rr := call(t, h, "POST", "/v1/templates", c.body, nil)
		if rr.Code != 422 || !hasError(errFields(t, rr), c.field+":") {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body)
		}
	}
	for name, body := range map[string]string{"empty": ``, "array": `[]`, "unknown field": `{"name":"n","channel":"sms","body":"x","x":1}`, "trailing": `{"name":"n","channel":"sms","body":"x"}{}`} {
		if rr := call(t, h, "POST", "/v1/templates", body, nil); rr.Code != 400 {
			t.Errorf("%s: %d", name, rr.Code)
		}
	}
	big := `{"name":"n","channel":"sms","body":"` + strings.Repeat("a", 70<<10) + `"}`
	if rr := call(t, h, "POST", "/v1/templates", big, nil); rr.Code != 413 {
		t.Errorf("oversize: %d", rr.Code)
	}
}

func TestTemplateCreateStoresRenderedContent(t *testing.T) {
	h := newAPI(t)
	tpl := mkTemplate(t, h, "welcome", "sms", "Hi {{ name }}, code {{code}}. Bye {{name}}")
	id := tpl["id"].(string)
	rr := call(t, h, "POST", "/v1/notifications", tplNotification(id, `{"name":"Ada","code":"42","extra":"x"}`), nil)
	if rr.Code != 201 {
		t.Fatalf("create: %d %s", rr.Code, rr.Body)
	}
	m := obj(t, rr)
	if m["content"] != "Hi Ada, code 42. Bye Ada" || m["template_id"] != id {
		t.Fatalf("create body: %v", m)
	}
	if v := m["template_vars"].(map[string]any); v["name"] != "Ada" || v["extra"] != "x" {
		t.Fatalf("vars: %v", m["template_vars"])
	}
	got := obj(t, call(t, h, "GET", "/v1/notifications/"+m["id"].(string), "", nil))
	if got["content"] != m["content"] || got["template_id"] != id || got["template_vars"] == nil {
		t.Fatalf("get: %v", got)
	}
	list := obj(t, call(t, h, "GET", "/v1/notifications", "", nil))["items"].([]any)
	if list[0].(map[string]any)["template_id"] != id {
		t.Fatalf("list: %v", list[0])
	}

	plain := obj(t, call(t, h, "POST", "/v1/notifications", smsBody, nil))
	if v, ok := plain["template_id"]; !ok || v != nil {
		t.Fatalf("content notification template_id must be null: %v", plain)
	}
	if v, ok := plain["template_vars"]; !ok || v != nil {
		t.Fatalf("content notification template_vars must be null: %v", plain)
	}
}

func TestRenderMissingVariableIs422(t *testing.T) {
	h := newAPI(t)
	id := mkTemplate(t, h, "t", "sms", "{{b}} {{a}} {{c}}")["id"].(string)
	rr := call(t, h, "POST", "/v1/notifications", tplNotification(id, `{"c":"SECRETVALUE"}`), nil)
	if rr.Code != 422 {
		t.Fatalf("code %d: %s", rr.Code, rr.Body)
	}
	errs := errFields(t, rr)
	if !hasError(errs, "template_vars: missing variables: a, b") {
		t.Fatalf("errors: %v", errs)
	}
	if strings.Contains(rr.Body.String(), "SECRETVALUE") {
		t.Error("problem body must not echo variable values")
	}
	// absent template_vars behaves like an empty object
	rr = call(t, h, "POST", "/v1/notifications", fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","template_id":%q}`, id), nil)
	if rr.Code != 422 || !hasError(errFields(t, rr), "missing variables: a, b, c") {
		t.Fatalf("no vars: %d %s", rr.Code, rr.Body)
	}
}

func TestRenderedContentValidatedWithChannelLimits(t *testing.T) {
	h := newAPI(t)
	id := mkTemplate(t, h, "t", "sms", "x{{v}}")["id"].(string)
	ok := call(t, h, "POST", "/v1/notifications", tplNotification(id, fmt.Sprintf(`{"v":%q}`, strings.Repeat("ş", 159))), nil)
	if ok.Code != 201 {
		t.Fatalf("160 runes: %d %s", ok.Code, ok.Body)
	}
	rr := call(t, h, "POST", "/v1/notifications", tplNotification(id, fmt.Sprintf(`{"v":%q}`, strings.Repeat("ş", 160))), nil)
	if rr.Code != 422 || !hasError(errFields(t, rr), "content: rendered content: exceeds the limit for this channel") {
		t.Fatalf("161 runes: %d %s", rr.Code, rr.Body)
	}
	blank := mkTemplate(t, h, "blank", "sms", "{{v}}")["id"].(string)
	if rr := call(t, h, "POST", "/v1/notifications", tplNotification(blank, `{"v":"  "}`), nil); rr.Code != 422 || !hasError(errFields(t, rr), "rendered content: is required") {
		t.Fatalf("blank render: %d %s", rr.Code, rr.Body)
	}
}

func TestTemplateChannelMismatchIs422(t *testing.T) {
	h := newAPI(t)
	id := mkTemplate(t, h, "mail", "email", "Hi")["id"].(string)
	rr := call(t, h, "POST", "/v1/notifications", tplNotification(id, `{}`), nil)
	if rr.Code != 422 || !hasError(errFields(t, rr), "template_id: template channel is email") {
		t.Fatalf("mismatch: %d %s", rr.Code, rr.Body)
	}
}

func TestUnknownTemplateIs422(t *testing.T) {
	h := newAPI(t)
	missing := "3f2b8c1e-5a4d-4c6e-9b7a-1d2e3f4a5b6c"
	rr := call(t, h, "POST", "/v1/notifications", tplNotification(missing, `{}`), nil)
	if rr.Code != 422 || !hasError(errFields(t, rr), "template_id: unknown template") {
		t.Fatalf("single: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/v1/notifications", tplNotification("nope", `{}`), nil); rr.Code != 422 {
		t.Fatalf("malformed id: %d", rr.Code)
	}
	good := mkTemplate(t, h, "ok", "sms", "Hi")["id"].(string)
	batch := fmt.Sprintf(`{"notifications":[%s,%s,%s]}`, smsBody, tplNotification(good, `{}`), tplNotification(missing, `{}`))
	rr = call(t, h, "POST", "/v1/notifications/batch", batch, nil)
	if rr.Code != 422 || !hasError(errFields(t, rr), "[2] template_id: unknown template") {
		t.Fatalf("batch: %d %s", rr.Code, rr.Body)
	}
}

func TestBatchTemplateItemsAreRenderedAndAtomic(t *testing.T) {
	h := newAPI(t)
	id := mkTemplate(t, h, "t", "sms", "Hi {{name}}")["id"].(string)
	batch := fmt.Sprintf(`{"notifications":[%s,%s,%s]}`, tplNotification(id, `{"name":"Ada"}`), smsBody, tplNotification(id, `{"name":"Bob"}`))
	rr := call(t, h, "POST", "/v1/notifications/batch", batch, nil)
	if rr.Code != 201 {
		t.Fatalf("batch: %d %s", rr.Code, rr.Body)
	}
	ids := obj(t, rr)["notification_ids"].([]any)
	want := []string{"Hi Ada", "hello", "Hi Bob"}
	for i, nid := range ids {
		got := obj(t, call(t, h, "GET", "/v1/notifications/"+nid.(string), "", nil))
		if got["content"] != want[i] {
			t.Errorf("item %d content %v", i, got["content"])
		}
	}
	if got := obj(t, call(t, h, "GET", "/v1/notifications/"+ids[0].(string), "", nil)); got["template_id"] != id {
		t.Errorf("template_id: %v", got["template_id"])
	}

	bad := fmt.Sprintf(`{"notifications":[%s,%s]}`, tplNotification(id, `{"name":"Ok"}`), tplNotification(id, `{}`))
	rr = call(t, h, "POST", "/v1/notifications/batch", bad, nil)
	if rr.Code != 422 || !hasError(errFields(t, rr), "[1] template_vars: missing variables: name") {
		t.Fatalf("missing var in batch: %d %s", rr.Code, rr.Body)
	}
	list := obj(t, call(t, h, "GET", "/v1/notifications", "", nil))["items"].([]any)
	if len(list) != 3 {
		t.Fatalf("a rejected batch must insert nothing, have %d rows", len(list))
	}
}

func TestTemplateReplayReturnsOriginal(t *testing.T) {
	h := newAPI(t)
	id := mkTemplate(t, h, "t", "sms", "Hi {{name}}")["id"].(string)
	key := map[string]string{"Idempotency-Key": "tpl-key"}
	body := tplNotification(id, `{"name":"Ada"}`)
	first := call(t, h, "POST", "/v1/notifications", body, key)
	if first.Code != 201 {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	again := call(t, h, "POST", "/v1/notifications", body, key)
	if again.Code != 200 || obj(t, again)["id"] != obj(t, first)["id"] || obj(t, again)["content"] != "Hi Ada" {
		t.Fatalf("replay: %d %s", again.Code, again.Body)
	}
	if rr := call(t, h, "POST", "/v1/notifications", tplNotification(id, `{"name":"Bob"}`), key); rr.Code != 409 {
		t.Fatalf("different vars same key: %d %s", rr.Code, rr.Body)
	}
	other := mkTemplate(t, h, "t2", "sms", "Hi {{name}}")["id"].(string)
	if rr := call(t, h, "POST", "/v1/notifications", tplNotification(other, `{"name":"Ada"}`), key); rr.Code != 409 {
		t.Fatalf("different template same key: %d", rr.Code)
	}
	// Equivalent literal content does not collide with the template request.
	if rr := call(t, h, "POST", "/v1/notifications", `{"recipient":"+905551234567","channel":"sms","content":"Hi Ada"}`, key); rr.Code != 409 {
		t.Fatalf("content vs template same key: %d", rr.Code)
	}

	bkey := map[string]string{"Idempotency-Key": "tpl-batch"}
	batch := fmt.Sprintf(`{"notifications":[%s]}`, body)
	b1 := call(t, h, "POST", "/v1/notifications/batch", batch, bkey)
	b2 := call(t, h, "POST", "/v1/notifications/batch", batch, bkey)
	if b1.Code != 201 || b2.Code != 200 || obj(t, b1)["batch_id"] != obj(t, b2)["batch_id"] {
		t.Fatalf("batch replay: %d %d", b1.Code, b2.Code)
	}
}

func TestTemplateRequestBoundsAreFieldErrors(t *testing.T) {
	h := newAPI(t)
	id := mkTemplate(t, h, "t", "sms", "Hi")["id"].(string)
	both := fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","content":"x","template_id":%q}`, id)
	if rr := call(t, h, "POST", "/v1/notifications", both, nil); rr.Code != 422 || !hasError(errFields(t, rr), "mutually exclusive") {
		t.Fatalf("both: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/v1/notifications", `{"recipient":"+905551234567","channel":"sms"}`, nil); rr.Code != 422 || !hasError(errFields(t, rr), "content or template_id is required") {
		t.Fatalf("neither: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/v1/notifications", `{"recipient":"+905551234567","channel":"sms","content":"x","template_vars":{"a":"b"}}`, nil); rr.Code != 422 {
		t.Fatalf("vars without id: %d", rr.Code)
	}
	// a non-string var value is a decoding error, not a field error
	if rr := call(t, h, "POST", "/v1/notifications", tplNotification(id, `{"a":1}`), nil); rr.Code != 400 {
		t.Fatalf("non-string var: %d", rr.Code)
	}
}

func TestTemplateVarsWithoutTemplateIDIs422(t *testing.T) {
	h := newAPI(t)
	for _, vars := range []string{`{}`, `{"a":"b"}`} {
		item := fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","content":"hello","template_vars":%s}`, vars)
		rr := call(t, h, "POST", "/v1/notifications", item, nil)
		if rr.Code != 422 || !hasError(errFields(t, rr), "template_vars: requires template_id") {
			t.Fatalf("single vars=%s: %d %s", vars, rr.Code, rr.Body)
		}
		batch := fmt.Sprintf(`{"notifications":[%s,%s]}`, smsBody, item)
		rr = call(t, h, "POST", "/v1/notifications/batch", batch, nil)
		if rr.Code != 422 || !hasError(errFields(t, rr), "[1] template_vars: requires template_id") {
			t.Fatalf("batch vars=%s: %d %s", vars, rr.Code, rr.Body)
		}
	}
	// JSON null is the same as absent.
	null := `{"recipient":"+905551234567","channel":"sms","content":"hello","template_vars":null}`
	if rr := call(t, h, "POST", "/v1/notifications", null, nil); rr.Code != 201 {
		t.Fatalf("null vars: %d %s", rr.Code, rr.Body)
	}
}

func TestBatchTemplateChannelMismatchIs422(t *testing.T) {
	h := newAPI(t)
	id := mkTemplate(t, h, "mail", "email", "Hi")["id"].(string)
	batch := fmt.Sprintf(`{"notifications":[%s,%s]}`, smsBody, tplNotification(id, `{}`))
	rr := call(t, h, "POST", "/v1/notifications/batch", batch, nil)
	if rr.Code != 422 || !hasError(errFields(t, rr), "[1] template_id: template channel is email") {
		t.Fatalf("batch mismatch: %d %s", rr.Code, rr.Body)
	}
	list := obj(t, call(t, h, "GET", "/v1/notifications", "", nil))["items"].([]any)
	if len(list) != 0 {
		t.Fatalf("a rejected batch must insert nothing, have %d rows", len(list))
	}
}
