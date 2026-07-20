package maxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

func NewClient(baseURL, token string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		token:      token,
		httpClient: httpClient,
	}
}

type UpdatesResponse struct {
	Updates []Update `json:"updates"`
	Marker  *int64   `json:"marker"`
}

type Update struct {
	UpdateType string          `json:"update_type"`
	Timestamp  int64           `json:"timestamp"`
	ChatID     *int64          `json:"chat_id,omitempty"`
	User       *User           `json:"user,omitempty"`
	Message    *Message        `json:"message,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

func (u *Update) UnmarshalJSON(data []byte) error {
	type alias Update
	var v alias
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*u = Update(v)
	u.Raw = append(u.Raw[:0], data...)
	if u.Message == nil {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(data, &envelope); err == nil {
			if messageRaw, ok := envelope["message"]; ok && string(messageRaw) != "null" {
				var message Message
				if err := json.Unmarshal(messageRaw, &message); err == nil {
					u.Message = &message
				}
			}
			if u.Message == nil {
				if _, hasBody := envelope["body"]; hasBody {
					var message Message
					if err := json.Unmarshal(data, &message); err == nil {
						u.Message = &message
					}
				}
			}
			if u.Message == nil {
				u.Message = messageFromNestedPayload(envelope)
			}
		}
	}
	return nil
}

func (u Update) CallbackID() string {
	if callback, ok := u.callback(); ok {
		return callback.CallbackID
	}
	return findStringField(u.Raw, "callback_id")
}

func (u Update) CallbackPayload() string {
	if callback, ok := u.callback(); ok {
		return callback.Payload
	}
	return findStringField(u.Raw, "payload")
}

func (u Update) EffectiveUserID() int64 {
	if u.User != nil && u.User.EffectiveID() != 0 {
		return u.User.EffectiveID()
	}
	if callback, ok := u.callback(); ok && callback.UserID() != 0 {
		return callback.UserID()
	}
	if u.Message != nil && u.Message.EffectiveSenderID() != 0 {
		return u.Message.EffectiveSenderID()
	}
	return findIntField(u.Raw, "user_id")
}

type callbackData struct {
	CallbackID string `json:"callback_id"`
	Payload    string `json:"payload"`
	User       struct {
		ID     int64 `json:"id"`
		UserID int64 `json:"user_id"`
	} `json:"user"`
}

func (c callbackData) UserID() int64 {
	if c.User.UserID != 0 {
		return c.User.UserID
	}
	return c.User.ID
}

func (u Update) callback() (callbackData, bool) {
	if len(u.Raw) == 0 {
		return callbackData{}, false
	}
	var envelope struct {
		Callback *callbackData `json:"callback"`
	}
	if err := json.Unmarshal(u.Raw, &envelope); err != nil || envelope.Callback == nil {
		return callbackData{}, false
	}
	return *envelope.Callback, true
}

func messageFromNestedPayload(value any) *Message {
	switch current := value.(type) {
	case map[string]json.RawMessage:
		if _, hasBody := current["body"]; hasBody {
			data, err := json.Marshal(current)
			if err == nil {
				var message Message
				if json.Unmarshal(data, &message) == nil {
					return &message
				}
			}
		}
		for _, nested := range current {
			var decoded any
			if json.Unmarshal(nested, &decoded) == nil {
				if message := messageFromNestedPayload(decoded); message != nil {
					return message
				}
			}
		}
	case map[string]any:
		if _, hasBody := current["body"]; hasBody {
			data, err := json.Marshal(current)
			if err == nil {
				var message Message
				if json.Unmarshal(data, &message) == nil {
					return &message
				}
			}
		}
		for _, nested := range current {
			if message := messageFromNestedPayload(nested); message != nil {
				return message
			}
		}
	case []any:
		for _, nested := range current {
			if message := messageFromNestedPayload(nested); message != nil {
				return message
			}
		}
	}
	return nil
}

type User struct {
	ID        int64  `json:"user_id,omitempty"`
	UserID    int64  `json:"id,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
	Username  string `json:"username,omitempty"`
}

func (u User) EffectiveID() int64 {
	if u.ID != 0 {
		return u.ID
	}
	return u.UserID
}

type Message struct {
	ID        string          `json:"message_id,omitempty"`
	Text      string          `json:"text,omitempty"`
	Body      *MessageBody    `json:"body,omitempty"`
	Sender    *User           `json:"sender,omitempty"`
	Recipient *Recipient      `json:"recipient,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	type alias Message
	var v alias
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*m = Message(v)
	m.Raw = append(m.Raw[:0], data...)
	return nil
}

type MessageBody struct {
	Text string `json:"text,omitempty"`
}

type KeyboardButton struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Payload string `json:"payload,omitempty"`
}

type InlineKeyboard struct {
	Type    string          `json:"type"`
	Payload KeyboardPayload `json:"payload"`
}

type KeyboardPayload struct {
	Buttons [][]KeyboardButton `json:"buttons"`
}

type NewMessageBody struct {
	Text        string           `json:"text"`
	Format      string           `json:"format,omitempty"`
	Notify      bool             `json:"notify,omitempty"`
	Attachments []InlineKeyboard `json:"attachments,omitempty"`
}

type MediaAttachment struct {
	Type     string
	Name     string
	MimeType string
	URL      string
	Token    string
}

type Recipient struct {
	ChatID *int64 `json:"chat_id,omitempty"`
	UserID *int64 `json:"user_id,omitempty"`
}

// TopLevelFields returns only JSON field names for diagnostics; values are never logged.
func TopLevelFields(raw json.RawMessage) []string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil
	}
	result := make([]string, 0, len(fields))
	for key := range fields {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func (m Message) EffectiveText() string {
	if m.Body != nil && strings.TrimSpace(m.Body.Text) != "" {
		return strings.TrimSpace(m.Body.Text)
	}
	if strings.TrimSpace(m.Text) != "" {
		return strings.TrimSpace(m.Text)
	}
	if transcription := findStringField(m.Raw, "transcription"); transcription != "" {
		return strings.TrimSpace(transcription)
	}
	if text := findStringField(m.Raw, "text"); text != "" {
		return strings.TrimSpace(text)
	}
	return ""
}

func (m Message) Transcription() string {
	return strings.TrimSpace(findStringField(m.Raw, "transcription"))
}

func (m Message) EffectiveSenderID() int64 {
	if m.Sender != nil {
		return m.Sender.EffectiveID()
	}
	if id := findIntField(m.Raw, "user_id"); id != 0 {
		return id
	}
	if id := findIntField(m.Raw, "sender_id"); id != 0 {
		return id
	}
	return 0
}

func (m Message) EffectiveChatID() int64 {
	if m.Recipient != nil && m.Recipient.ChatID != nil {
		return *m.Recipient.ChatID
	}
	if id := findIntField(m.Raw, "chat_id"); id != 0 {
		return id
	}
	return 0
}

func (m Message) EffectiveID() string {
	if strings.TrimSpace(m.ID) != "" {
		return m.ID
	}
	if id := findStringField(m.Raw, "message_id"); id != "" {
		return id
	}
	return findStringField(m.Raw, "mid")
}

func (m Message) HasAttachments() bool {
	if len(m.Raw) == 0 {
		return false
	}
	var value any
	if err := json.Unmarshal(m.Raw, &value); err != nil {
		return false
	}
	return hasAttachmentsValue(value)
}

func (m Message) DocumentAttachments() []MediaAttachment {
	if len(m.Raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(m.Raw, &value); err != nil {
		return nil
	}
	var result []MediaAttachment
	collectAttachments(value, &result)
	var documents []MediaAttachment
	for _, attachment := range result {
		typ := strings.ToLower(attachment.Type)
		if strings.Contains(typ, "audio") || strings.Contains(typ, "voice") || strings.Contains(typ, "image") || strings.Contains(typ, "video") || strings.Contains(typ, "sticker") {
			continue
		}
		if typ == "file" || typ == "document" || attachment.MimeType != "" || attachment.Token != "" || strings.HasSuffix(strings.ToLower(attachment.Name), ".pdf") {
			documents = append(documents, attachment)
		}
	}
	return documents
}

func collectAttachments(value any, result *[]MediaAttachment) {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			collectAttachments(item, result)
		}
	case map[string]any:
		for key, item := range current {
			if strings.EqualFold(key, "attachments") || strings.EqualFold(key, "attachment") {
				if attachments, ok := item.([]any); ok {
					for _, rawAttachment := range attachments {
						if attachment, ok := parseAttachment(rawAttachment); ok {
							*result = append(*result, attachment)
						}
					}
				} else if attachment, ok := parseAttachment(item); ok {
					*result = append(*result, attachment)
				}
				continue
			}
			collectAttachments(item, result)
		}
	}
}

func parseAttachment(value any) (MediaAttachment, bool) {
	current, ok := value.(map[string]any)
	if !ok {
		return MediaAttachment{}, false
	}
	attachment := MediaAttachment{}
	attachment.Type, _ = current["type"].(string)
	attachment.Name = firstStringInValue(current, "filename", "file_name", "name", "title")
	attachment.MimeType = firstStringInValue(current, "mime_type", "mime", "content_type")
	attachment.URL = findURLInValue(current)
	attachment.Token = findStringInValue(current, "token")
	return attachment, attachment.Type != "" || attachment.URL != "" || attachment.Token != ""
}

func firstStringInValue(value any, keys ...string) string {
	for _, key := range keys {
		if item := findStringInValue(value, key); item != "" {
			return item
		}
	}
	return ""
}

func findURLInValue(value any) string {
	if candidate := findStringInValue(value, "url"); strings.HasPrefix(candidate, "http") {
		return candidate
	}
	for _, key := range []string{"file_url", "download_url", "src", "link"} {
		if candidate := findStringInValue(value, key); strings.HasPrefix(candidate, "http") {
			return candidate
		}
	}
	return ""
}

func findStringInValue(value any, key string) string {
	switch current := value.(type) {
	case map[string]any:
		if found, ok := current[key].(string); ok {
			return strings.TrimSpace(found)
		}
		for _, nested := range current {
			if found := findStringInValue(nested, key); found != "" {
				return found
			}
		}
	case []any:
		for _, nested := range current {
			if found := findStringInValue(nested, key); found != "" {
				return found
			}
		}
	}
	return ""
}

func hasAttachmentsValue(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, item := range current {
			if strings.EqualFold(key, "attachments") || strings.EqualFold(key, "attachment") {
				if items, ok := item.([]any); ok && len(items) > 0 {
					return true
				}
				if item != nil {
					return true
				}
			}
			if _, ok := item.(map[string]any); ok && hasAttachmentsValue(item) {
				return true
			}
		}
	}
	return false
}

func (c *Client) GetUpdates(ctx context.Context, marker *int64, types []string) (UpdatesResponse, error) {
	values := url.Values{}
	values.Set("limit", "20")
	values.Set("timeout", "30")
	if len(types) > 0 {
		values.Set("types", strings.Join(types, ","))
	}
	if marker != nil {
		values.Set("marker", strconv.FormatInt(*marker, 10))
	}

	var out UpdatesResponse
	err := c.do(ctx, http.MethodGet, "/updates?"+values.Encode(), nil, &out)
	return out, err
}

func (c *Client) SendMessage(ctx context.Context, chatID int64, userID int64, text string) (*Message, error) {
	return c.sendMessage(ctx, chatID, userID, text, nil)
}

func (c *Client) SendMessageWithKeyboard(ctx context.Context, chatID int64, userID int64, text string, buttons [][]KeyboardButton) (*Message, error) {
	return c.sendMessage(ctx, chatID, userID, text, buttons)
}

func (c *Client) sendMessage(ctx context.Context, chatID int64, userID int64, text string, buttons [][]KeyboardButton) (*Message, error) {
	if len([]rune(text)) > 3900 {
		text = string([]rune(text)[:3900])
	}
	values := url.Values{}
	if chatID != 0 {
		values.Set("chat_id", strconv.FormatInt(chatID, 10))
	} else {
		values.Set("user_id", strconv.FormatInt(userID, 10))
	}
	body := map[string]any{
		"text":   text,
		"format": "markdown",
		"notify": true,
	}
	if len(buttons) > 0 {
		body["attachments"] = []InlineKeyboard{{Type: "inline_keyboard", Payload: KeyboardPayload{Buttons: buttons}}}
	}
	var response json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/messages?"+values.Encode(), body, &response); err != nil {
		return nil, err
	}

	if id := messageIDFromJSON(response); id != "" {
		return &Message{ID: id}, nil
	}
	return &Message{}, nil
}

func (c *Client) DeleteMessage(ctx context.Context, messageID string) error {
	if strings.TrimSpace(messageID) == "" {
		return nil
	}
	values := url.Values{}
	values.Set("message_id", messageID)
	return c.do(ctx, http.MethodDelete, "/messages?"+values.Encode(), nil, nil)
}

func (c *Client) EditMessage(ctx context.Context, messageID, text string) error {
	return c.EditMessageWithKeyboard(ctx, messageID, text, nil)
}

func (c *Client) EditMessageWithKeyboard(ctx context.Context, messageID, text string, buttons [][]KeyboardButton) error {
	if strings.TrimSpace(messageID) == "" {
		return nil
	}
	if len([]rune(text)) > 3900 {
		text = string([]rune(text)[:3900])
	}
	values := url.Values{}
	values.Set("message_id", messageID)
	body := map[string]any{
		"text":   text,
		"format": "markdown",
	}
	if buttons != nil {
		body["attachments"] = []InlineKeyboard{{Type: "inline_keyboard", Payload: KeyboardPayload{Buttons: buttons}}}
	}
	return c.do(ctx, http.MethodPut, "/messages?"+values.Encode(), body, nil)
}

func (c *Client) AnswerCallback(ctx context.Context, callbackID, notification string) error {
	if strings.TrimSpace(callbackID) == "" {
		return nil
	}
	values := url.Values{}
	values.Set("callback_id", callbackID)
	body := map[string]any{"notification": notification}
	return c.do(ctx, http.MethodPost, "/answers?"+values.Encode(), body, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := *c.httpClient
	if client.Timeout == 0 {
		client.Timeout = 95 * time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("MAX API %s %s returned %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func messageIDFromJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ""
	}
	if id := jsonValueString(fields["mid"]); id != "" {
		return id
	}
	if id := jsonValueString(fields["message_id"]); id != "" {
		return id
	}
	if id := jsonValueString(fields["id"]); id != "" {
		return id
	}
	if id := findStringField(raw, "mid"); id != "" {
		return id
	}
	return findStringField(raw, "message_id")
}

func jsonValueString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(string(raw))
}

func findStringField(raw json.RawMessage, key string) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	found, _ := walk(value, key)
	if s, ok := found.(string); ok {
		return s
	}
	return ""
}

func findIntField(raw json.RawMessage, key string) int64 {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0
	}
	found, _ := walk(value, key)
	switch v := found.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		i, _ := v.Int64()
		return i
	}
	return 0
}

func walk(value any, key string) (any, bool) {
	switch v := value.(type) {
	case map[string]any:
		if found, ok := v[key]; ok {
			return found, true
		}
		for _, nested := range v {
			if found, ok := walk(nested, key); ok {
				return found, true
			}
		}
	case []any:
		for _, nested := range v {
			if found, ok := walk(nested, key); ok {
				return found, true
			}
		}
	}
	return nil, false
}
