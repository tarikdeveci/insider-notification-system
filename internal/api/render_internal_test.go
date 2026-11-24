package api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
)

func discardLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func hostileTemplate() domain.Template {
	return domain.Template{ID: "t1", Channel: domain.ChannelSMS, Body: strings.Repeat("{{a}}", 100)}
}

func hostileItem() domain.NewNotification {
	tid := "t1"
	return domain.NewNotification{
		Channel:      domain.ChannelSMS,
		TemplateID:   &tid,
		TemplateVars: map[string]string{"a": strings.Repeat("x", 1000)},
	}
}

func TestRenderIntoRejectsOversizeBeforeRendering(t *testing.T) {
	nn := hostileItem()
	errs := renderInto(&nn, hostileTemplate(), nil)
	if len(errs) != 1 || errs[0].Field != "content" || !strings.Contains(errs[0].Message, "exceeds the limit") {
		t.Fatalf("errs: %+v", errs)
	}
	if nn.Content != "" {
		t.Fatal("content must stay empty")
	}
}

func TestRenderItemsStopsOnceDoomed(t *testing.T) {
	items := make([]domain.NewNotification, 500)
	for i := range items {
		items[i] = hostileItem()
	}
	errs, err := renderItems(context.Background(), items, map[string]domain.Template{"t1": hostileTemplate()})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != maxBatchFieldErrors {
		t.Fatalf("got %d errors, want the cap %d", len(errs), maxBatchFieldErrors)
	}
	if errs[0].Index == nil || *errs[0].Index != 0 {
		t.Fatalf("first error must carry item index 0: %+v", errs[0])
	}
}

func TestRenderItemsStopsWhenContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	items := []domain.NewNotification{hostileItem()}
	if _, err := renderItems(ctx, items, map[string]domain.Template{"t1": hostileTemplate()}); err == nil {
		t.Fatal("canceled context must stop rendering")
	}
}

func TestCreateTemplateRejectsBodyThatCanNeverFitChannel(t *testing.T) {
	h := NewServer(Deps{Logger: discardLog()})
	body := `{"name":"n","channel":"sms","body":"` + strings.Repeat("a", 161) + `"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/templates", strings.NewReader(body)))
	if rr.Code != 422 || !strings.Contains(rr.Body.String(), `"field":"body"`) {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
}

func TestCreateTemplateRejectsTooManyPlaceholders(t *testing.T) {
	h := NewServer(Deps{Logger: discardLog()})
	body := `{"name":"n","channel":"email","body":"` + strings.Repeat("{{a}}", 101) + `"}`
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/templates", strings.NewReader(body)))
	if rr.Code != 422 || !strings.Contains(rr.Body.String(), "too many placeholders") {
		t.Fatalf("%d %s", rr.Code, rr.Body)
	}
}
