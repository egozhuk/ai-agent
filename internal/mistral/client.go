package mistral

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	apiKey     string
	model      string
	httpClient *http.Client
}

func NewClient(baseURL, apiKey, model string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		model:      model,
		httpClient: httpClient,
	}
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		Name       string          `json:"name"`
		ToolCallID string          `json:"tool_call_id"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	content, err := parseMessageContent(raw.Content)
	if err != nil {
		return err
	}
	*m = Message{Role: raw.Role, Content: content, Name: raw.Name, ToolCallID: raw.ToolCallID, ToolCalls: raw.ToolCalls}
	return nil
}

func parseMessageContent(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var chunks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &chunks); err == nil {
		var parts []string
		for _, chunk := range chunks {
			if strings.TrimSpace(chunk.Text) != "" {
				parts = append(parts, chunk.Text)
			}
		}
		return strings.Join(parts, "\n"), nil
	}
	var chunk struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &chunk); err == nil && chunk.Text != "" {
		return chunk.Text, nil
	}
	return "", fmt.Errorf("unsupported message content format")
}

type Tool struct {
	Type     string        `json:"type"`
	Function *ToolFunction `json:"function,omitempty"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ChatRequest struct {
	Model             string    `json:"model"`
	Messages          []Message `json:"messages"`
	Tools             []Tool    `json:"tools,omitempty"`
	ToolChoice        string    `json:"tool_choice,omitempty"`
	Temperature       float64   `json:"temperature,omitempty"`
	MaxTokens         int       `json:"max_tokens,omitempty"`
	ParallelToolCalls bool      `json:"parallel_tool_calls"`
}

type documentChatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type documentChatRequest struct {
	Model             string                `json:"model"`
	Messages          []documentChatMessage `json:"messages"`
	Temperature       float64               `json:"temperature,omitempty"`
	MaxTokens         int                   `json:"max_tokens,omitempty"`
	ParallelToolCalls bool                  `json:"parallel_tool_calls"`
}

type ChatResponse struct {
	ID      string   `json:"id"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   Usage    `json:"usage"`
}

type Choice struct {
	Index    int       `json:"index"`
	Message  Message   `json:"message"`
	Messages []Message `json:"messages"`
}

func (c Choice) FirstMessage() Message {
	if len(c.Messages) > 0 {
		return c.Messages[len(c.Messages)-1]
	}
	return c.Message
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type WebSearchResponse struct {
	Content string
	Sources []Source
	Usage   Usage
}

type AutomationCall struct {
	Name      string
	Arguments json.RawMessage
	Usage     Usage
}

type ContextDecision struct {
	Reset      bool
	Confidence float64
	Usage      Usage
}

type APIError struct {
	Prefix     string
	StatusCode int
	Body       string
	RetryAfter string
	Reset      string
}

func (e *APIError) Error() string {
	details := ""
	if e.RetryAfter != "" {
		details += " Retry-After=" + e.RetryAfter
	}
	if e.Reset != "" {
		details += " X-RateLimit-Reset=" + e.Reset
	}
	return fmt.Sprintf("%s returned %d: %s%s", e.Prefix, e.StatusCode, e.Body, details)
}

func RateLimitDetails(err error) (*APIError, bool) {
	apiErr, ok := err.(*APIError)
	return apiErr, ok && apiErr.StatusCode == http.StatusTooManyRequests
}

type Source struct {
	Title  string
	URL    string
	Source string
}

type conversationRequest struct {
	Model          string         `json:"model"`
	Inputs         []Message      `json:"inputs"`
	Tools          []Tool         `json:"tools,omitempty"`
	Store          bool           `json:"store"`
	CompletionArgs map[string]any `json:"completion_args,omitempty"`
}

type conversationResponse struct {
	ConversationID string              `json:"conversation_id"`
	ID             string              `json:"id"`
	Outputs        []conversationEntry `json:"outputs"`
	Output         []conversationEntry `json:"output"`
	Usage          Usage               `json:"usage"`
}

type conversationEntry struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name"`
}

type contentChunk struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Source string `json:"source"`
	Tool   string `json:"tool"`
}

func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool) (ChatResponse, error) {
	reqBody := ChatRequest{
		Model:             c.model,
		Messages:          messages,
		Tools:             tools,
		ToolChoice:        "auto",
		Temperature:       0.3,
		MaxTokens:         1200,
		ParallelToolCalls: true,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return ChatResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return ChatResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := *c.httpClient
	if client.Timeout == 0 {
		client.Timeout = 90 * time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return ChatResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, responseError("Mistral API", resp, data)
	}

	var out ChatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return ChatResponse{}, err
	}
	return out, nil
}

func (c *Client) CompleteWithDocument(ctx context.Context, history []Message, question, documentURL string) (ChatResponse, error) {
	messages := []documentChatMessage{{
		Role: "system",
		Content: "Тебя зовут Люми. Всегда называй себя только Люми и никогда не используй другие имена для себя. " +
			"Ты отвечаешь на вопросы пользователя по прикрепленному документу. " +
			"Опирайся прежде всего на документ, не выдумывай отсутствующие факты и отвечай по-русски. " +
			"Не упоминай провайдера, модель или внутренние инструменты.",
	}}
	for _, message := range history {
		if message.Role == "user" || message.Role == "assistant" {
			messages = append(messages, documentChatMessage{Role: message.Role, Content: message.Content})
		}
	}
	messages = append(messages, documentChatMessage{
		Role: "user",
		Content: []map[string]any{
			{"type": "text", "text": question},
			{"type": "document_url", "document_url": documentURL},
		},
	})
	reqBody := documentChatRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: 0.2,
		MaxTokens:   1400,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return ChatResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return ChatResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	client := *c.httpClient
	if client.Timeout == 0 {
		client.Timeout = 180 * time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return ChatResponse{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return ChatResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatResponse{}, responseError("Mistral document API", resp, data)
	}
	var out ChatResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return ChatResponse{}, err
	}
	return out, nil
}

func (c *Client) PlanAutomation(ctx context.Context, inputs []Message, location *time.Location) (*AutomationCall, error) {
	if location == nil {
		location = time.UTC
	}
	now := time.Now().In(location)
	zone, offset := now.Zone()
	messages := []Message{
		{Role: "system", Content: "Тебя зовут Люми. Никогда не называй себя иначе и не упоминай другие имена для себя. Ты планировщик действий персонального ассистента. " +
			"Если пользователь просит поставить напоминание, вызови create_reminder. " +
			"Никогда не вызывай create_reminder без времени или задержки. Если время события неизвестно, но его можно найти в интернете, сначала вызови web_search. " +
			"Не проси пользователя уточнить данные, которые можно получить доступными инструментами. " +
			"Если для выполнения задачи сначала нужны актуальные данные, вызови web_search, а после получения результата продолжи исходную задачу. " +
			"Если просит через некоторое время проверить актуальную информацию и написать результат, вызови schedule_research. " +
			"schedule_research используй только при явной просьбе проверить информацию позже или через указанное время. " +
			"Вопросы «когда», «во сколько», «какой счёт» и подобные обрабатывай сразу, без schedule_research. " +
			"Если пользователь просит напоминание, но время не указал, всё равно вызови create_reminder без времени, чтобы ассистент запросил время. " +
			"Если пользователь ссылается на событие из предыдущего ответа («об этом матче»), используй дату и время из контекста, только если они там явно указаны. " +
			"Если просит следить за URL и сообщать об изменениях, вызови watch_url. " +
			"Если пользователь прислал URL и спрашивает, о чем страница или что на ней написано, вызови web_search. " +
			"Если просит показать его напоминания или задачи, вызови list_tasks. " +
			"Если просит удалить, отменить или убрать напоминание/задачу, вызови cancel_task. " +
			"Если просит каждый день присылать список задач на день, вызови schedule_daily_digest. " +
			"Если текущий запрос явно относится к новой теме и продолжение предыдущего диалога будет мешать, вызови reset_context. " +
			"Не вызывай reset_context для обычного уточнения, продолжения ответа, вопроса по активному документу или задачи, где нужен предыдущий контекст. " +
			"Если это не такая просьба, не вызывай инструмент. Не упоминай провайдера, модель или внутренние инструменты пользователю. " +
			"Текущее местное время пользователя: " + now.Format(time.RFC3339) + " (часовой пояс " + zone + ", UTC offset " + fmt.Sprintf("%d", offset) + "). " +
			"Для фраз «через N минут/часов» обязательно используй delay_seconds и не заполняй run_at. " +
			"Отвечай только вызовом инструмента без дополнительных вопросов, если данных достаточно."},
	}
	messages = append(messages, inputs...)
	tools := []Tool{
		{Type: "function", Function: &ToolFunction{
			Name:        "create_reminder",
			Description: "Создать одноразовое напоминание пользователя.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"message":       map[string]any{"type": "string", "description": "Текст напоминания"},
					"delay_seconds": map[string]any{"type": "integer", "description": "Через сколько секунд напомнить для относительного времени"},
					"run_at":        map[string]any{"type": "string", "description": "Абсолютное время в RFC3339, если пользователь назвал конкретную дату и время"},
				},
				"required": []string{"message"},
			},
		}},
		{Type: "function", Function: &ToolFunction{
			Name:        "schedule_research",
			Description: "Через указанное время выполнить веб-поиск по инструкции и отправить пользователю результат.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"instruction":   map[string]any{"type": "string", "description": "Что проверить и о чем написать пользователю"},
					"delay_seconds": map[string]any{"type": "integer", "description": "Через сколько секунд выполнить проверку"},
					"run_at":        map[string]any{"type": "string", "description": "Абсолютное время RFC3339, если задана конкретная дата и время"},
				},
				"required": []string{"instruction", "delay_seconds"},
			},
		}},
		{Type: "function", Function: &ToolFunction{
			Name:        "web_search",
			Description: "Найти актуальную информацию в интернете как промежуточный шаг цепочки. После получения результатов продолжи выполнение исходной задачи.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string", "description": "Что найти в интернете"},
				},
				"required": []string{"query"},
			},
		}},
		{Type: "function", Function: &ToolFunction{
			Name:        "watch_url",
			Description: "Периодически проверять веб-страницу и сообщать об изменениях.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":              map[string]any{"type": "string", "description": "Полный URL страницы"},
					"interval_seconds": map[string]any{"type": "integer", "description": "Интервал проверки в секундах, минимум 60"},
				},
				"required": []string{"url", "interval_seconds"},
			},
		}},
		{Type: "function", Function: &ToolFunction{
			Name:        "list_tasks",
			Description: "Показать активные напоминания и наблюдения текущего пользователя.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		}},
		{Type: "function", Function: &ToolFunction{
			Name:        "cancel_task",
			Description: "Удалить активную задачу текущего пользователя по ID или описанию из контекста.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task_id":     map[string]any{"type": "string", "description": "ID задачи, если он известен"},
					"description": map[string]any{"type": "string", "description": "Уникальная часть текста напоминания или задачи"},
					"run_at":      map[string]any{"type": "string", "description": "Время задачи в RFC3339 или формате YYYY-MM-DD HH:MM, если нужно различить похожие задачи"},
				},
			},
		}},
		{Type: "function", Function: &ToolFunction{
			Name:        "schedule_daily_digest",
			Description: "Каждый день отправлять пользователю список его задач на этот день.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local_time": map[string]any{"type": "string", "description": "Местное время отправки в формате HH:MM"},
				},
				"required": []string{"local_time"},
			},
		}},
		{Type: "function", Function: &ToolFunction{
			Name:        "reset_context",
			Description: "Очистить историю текущего диалога перед ответом на явно новую, не связанную с предыдущей темой просьбу.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		}},
	}
	resp, err := c.Complete(ctx, messages, tools)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return &AutomationCall{Usage: resp.Usage}, nil
	}
	message := resp.Choices[0].FirstMessage()
	if len(message.ToolCalls) == 0 {
		return &AutomationCall{Usage: resp.Usage}, nil
	}
	call := message.ToolCalls[0]
	return &AutomationCall{Name: call.Function.Name, Arguments: call.Function.Arguments, Usage: resp.Usage}, nil
}

func (c *Client) AssessContext(ctx context.Context, history []Message, current string) (ContextDecision, error) {
	if len(history) == 0 {
		return ContextDecision{}, nil
	}
	messages := []Message{{
		Role: "system",
		Content: "Тебя зовут Люми. Не используй другие имена для себя. Ты классификатор контекста персонального ассистента. " +
			"Определи, является ли текущий запрос новой самостоятельной темой относительно истории. " +
			"Продолжение вопроса, уточнение, ссылка на предыдущий ответ или вопрос по активному документу не являются новой темой. " +
			"Верни только JSON: {\"reset\":true|false,\"confidence\":число от 0 до 1}.",
	}}
	messages = append(messages, history...)
	messages = append(messages, Message{Role: "user", Content: current})
	resp, err := c.Complete(ctx, messages, nil)
	if err != nil {
		return ContextDecision{}, err
	}
	if len(resp.Choices) == 0 {
		return ContextDecision{Usage: resp.Usage}, nil
	}
	var decision struct {
		Reset      bool    `json:"reset"`
		Confidence float64 `json:"confidence"`
	}
	content := strings.TrimSpace(resp.Choices[0].FirstMessage().Content)
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(strings.TrimSpace(content), "```")
	if err := json.Unmarshal([]byte(strings.TrimSpace(content)), &decision); err != nil {
		return ContextDecision{Usage: resp.Usage}, nil
	}
	return ContextDecision{Reset: decision.Reset, Confidence: decision.Confidence, Usage: resp.Usage}, nil
}

func (c *Client) CompleteWithWebSearch(ctx context.Context, messages []Message) (WebSearchResponse, error) {
	reqBody := conversationRequest{
		Model:  c.model,
		Inputs: messages,
		Tools: []Tool{
			{Type: "web_search"},
		},
		Store: false,
		CompletionArgs: map[string]any{
			"temperature": 0.3,
			"max_tokens":  1200,
		},
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return WebSearchResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/conversations", bytes.NewReader(payload))
	if err != nil {
		return WebSearchResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := *c.httpClient
	if client.Timeout == 0 {
		client.Timeout = 120 * time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return WebSearchResponse{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
	if err != nil {
		return WebSearchResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return WebSearchResponse{}, responseError("Mistral Conversations API", resp, data)
	}

	var out conversationResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return WebSearchResponse{}, err
	}
	return parseConversation(out), nil
}

func responseError(prefix string, resp *http.Response, data []byte) error {
	return &APIError{
		Prefix:     prefix,
		StatusCode: resp.StatusCode,
		Body:       strings.TrimSpace(string(data)),
		RetryAfter: strings.TrimSpace(resp.Header.Get("Retry-After")),
		Reset:      strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")),
	}
}

func parseConversation(resp conversationResponse) WebSearchResponse {
	entries := resp.Outputs
	if len(entries) == 0 {
		entries = resp.Output
	}

	var texts []string
	var sources []Source
	seenSources := map[string]bool{}

	for _, entry := range entries {
		if entry.Type != "" && entry.Type != "message.output" && entry.Type != "message" {
			continue
		}
		chunks := parseContent(entry.Content)
		for _, chunk := range chunks {
			switch chunk.Type {
			case "text", "":
				if strings.TrimSpace(chunk.Text) != "" {
					texts = append(texts, strings.TrimSpace(chunk.Text))
				}
			case "tool_reference":
				if chunk.URL == "" || seenSources[chunk.URL] {
					continue
				}
				seenSources[chunk.URL] = true
				sources = append(sources, Source{
					Title:  chunk.Title,
					URL:    chunk.URL,
					Source: chunk.Source,
				})
			}
		}
	}

	return WebSearchResponse{
		Content: strings.TrimSpace(strings.Join(texts, "\n\n")),
		Sources: sources,
		Usage:   resp.Usage,
	}
}

func parseContent(raw json.RawMessage) []contentChunk {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []contentChunk{{Type: "text", Text: text}}
	}
	var chunks []contentChunk
	if err := json.Unmarshal(raw, &chunks); err == nil {
		return chunks
	}
	var chunk contentChunk
	if err := json.Unmarshal(raw, &chunk); err == nil {
		return []contentChunk{chunk}
	}
	return nil
}
