package integrations

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"AI-agent/internal/mistral"
	"AI-agent/internal/store"
)

type Options struct {
	GoogleClientID     string
	GoogleClientSecret string
	PublicBaseURL      string
	TokenEncryptionKey string
	Store              *store.Store
	HTTPClient         *http.Client
}

type Registry struct {
	google *googleOAuth
}

func NewRegistry(opts Options) (*Registry, error) {
	google, err := newGoogleOAuth(opts.GoogleClientID, opts.GoogleClientSecret, opts.PublicBaseURL, opts.TokenEncryptionKey, opts.Store, opts.HTTPClient)
	if err != nil {
		return nil, err
	}
	return &Registry{
		google: google,
	}, nil
}

func (r *Registry) GoogleConfigured() bool {
	return r.google.configured()
}

func (r *Registry) GoogleAuthURL(userID int64) (string, error) {
	return r.google.authURL(userID)
}

func (r *Registry) GoogleOAuthInfo() string {
	if !r.GoogleConfigured() {
		return "Google OAuth не настроен."
	}
	return fmt.Sprintf("Google OAuth настроен.\n\nRedirect URI для Google Cloud Console:\n`%s`\n\nClient ID: `%s`\n\nПроверьте, что redirect URI добавлен в OAuth Client типа Web application без лишнего слеша в конце.", r.google.redirectURI(), r.google.clientIDPreview())
}

func (r *Registry) HandleGoogleCallback(w http.ResponseWriter, req *http.Request) {
	r.google.handleCallback(w, req)
}

func (r *Registry) HasGoogleAccount(userID int64) bool {
	if !r.GoogleConfigured() {
		return false
	}
	_, ok := r.google.store.Account(userID, googleProvider)
	return ok
}

func (r *Registry) DisconnectGoogle(userID int64) error {
	if !r.GoogleConfigured() {
		return nil
	}
	return r.google.store.DeleteAccount(userID, googleProvider)
}

func (r *Registry) Tools() []mistral.Tool {
	return []mistral.Tool{
		functionTool("search_web", "Найти актуальную информацию в интернете через подключенный поисковый провайдер.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Поисковый запрос."},
			},
			"required": []string{"query"},
		}),
		functionTool("list_recent_email", "Показать последние письма пользователя из подключенной почты.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Фильтр поиска писем, например from:boss или invoice."},
				"limit": map[string]any{"type": "integer", "description": "Сколько писем вернуть, от 1 до 10."},
			},
		}),
		functionTool("create_calendar_event", "Создать событие в календаре пользователя.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"title":       map[string]any{"type": "string"},
				"starts_at":   map[string]any{"type": "string", "description": "Дата и время начала в ISO-8601."},
				"ends_at":     map[string]any{"type": "string", "description": "Дата и время окончания в ISO-8601."},
				"description": map[string]any{"type": "string"},
			},
			"required": []string{"title", "starts_at"},
		}),
		functionTool("remember_note", "Сохранить короткую заметку в памяти пользователя.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string", "description": "Текст заметки."},
			},
			"required": []string{"text"},
		}),
	}
}

func (r *Registry) Execute(ctx context.Context, userID int64, name string, args json.RawMessage) (map[string]any, error) {
	switch name {
	case "search_web":
		var input struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(args, &input); err != nil {
			return nil, err
		}
		return map[string]any{
			"status":  "not_configured",
			"message": "Веб-поиск пока не подключен к этому действию.",
			"query":   input.Query,
		}, nil
	case "list_recent_email":
		if !r.GoogleConfigured() {
			return map[string]any{
				"status":  "not_configured",
				"message": "Почта пока не подключена. Нужен OAuth-клиент Google или другой почтовый провайдер.",
			}, nil
		}
		return r.listRecentEmail(ctx, userID, args)
	case "create_calendar_event":
		if !r.GoogleConfigured() {
			return r.calendarNotConfigured(args)
		}
		return r.createCalendarEvent(ctx, userID, args)
	case "remember_note":
		var input struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(args, &input); err != nil {
			return nil, err
		}
		return map[string]any{"status": "ok", "message": "Заметка принята агентом. Постоянное хранилище заметок можно добавить отдельной таблицей.", "text": input.Text}, nil
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func (r *Registry) listRecentEmail(ctx context.Context, userID int64, args json.RawMessage) (map[string]any, error) {
	var input struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if input.Limit <= 0 || input.Limit > 10 {
		input.Limit = 5
	}
	accessToken, err := r.google.accessToken(ctx, userID)
	if err != nil {
		return r.googleConnectionRequired(userID, "Чтобы прочитать почту, нужно подключить Google аккаунт.")
	}

	values := url.Values{}
	values.Set("maxResults", strconv.Itoa(input.Limit))
	if strings.TrimSpace(input.Query) != "" {
		values.Set("q", input.Query)
	}
	var listResp struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := r.google.getJSON(ctx, "https://gmail.googleapis.com/gmail/v1/users/me/messages?"+values.Encode(), accessToken, &listResp); err != nil {
		return nil, err
	}

	emails := make([]map[string]any, 0, len(listResp.Messages))
	for _, msg := range listResp.Messages {
		getValues := url.Values{}
		getValues.Set("format", "metadata")
		for _, header := range []string{"Subject", "From", "Date"} {
			getValues.Add("metadataHeaders", header)
		}
		var msgResp struct {
			ID      string `json:"id"`
			Snippet string `json:"snippet"`
			Payload struct {
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"payload"`
		}
		endpoint := "https://gmail.googleapis.com/gmail/v1/users/me/messages/" + url.PathEscape(msg.ID) + "?" + getValues.Encode()
		if err := r.google.getJSON(ctx, endpoint, accessToken, &msgResp); err != nil {
			return nil, err
		}
		email := map[string]any{"id": msgResp.ID, "snippet": msgResp.Snippet}
		for _, h := range msgResp.Payload.Headers {
			email[strings.ToLower(h.Name)] = h.Value
		}
		emails = append(emails, email)
	}
	return map[string]any{"status": "ok", "emails": emails}, nil
}

func (r *Registry) calendarNotConfigured(args json.RawMessage) (map[string]any, error) {
	var input struct {
		Title       string `json:"title"`
		StartsAt    string `json:"starts_at"`
		EndsAt      string `json:"ends_at"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(args, &input)
	return map[string]any{
		"status":  "not_configured",
		"message": "Календарь пока не подключен. Нужен OAuth-клиент Google Calendar или другой провайдер.",
		"draft":   input,
	}, nil
}

func (r *Registry) createCalendarEvent(ctx context.Context, userID int64, args json.RawMessage) (map[string]any, error) {
	var input struct {
		Title       string `json:"title"`
		StartsAt    string `json:"starts_at"`
		EndsAt      string `json:"ends_at"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Title) == "" {
		return nil, fmt.Errorf("title is required")
	}
	if _, err := time.Parse(time.RFC3339, input.StartsAt); err != nil {
		return nil, fmt.Errorf("starts_at must be RFC3339/ISO-8601: %w", err)
	}
	start, _ := time.Parse(time.RFC3339, input.StartsAt)
	end := start.Add(time.Hour)
	if strings.TrimSpace(input.EndsAt) != "" {
		parsedEnd, err := time.Parse(time.RFC3339, input.EndsAt)
		if err != nil {
			return nil, fmt.Errorf("ends_at must be RFC3339/ISO-8601: %w", err)
		}
		end = parsedEnd
	}
	accessToken, err := r.google.accessToken(ctx, userID)
	if err != nil {
		result, linkErr := r.googleConnectionRequired(userID, "Чтобы создать событие в календаре, нужно подключить Google аккаунт.")
		if linkErr != nil {
			return nil, linkErr
		}
		result["draft"] = input
		return result, nil
	}

	body := map[string]any{
		"summary":     input.Title,
		"description": input.Description,
		"start":       map[string]string{"dateTime": start.Format(time.RFC3339)},
		"end":         map[string]string{"dateTime": end.Format(time.RFC3339)},
	}
	var out struct {
		ID       string `json:"id"`
		HTMLLink string `json:"htmlLink"`
		Summary  string `json:"summary"`
	}
	endpoint := "https://www.googleapis.com/calendar/v3/calendars/primary/events?sendUpdates=none"
	if err := r.google.postJSON(ctx, endpoint, accessToken, body, &out); err != nil {
		return nil, err
	}
	return map[string]any{"status": "ok", "event": out}, nil
}

func (r *Registry) googleConnectionRequired(userID int64, message string) (map[string]any, error) {
	authURL, err := r.GoogleAuthURL(userID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"status":   "not_connected",
		"message":  message,
		"auth_url": authURL,
		"scopes": []string{
			"gmail.readonly",
			"calendar.events",
		},
		"privacy": "OAuth state привязан к MAX user_id; access/refresh tokens сохраняются зашифрованно и используются только для этого пользователя.",
	}, nil
}

func functionTool(name, description string, parameters map[string]any) mistral.Tool {
	return mistral.Tool{
		Type: "function",
		Function: &mistral.ToolFunction{
			Name:        name,
			Description: description,
			Parameters:  parameters,
		},
	}
}
