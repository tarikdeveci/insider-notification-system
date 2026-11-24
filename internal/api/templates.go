package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
	tpl "github.com/tarikdeveci/insider-notification-system/internal/template"
)

const (
	maxTemplateBody    = 64 << 10
	maxTemplateName    = 100
	maxTemplateText    = 10000
	maxTemplateList    = 100
	defaultTemplateLen = 50
)

type templateRequest struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
	Body    string `json:"body"`
}

type templateDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Channel   string    `json:"channel"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type templateListResponse struct {
	Items []templateDTO `json:"items"`
}

func toTemplateDTO(t domain.Template) templateDTO {
	return templateDTO{ID: t.ID, Name: t.Name, Channel: string(t.Channel), Body: t.Body, CreatedAt: t.CreatedAt}
}

func (s *server) createTemplate(w http.ResponseWriter, r *http.Request) {
	var req templateRequest
	if err := decodeJSON(w, r, &req, maxTemplateBody); err != nil {
		writeRequestError(w, r, err)
		return
	}
	var errs []domain.FieldError
	bad := func(field, msg string) { errs = append(errs, domain.FieldError{Field: field, Message: msg}) }

	name := strings.TrimSpace(req.Name)
	if n := utf8.RuneCountInString(name); n < 1 || n > maxTemplateName {
		bad("name", "must be 1 to 100 characters")
	}
	ch := domain.Channel(req.Channel)
	if !ch.Valid() {
		bad("channel", "must be one of sms, email, push")
	}
	if n := utf8.RuneCountInString(req.Body); n < 1 || n > maxTemplateText {
		bad("body", "must be 1 to 10000 characters")
	} else if _, err := tpl.Parse(req.Body); err != nil {
		bad("body", err.Error())
	}
	if len(errs) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed", errs)
		return
	}
	t, err := s.Store.CreateTemplate(r.Context(), name, ch, req.Body)
	switch {
	case errors.Is(err, store.ErrTemplateNameTaken):
		writeProblem(w, r, http.StatusConflict, "a template with this name already exists", nil)
	case err != nil:
		internalError(w, r, s.Logger, err)
	default:
		writeJSON(w, http.StatusCreated, toTemplateDTO(t))
	}
}

func (s *server) getTemplate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !domain.IsUUID(id) {
		writeProblem(w, r, http.StatusNotFound, "template not found", nil)
		return
	}
	t, err := s.Store.GetTemplate(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "template not found", nil)
	case err != nil:
		internalError(w, r, s.Logger, err)
	default:
		writeJSON(w, http.StatusOK, toTemplateDTO(t))
	}
}

func (s *server) listTemplates(w http.ResponseWriter, r *http.Request) {
	limit := defaultTemplateLen
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxTemplateList {
			writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed",
				[]domain.FieldError{{Field: "limit", Message: "must be an integer from 1 to 100"}})
			return
		}
		limit = n
	}
	ts, err := s.Store.ListTemplates(r.Context(), limit)
	if err != nil {
		internalError(w, r, s.Logger, err)
		return
	}
	resp := templateListResponse{Items: make([]templateDTO, 0, len(ts))}
	for _, t := range ts {
		resp.Items = append(resp.Items, toTemplateDTO(t))
	}
	writeJSON(w, http.StatusOK, resp)
}

// indexed returns fe tagged with a batch index, or fe itself when idx is nil.
func indexed(fe domain.FieldError, idx *int) domain.FieldError {
	fe.Index = idx
	return fe
}

func unknownTemplate(idx *int) domain.FieldError {
	return indexed(domain.FieldError{Field: "template_id", Message: "unknown template"}, idx)
}

// renderInto fills nn.Content from the template and checks the result. The
// errors name fields and variable names only, never values or rendered text.
func renderInto(nn *domain.NewNotification, t domain.Template, idx *int) []domain.FieldError {
	if t.Channel != nn.Channel {
		return []domain.FieldError{indexed(domain.FieldError{Field: "template_id", Message: "template channel is " + string(t.Channel)}, idx)}
	}
	text, missing := tpl.Render(t.Body, nn.TemplateVars)
	if len(missing) > 0 {
		return []domain.FieldError{indexed(domain.FieldError{Field: "template_vars", Message: "missing variables: " + strings.Join(missing, ", ")}, idx)}
	}
	if cerrs := domain.CheckRenderedContent(nn.Channel, text); len(cerrs) > 0 {
		for i := range cerrs {
			cerrs[i] = indexed(cerrs[i], idx)
		}
		return cerrs
	}
	nn.Content = text
	return nil
}

// renderBatch renders every template item of a batch in place, loading the
// distinct templates with one query. It returns the field errors of all items.
func (s *server) renderBatch(ctx context.Context, items []domain.NewNotification) ([]domain.FieldError, error) {
	var ids []string
	seen := map[string]bool{}
	for _, it := range items {
		if it.TemplateID != nil && !seen[*it.TemplateID] {
			seen[*it.TemplateID] = true
			ids = append(ids, *it.TemplateID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	found, err := s.Store.GetTemplates(ctx, ids)
	if err != nil {
		return nil, err
	}
	var errs []domain.FieldError
	for i := range items {
		if items[i].TemplateID == nil {
			continue
		}
		idx := i
		t, ok := found[*items[i].TemplateID]
		if !ok {
			errs = append(errs, unknownTemplate(&idx))
			continue
		}
		errs = append(errs, renderInto(&items[i], t, &idx)...)
	}
	return errs, nil
}
