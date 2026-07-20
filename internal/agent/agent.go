package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"AI-agent/internal/integrations"
	"AI-agent/internal/maxapi"
	"AI-agent/internal/mistral"
	"AI-agent/internal/search"
	"AI-agent/internal/store"
)

type Options struct {
	OwnerUserID int64
	AccessMode  string
	Store       *store.Store
	Max         *maxapi.Client
	Mistral     *mistral.Client
	Search      *search.Client
	Tools       *integrations.Registry
	Logger      *slog.Logger
}

type Agent struct {
	ownerUserID int64
	accessMode  string
	store       *store.Store
	max         *maxapi.Client
	mistral     *mistral.Client
	search      *search.Client
	tools       *integrations.Registry
	logger      *slog.Logger
	pendingMu   sync.Mutex
	pending     map[int64]pendingInput
}

type pendingInput struct {
	kind   string
	taskID string
}

const contextResetSignal = "__context_reset__"

func New(opts Options) *Agent {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{
		ownerUserID: opts.OwnerUserID,
		accessMode:  opts.AccessMode,
		store:       opts.Store,
		max:         opts.Max,
		mistral:     opts.Mistral,
		search:      opts.Search,
		tools:       opts.Tools,
		logger:      logger,
		pending:     make(map[int64]pendingInput),
	}
}

func (a *Agent) HandleUpdate(ctx context.Context, update maxapi.Update) error {
	a.logger.Debug("received MAX update", "type", update.UpdateType, "has_message", update.Message != nil, "raw_bytes", len(update.Raw))
	if update.UpdateType == "message_callback" {
		return a.handleCallback(ctx, update)
	}
	if update.UpdateType == "bot_started" {
		userID := int64(0)
		if update.User != nil {
			userID = update.User.EffectiveID()
		}
		if !a.allowed(userID) {
			return a.replyIfAllowed(ctx, update, userID, "Бот пока доступен только владельцу.")
		}
		if !a.profileComplete(userID) {
			return a.beginProfileSetup(ctx, update, userID)
		}
		return a.sendMainMenu(ctx, update)
	}
	messageUpdate := update.UpdateType == "message_created" || (update.UpdateType == "message_edited" && update.Message != nil && update.Message.Transcription() != "")
	if !messageUpdate || update.Message == nil {
		if update.UpdateType != "message_created" && update.UpdateType != "message_edited" {
			a.logger.Debug("ignored MAX update type", "type", update.UpdateType)
		} else {
			a.logger.Warn("message update has no usable message payload", "type", update.UpdateType, "top_level_fields", maxapi.TopLevelFields(update.Raw))
		}
		return nil
	}

	userID := update.Message.EffectiveSenderID()
	if userID == 0 && update.User != nil {
		userID = update.User.EffectiveID()
	}
	if !a.allowed(userID) {
		a.logger.Warn("blocked non-owner message", "user_id", userID)
		return a.send(ctx, update, "Бот пока доступен только владельцу.")
	}
	text := update.Message.EffectiveText()
	if !a.profileComplete(userID) {
		if handled, err := a.handlePendingInput(ctx, update, userID, text); handled {
			return err
		}
		return a.beginProfileSetup(ctx, update, userID)
	}
	hasAttachments := update.Message.HasAttachments()
	documents := update.Message.DocumentAttachments()
	if len(documents) > 0 {
		document := documents[0]
		if document.URL == "" {
			return a.send(ctx, update, "Получил файл, но MAX не передал доступную ссылку для его чтения.")
		}
		question := recentDocumentQuestion(a.store.History(userID))
		progressText := "📄 Прикрепляю документ…"
		if question != "" {
			progressText = "📄 Изучаю документ…"
		}
		progress, progressErr := a.sendProgress(ctx, update, progressText)
		if progressErr != nil {
			a.logger.Warn("document progress message failed", "error", progressErr)
		}
		stored := store.Document{
			ID:        fmt.Sprintf("document-%d", time.Now().UnixNano()),
			UserID:    userID,
			Name:      document.Name,
			MimeType:  document.MimeType,
			URL:       document.URL,
			CreatedAt: time.Now().UTC(),
		}
		if strings.TrimSpace(stored.Name) == "" {
			stored.Name = "прикрепленный документ"
		}
		err := a.store.SaveDocument(stored)
		if err != nil {
			if progress != nil && progress.ID != "" {
				_ = a.max.DeleteMessage(ctx, progress.ID)
			}
			a.logger.Warn("document save failed", "error", err)
			return a.send(ctx, update, "Не удалось сохранить документ.")
		}
		if question != "" {
			answer, questionErr := a.askDocument(ctx, userID, question, stored.URL)
			if progress != nil && progress.ID != "" {
				_ = a.max.DeleteMessage(ctx, progress.ID)
			}
			if questionErr != nil {
				a.logger.Warn("document question failed", "error", questionErr)
				return a.send(ctx, update, "Документ сохранён, но не удалось прочитать его содержимое.")
			}
			return a.send(ctx, update, answer)
		}
		if progress != nil && progress.ID != "" {
			_ = a.max.DeleteMessage(ctx, progress.ID)
		}
		return a.send(ctx, update, fmt.Sprintf("Документ `%s` сохранён. Теперь задавайте вопросы по нему.", stored.Name))
	}
	if strings.TrimSpace(text) == "" {
		if hasAttachments {
			return a.send(ctx, update, "Получил вложение, но этот тип файла пока не поддерживается.")
		}
		return nil
	}
	if handled, err := a.handlePendingInput(ctx, update, userID, text); handled {
		return err
	}
	if containsURL(text) {
		// A URL is a web research request, not a document attachment. Drop a
		// stale active document left by older MAX link-preview payloads.
		if err := a.store.ClearDocuments(userID); err != nil {
			return err
		}
	}

	lowerText := strings.ToLower(strings.TrimSpace(text))
	if strings.HasPrefix(lowerText, "/remind ") {
		return a.createReminder(ctx, update, userID, text)
	}
	if strings.HasPrefix(lowerText, "/watch ") {
		return a.createURLWatch(ctx, update, userID, text)
	}
	if strings.HasPrefix(lowerText, "/cancel ") {
		return a.cancelTask(ctx, update, userID, text)
	}
	switch strings.TrimSpace(strings.ToLower(text)) {
	case "/start":
		return a.sendMainMenu(ctx, update)
	case "/clear_context", "/reset_context", "/clear":
		if err := a.store.ClearHistory(userID); err != nil {
			return err
		}
		if err := a.store.ClearDocuments(userID); err != nil {
			return err
		}
		return a.send(ctx, update, "Контекст очищен. Статистика токенов и подключенные аккаунты сохранены.")
	case "/usage":
		usage := a.store.Usage(userID)
		return a.send(ctx, update, fmt.Sprintf("Токены по вашему пользователю:\n\n- prompt: `%d`\n- completion: `%d`\n- total: `%d`", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens))
	case "/timezone":
		return a.sendTimezoneSetup(ctx, update, userID)
	case "/reminders":
		return a.listTasks(ctx, update, userID)
	case "/clear_reminders", "/delete_all_reminders":
		count := a.store.DeleteTasksByKind(userID, "reminder")
		if count == 0 {
			return a.send(ctx, update, "Активных напоминаний нет.")
		}
		return a.send(ctx, update, fmt.Sprintf("Удалил напоминаний: `%d`.", count))
	case "/connect_google":
		if !a.tools.GoogleConfigured() {
			return a.send(ctx, update, "Google OAuth не настроен на сервере. Нужны `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `PUBLIC_BASE_URL`, `TOKEN_ENCRYPTION_KEY`.")
		}
		link, err := a.tools.GoogleAuthURL(userID)
		if err != nil {
			return err
		}
		return a.send(ctx, update, "Подключите Google аккаунт по ссылке:\n\n"+link+"\n\nДоступы: чтение Gmail и создание событий в Google Calendar. Токены будут сохранены зашифрованно.")
	case "/oauth_google_info":
		return a.send(ctx, update, a.tools.GoogleOAuthInfo())
	case "/connections":
		if a.tools.HasGoogleAccount(userID) {
			return a.send(ctx, update, "Подключено: Google Gmail/Calendar.")
		}
		return a.send(ctx, update, "Пока нет подключенных аккаунтов. Используйте `/connect_google`.")
	case "/disconnect_google":
		if err := a.tools.DisconnectGoogle(userID); err != nil {
			return err
		}
		return a.send(ctx, update, "Google аккаунт отключен, сохраненные токены удалены.")
	}

	contextReset := false
	if history := a.store.History(userID); len(history) > 0 {
		decision, decisionErr := a.mistral.AssessContext(ctx, history, text)
		if decisionErr != nil {
			a.logger.Warn("context assessment failed", "error", decisionErr)
		} else {
			if err := a.store.AddUsage(userID, decision.Usage); err != nil {
				return err
			}
			if decision.Reset && decision.Confidence >= 0.8 {
				if err := a.store.ClearHistory(userID); err != nil {
					return err
				}
				if err := a.store.ClearDocuments(userID); err != nil {
					return err
				}
				contextReset = true
			}
		}
	}
	initialProgress := progressText(text)
	if contextReset {
		initialProgress = "🧹 Начинаю новую тему…"
	}
	progress, progressErr := a.sendProgress(ctx, update, initialProgress)
	if progressErr != nil {
		a.logger.Warn("progress message failed", "error", progressErr)
	}
	if contextReset {
		a.updateProgress(ctx, progress, "🧠 Планирую действия…")
	}
	var answer string
	var err error
	var handled bool
	handled, answer, err = a.handleMistralAutomation(ctx, update, userID, text, progress, 0)
	if answer == contextResetSignal && err == nil {
		answer, err = a.askMistral(ctx, userID, text)
		if err == nil {
			answer = "🧹 Контекст очищен. Начинаю новую тему.\n\n" + answer
		}
		handled = true
	}
	if !handled && err == nil {
		answer, err = a.askMistral(ctx, userID, text)
	}
	if err != nil {
		a.logger.Warn("mistral request failed", "error", err)
		if rateLimit, ok := mistral.RateLimitDetails(err); ok {
			answer = rateLimitMessage(rateLimit)
		} else {
			answer = "Не смог получить ответ. Попробуйте еще раз чуть позже."
		}
	}
	if contextReset && answer != "" && !strings.HasPrefix(answer, "🧹 Контекст очищен.") {
		answer = "🧹 Контекст очищен. Начинаю новую тему.\n\n" + answer
	}

	if progress != nil && progress.ID != "" {
		if deleteErr := a.max.DeleteMessage(ctx, progress.ID); deleteErr == nil {
			return a.send(ctx, update, answer)
		} else {
			a.logger.Warn("progress message delete failed, trying edit", "message_id", progress.ID, "error", deleteErr)
			if editErr := a.max.EditMessage(ctx, progress.ID, answer); editErr == nil {
				return nil
			} else {
				a.logger.Warn("progress message edit failed", "message_id", progress.ID, "error", editErr)
			}
		}
	}
	return a.send(ctx, update, answer)
}

func rateLimitMessage(rateLimit *mistral.APIError) string {
	message := "⚠️ Временно достигнут лимит внешнего сервиса."
	if rateLimit.RetryAfter != "" {
		return message + " Повторите запрос через `" + rateLimit.RetryAfter + "` секунд."
	}
	if rateLimit.Reset != "" {
		return message + " Сброс лимита указан в `X-RateLimit-Reset: " + rateLimit.Reset + "`."
	}
	return message + " Точное время сброса сервис не передал. Обычно нужно подождать и повторить запрос позже."
}

func (a *Agent) handleMistralAutomation(ctx context.Context, update maxapi.Update, userID int64, text string, progress *maxapi.Message, depth int) (bool, string, error) {
	inputs := append(a.store.History(userID), mistral.Message{Role: "user", Content: text})
	location := a.userLocation(userID)
	call, err := a.mistral.PlanAutomation(ctx, inputs, location)
	if err != nil {
		return false, "", err
	}
	if call == nil {
		return false, "", nil
	}
	if err := a.store.AddUsage(userID, call.Usage); err != nil {
		return true, "", err
	}
	if call.Name == "" {
		return false, "", nil
	}
	if call.Name == "web_search" {
		if depth >= 4 {
			return true, "Не удалось завершить цепочку действий: превышено число шагов.", nil
		}
		var args struct {
			Query string `json:"query"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil || strings.TrimSpace(args.Query) == "" {
			return true, "Не смог сформировать поисковый запрос.", nil
		}
		a.updateProgress(ctx, progress, "🔎 Ищу информацию в интернете…")
		if a.search == nil {
			return true, "Поиск не настроен на сервере.", nil
		}
		webResp, err := a.search.Search(ctx, args.Query)
		if err != nil {
			return true, "Не удалось выполнить веб-поиск.", err
		}
		a.updateProgress(ctx, progress, "🧠 Продолжаю выполнение задачи…")
		enrichedText := text + "\n\nРезультат промежуточного веб-поиска:\n" + webResp.Content
		nextHandled, nextAnswer, nextErr := a.handleMistralAutomation(ctx, update, userID, enrichedText, progress, depth+1)
		if nextErr != nil {
			return true, "", nextErr
		}
		if nextAnswer == contextResetSignal {
			return true, contextResetSignal, nil
		}
		if !nextHandled {
			nextAnswer, nextErr = a.askMistral(ctx, userID, enrichedText)
		}
		if nextErr != nil {
			return true, "", nextErr
		}
		return true, withSources(nextAnswer, webResp.Results), nil
	}
	now := time.Now().UTC()
	var answer string
	switch call.Name {
	case "create_reminder":
		var args struct {
			Message      string `json:"message"`
			DelaySeconds int64  `json:"delay_seconds"`
			RunAt        string `json:"run_at"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil || strings.TrimSpace(args.Message) == "" {
			return true, "Не смог разобрать текст напоминания.", nil
		}
		if args.DelaySeconds <= 0 && strings.TrimSpace(args.RunAt) == "" {
			if depth < 4 {
				a.updateProgress(ctx, progress, "🧠 Уточняю недостающие данные…")
				replanText := text + "\n\nСистемное наблюдение: создать напоминание пока нельзя, потому что время не определено. Не спрашивай пользователя сразу. Если время относится к внешнему событию, сначала найди его через web_search, затем повтори планирование."
				return a.handleMistralAutomation(ctx, update, userID, replanText, progress, depth+1)
			}
			return true, "На какое время поставить напоминание?", nil
		}
		runAt := now.Add(time.Duration(args.DelaySeconds) * time.Second)
		if delay, ok := parseRelativeDelay(text); ok {
			runAt = now.Add(delay)
		} else if strings.TrimSpace(args.RunAt) != "" {
			parsed, parseErr := time.Parse(time.RFC3339, args.RunAt)
			if parseErr != nil {
				parsed, parseErr = time.ParseInLocation("2006-01-02 15:04", args.RunAt, location)
				if parseErr != nil {
					return true, "Не смог разобрать время напоминания.", nil
				}
			}
			runAt = parsed.UTC()
		}
		if !runAt.After(now) {
			return true, "Время напоминания уже прошло.", nil
		}
		task := store.Task{
			ID:        fmt.Sprintf("reminder-%d", time.Now().UnixNano()),
			UserID:    userID,
			ChatID:    updateChatID(update),
			Kind:      "reminder",
			Text:      strings.TrimSpace(args.Message),
			NextRunAt: runAt,
			Active:    true,
			CreatedAt: now,
		}
		if err := a.store.SaveTask(task); err != nil {
			return true, "", err
		}
		answer = fmt.Sprintf("Поставил напоминание на `%s` (%s).", runAt.In(location).Format("02.01.2006 15:04"), location.String())
	case "schedule_research":
		var args struct {
			Instruction  string `json:"instruction"`
			DelaySeconds int64  `json:"delay_seconds"`
			RunAt        string `json:"run_at"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil || strings.TrimSpace(args.Instruction) == "" {
			return true, "Не смог разобрать отложенную проверку.", nil
		}
		if args.DelaySeconds <= 0 && strings.TrimSpace(args.RunAt) == "" {
			if depth < 4 {
				replanText := text + "\n\nСистемное наблюдение: отложенное исследование нельзя запустить без времени. Если пользователь просит ответ сейчас, используй web_search вместо schedule_research."
				return a.handleMistralAutomation(ctx, update, userID, replanText, progress, depth+1)
			}
			return true, "Не удалось определить время выполнения задачи.", nil
		}
		runAt := now.Add(time.Duration(args.DelaySeconds) * time.Second)
		if delay, ok := parseRelativeDelay(text); ok {
			runAt = now.Add(delay)
		} else if strings.TrimSpace(args.RunAt) != "" {
			parsed, parseErr := time.Parse(time.RFC3339, args.RunAt)
			if parseErr != nil {
				parsed, parseErr = time.ParseInLocation("2006-01-02 15:04", args.RunAt, location)
				if parseErr != nil {
					return true, "Не смог разобрать время отложенной проверки.", nil
				}
			}
			runAt = parsed.UTC()
		}
		if !runAt.After(now) {
			return true, "Время отложенной проверки уже прошло.", nil
		}
		task := store.Task{
			ID:        fmt.Sprintf("research-%d", time.Now().UnixNano()),
			UserID:    userID,
			ChatID:    updateChatID(update),
			Kind:      "scheduled_research",
			Text:      strings.TrimSpace(args.Instruction),
			NextRunAt: runAt,
			Active:    true,
			CreatedAt: now,
		}
		if err := a.store.SaveTask(task); err != nil {
			return true, "", err
		}
		answer = fmt.Sprintf("Хорошо, проверю это через заданное время и напишу результат около `%s` (%s).", runAt.In(location).Format("02.01.2006 15:04"), location.String())
	case "watch_url":
		var args struct {
			URL             string `json:"url"`
			IntervalSeconds int64  `json:"interval_seconds"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return true, "Не смог разобрать ссылку для наблюдения.", nil
		}
		parsed, parseErr := url.Parse(strings.TrimSpace(args.URL))
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return true, "Нужна полноценная ссылка, начинающаяся с `https://` или `http://`.", nil
		}
		if args.IntervalSeconds < 60 {
			args.IntervalSeconds = 24 * 60 * 60
		}
		task := store.Task{
			ID:              fmt.Sprintf("watch-%d", time.Now().UnixNano()),
			UserID:          userID,
			ChatID:          updateChatID(update),
			Kind:            "watch_url",
			URL:             parsed.String(),
			IntervalSeconds: args.IntervalSeconds,
			NextRunAt:       now,
			Active:          true,
			CreatedAt:       now,
		}
		if err := a.store.SaveTask(task); err != nil {
			return true, "", err
		}
		answer = fmt.Sprintf("Буду проверять страницу каждые `%s` и сообщать об изменениях.", formatInterval(args.IntervalSeconds))
	case "list_tasks":
		answer = a.tasksText(userID)
	case "cancel_task":
		var args struct {
			TaskID      string `json:"task_id"`
			Description string `json:"description"`
			RunAt       string `json:"run_at"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return true, "Не смог понять, какую задачу удалить.", nil
		}
		deleted, ambiguous := a.cancelMatchingTask(userID, args.TaskID, args.Description, args.RunAt)
		if ambiguous {
			return true, "Нашел несколько похожих задач. Уточни время или текст напоминания.", nil
		}
		if !deleted {
			return true, "Не нашел активную задачу по этому описанию.", nil
		}
		answer = "Задача удалена."
	case "schedule_daily_digest":
		var args struct {
			LocalTime string `json:"local_time"`
		}
		if err := decodeToolArguments(call.Arguments, &args); err != nil {
			return true, "Не смог разобрать время ежедневной сводки.", nil
		}
		hour, minute, parseErr := parseLocalTime(args.LocalTime)
		if parseErr != nil {
			return true, "Не смог разобрать время ежедневной сводки.", nil
		}
		localNow := time.Now().In(location)
		nextLocal := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, location)
		if !nextLocal.After(localNow) {
			nextLocal = nextLocal.AddDate(0, 0, 1)
		}
		task := store.Task{
			ID:        fmt.Sprintf("digest-%d", time.Now().UnixNano()),
			UserID:    userID,
			ChatID:    updateChatID(update),
			Kind:      "daily_digest",
			NextRunAt: nextLocal.UTC(),
			Active:    true,
			CreatedAt: now,
		}
		if err := a.store.SaveTask(task); err != nil {
			return true, "", err
		}
		answer = fmt.Sprintf("Хорошо, буду каждый день присылать задачи на этот день в `%02d:%02d`.", hour, minute)
	case "reset_context", "context_reset":
		if err := a.store.ClearHistory(userID); err != nil {
			return true, "", err
		}
		if err := a.store.ClearDocuments(userID); err != nil {
			return true, "", err
		}
		return true, contextResetSignal, nil
	default:
		return false, "", nil
	}
	userMessage := mistral.Message{Role: "user", Content: text}
	if err := a.store.AppendMessages(userID, userMessage, mistral.Message{Role: "assistant", Content: answer}); err != nil {
		return true, "", err
	}
	return true, answer, nil
}

func decodeToolArguments(raw json.RawMessage, target any) error {
	if err := json.Unmarshal(raw, target); err == nil {
		return nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil {
		return err
	}
	return json.Unmarshal([]byte(encoded), target)
}

func parseLocalTime(value string) (int, int, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(value))
	if err != nil {
		return 0, 0, err
	}
	return parsed.Hour(), parsed.Minute(), nil
}

func parseRelativeDelay(text string) (time.Duration, bool) {
	parts := strings.Fields(strings.ToLower(text))
	for i, part := range parts {
		if strings.Trim(part, ",.!?") != "через" || i+1 >= len(parts) {
			continue
		}
		value := strings.Trim(parts[i+1], ",.!?")
		if value == "полчаса" {
			return 30 * time.Minute, true
		}
		if value == "минуту" || value == "минутка" {
			return time.Minute, true
		}
		if value == "час" {
			return time.Hour, true
		}
		if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
			return duration, true
		}
		amount, err := strconv.Atoi(value)
		if err != nil || amount <= 0 || i+2 >= len(parts) {
			continue
		}
		switch strings.Trim(parts[i+2], ",.!?") {
		case "с", "сек", "секунда", "секунды", "секунд":
			return time.Duration(amount) * time.Second, true
		case "м", "мин", "минута", "минуты", "минут":
			return time.Duration(amount) * time.Minute, true
		case "ч", "час", "часа", "часов":
			return time.Duration(amount) * time.Hour, true
		case "день", "дня", "дней":
			return time.Duration(amount) * 24 * time.Hour, true
		}
	}
	return 0, false
}

func (a *Agent) createReminder(ctx context.Context, update maxapi.Update, userID int64, text string) error {
	parts := strings.Fields(text)
	if len(parts) < 3 {
		return a.send(ctx, update, "Формат: `/remind 10m текст` или `/remind 2026-07-20 10:00 текст`")
	}
	now := time.Now().UTC()
	textStart := 2
	var runAt time.Time
	if duration, err := time.ParseDuration(parts[1]); err == nil {
		runAt = now.Add(duration)
	} else if len(parts) >= 4 {
		parsed, parseErr := time.ParseInLocation("2006-01-02 15:04", parts[1]+" "+parts[2], a.userLocation(userID))
		if parseErr != nil {
			return a.send(ctx, update, "Не понял дату. Используйте `10m` или формат `2026-07-20 10:00`.")
		}
		runAt = parsed.UTC()
		textStart = 3
	} else {
		return a.send(ctx, update, "Не понял время. Используйте `10m` или формат `2026-07-20 10:00`.")
	}
	if !runAt.After(now) {
		return a.send(ctx, update, "Время напоминания уже прошло.")
	}
	taskText := strings.TrimSpace(strings.Join(parts[textStart:], " "))
	task := store.Task{
		ID:        fmt.Sprintf("reminder-%d", time.Now().UnixNano()),
		UserID:    userID,
		ChatID:    updateChatID(update),
		Kind:      "reminder",
		Text:      taskText,
		NextRunAt: runAt,
		Active:    true,
		CreatedAt: now,
	}
	if err := a.store.SaveTask(task); err != nil {
		return err
	}
	return a.send(ctx, update, fmt.Sprintf("Напомню: `%s`\nID задачи: `%s`", runAt.Local().Format("02.01.2006 15:04"), task.ID))
}

func (a *Agent) createURLWatch(ctx context.Context, update maxapi.Update, userID int64, text string) error {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return a.send(ctx, update, "Формат: `/watch https://example.com daily`")
	}
	parsed, err := url.Parse(parts[1])
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return a.send(ctx, update, "Нужна полноценная ссылка, начинающаяся с `https://` или `http://`.")
	}
	seconds := int64(24 * time.Hour / time.Second)
	if len(parts) >= 3 {
		switch strings.ToLower(parts[2]) {
		case "hourly", "час":
			seconds = int64(time.Hour / time.Second)
		case "daily", "день", "ежедневно":
		default:
			duration, durationErr := time.ParseDuration(parts[2])
			if durationErr != nil || duration < time.Minute {
				return a.send(ctx, update, "Интервал: `hourly`, `daily` или, например, `6h`.")
			}
			seconds = int64(duration / time.Second)
		}
	}
	now := time.Now().UTC()
	task := store.Task{
		ID:              fmt.Sprintf("watch-%d", time.Now().UnixNano()),
		UserID:          userID,
		ChatID:          updateChatID(update),
		Kind:            "watch_url",
		URL:             parsed.String(),
		IntervalSeconds: seconds,
		NextRunAt:       now,
		Active:          true,
		CreatedAt:       now,
	}
	if err := a.store.SaveTask(task); err != nil {
		return err
	}
	return a.send(ctx, update, fmt.Sprintf("Буду проверять страницу каждые `%s`.", formatInterval(seconds)))
}

func (a *Agent) cancelTask(ctx context.Context, update maxapi.Update, userID int64, text string) error {
	parts := strings.Fields(text)
	if len(parts) != 2 {
		return a.send(ctx, update, "Формат: `/cancel ID задачи`")
	}
	if !a.store.DeleteTask(userID, parts[1]) {
		return a.send(ctx, update, "Активная задача с таким ID не найдена.")
	}
	return a.send(ctx, update, "Задача отменена.")
}

func (a *Agent) cancelMatchingTask(userID int64, taskID, description, runAt string) (bool, bool) {
	if taskID != "" && a.store.DeleteTask(userID, strings.TrimSpace(taskID)) {
		return true, false
	}
	target := normalizeTaskText(description)
	if target == "" {
		return false, false
	}
	var targetTime time.Time
	if strings.TrimSpace(runAt) != "" {
		targetTime, _ = time.Parse(time.RFC3339, strings.TrimSpace(runAt))
		if targetTime.IsZero() {
			targetTime, _ = time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(runAt), a.userLocation(userID))
		}
	}
	var matches []store.Task
	for _, task := range a.store.Tasks(userID) {
		candidate := normalizeTaskText(task.Text)
		if candidate == "" || (candidate != target && !strings.Contains(candidate, target) && !strings.Contains(target, candidate)) {
			continue
		}
		if !targetTime.IsZero() && !task.NextRunAt.In(a.userLocation(userID)).Truncate(time.Minute).Equal(targetTime.In(a.userLocation(userID)).Truncate(time.Minute)) {
			continue
		}
		matches = append(matches, task)
	}
	if len(matches) != 1 {
		return false, len(matches) > 1
	}
	return a.store.DeleteTask(userID, matches[0].ID), false
}

func normalizeTaskText(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= 'а' && r <= 'я') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func (a *Agent) listTasks(ctx context.Context, update maxapi.Update, userID int64) error {
	_, err := a.max.SendMessageWithKeyboard(ctx, updateChatID(update), userID, a.tasksText(userID), a.tasksKeyboard(userID))
	return err
}

func (a *Agent) sendMainMenu(ctx context.Context, update maxapi.Update) error {
	userID := updateUserID(update)
	text := "Готов помочь. Выберите действие или просто напишите просьбу обычным текстом."
	_, err := a.max.SendMessageWithKeyboard(ctx, updateChatID(update), userID, text, mainMenuKeyboard())
	return err
}

func (a *Agent) hasTimezone(userID int64) bool {
	_, err := loadUserLocation(a.store.Timezone(userID))
	return err == nil
}

func (a *Agent) profileComplete(userID int64) bool {
	return a.hasTimezone(userID) && strings.TrimSpace(a.store.HomeLocation(userID)) != ""
}

func (a *Agent) beginProfileSetup(ctx context.Context, update maxapi.Update, userID int64) error {
	a.setPending(userID, pendingInput{kind: "profile"})
	var text string
	switch {
	case !a.hasTimezone(userID) && strings.TrimSpace(a.store.HomeLocation(userID)) == "":
		text = "Привет! Я Люми. Чтобы правильно ставить напоминания и смотреть погоду, напиши одним сообщением город и текущее время.\n\nНапример: `Москва, сейчас 15:40` или `Амстердам 09:20`."
	case !a.hasTimezone(userID):
		text = "Город сохранил. Теперь напиши, сколько сейчас времени у тебя, например `15:40`, чтобы определить часовой пояс."
	default:
		text = "Напиши город, в котором обычно находишься, например `Москва`. Он будет использоваться для прогноза погоды."
	}
	return a.send(ctx, update, text)
}

func (a *Agent) userLocation(userID int64) *time.Location {
	location, err := loadUserLocation(a.store.Timezone(userID))
	if err != nil {
		return time.UTC
	}
	return location
}

func loadUserLocation(zone string) (*time.Location, error) {
	if strings.HasPrefix(zone, "UTC+") || strings.HasPrefix(zone, "UTC-") {
		if len(zone) != len("UTC+00:00") {
			return nil, fmt.Errorf("invalid fixed timezone")
		}
		hour, hourErr := strconv.Atoi(zone[4:6])
		minute, minuteErr := strconv.Atoi(zone[7:9])
		if hourErr != nil || minuteErr != nil || minute > 59 || hour > 14 {
			return nil, fmt.Errorf("invalid fixed timezone")
		}
		offset := (hour*60 + minute) * 60
		if zone[3] == '-' {
			offset = -offset
		}
		return time.FixedZone(zone, offset), nil
	}
	return time.LoadLocation(zone)
}

func timezoneKeyboard() [][]maxapi.KeyboardButton {
	return [][]maxapi.KeyboardButton{
		{{Type: "callback", Text: "🇷🇺 Москва", Payload: "ui:tz:Europe/Moscow"}, {Type: "callback", Text: "🇷🇺 Калининград", Payload: "ui:tz:Europe/Kaliningrad"}},
		{{Type: "callback", Text: "🇷🇺 Екатеринбург", Payload: "ui:tz:Asia/Yekaterinburg"}, {Type: "callback", Text: "🇷🇺 Владивосток", Payload: "ui:tz:Asia/Vladivostok"}},
		{{Type: "callback", Text: "🕐 Указать текущее время", Payload: "ui:tz:clock"}},
		{{Type: "callback", Text: "🌍 Другой часовой пояс", Payload: "ui:tz:custom"}},
	}
}

func (a *Agent) sendTimezoneSetup(ctx context.Context, update maxapi.Update, userID int64) error {
	text := "Перед началом выберите ваш часовой пояс. Я буду использовать его для напоминаний и ежедневных сводок."
	if zone := a.store.Timezone(userID); zone != "" {
		text = "Текущий часовой пояс: `" + zone + "`. Выберите новый, если нужно изменить настройку."
	} else {
		text += "\n\nЕсли вы в Москве, выберите «Москва»."
	}
	_, err := a.max.SendMessageWithKeyboard(ctx, updateChatID(update), userID, text, timezoneKeyboard())
	return err
}

func (a *Agent) handleTimezoneRequired(ctx context.Context, update maxapi.Update, userID int64) error {
	return a.sendTimezoneSetup(ctx, update, userID)
}

func mainMenuKeyboard() [][]maxapi.KeyboardButton {
	return [][]maxapi.KeyboardButton{
		{{Type: "callback", Text: "🔎 Найти информацию", Payload: "ui:search"}},
		{{Type: "callback", Text: "⏰ Напоминания", Payload: "ui:tasks"}, {Type: "callback", Text: "➕ Добавить", Payload: "ui:add"}},
		{{Type: "callback", Text: "🌐 Следить за страницей", Payload: "ui:watch"}, {Type: "callback", Text: "📊 Расход токенов", Payload: "ui:usage"}},
		{{Type: "callback", Text: "🔗 Подключения", Payload: "ui:connections"}, {Type: "callback", Text: "🕒 Часовой пояс", Payload: "ui:timezone"}},
		{{Type: "callback", Text: "❓ Помощь", Payload: "ui:help"}},
	}
}

func (a *Agent) tasksKeyboard(userID int64) [][]maxapi.KeyboardButton {
	tasks := a.store.Tasks(userID)
	buttons := make([][]maxapi.KeyboardButton, 0, len(tasks)+2)
	for i, task := range tasks {
		label := fmt.Sprintf("%d", i+1)
		buttons = append(buttons, []maxapi.KeyboardButton{
			{Type: "callback", Text: "✏️ Изменить #" + label, Payload: "ui:edit:" + task.ID},
			{Type: "callback", Text: "🗑 Удалить #" + label, Payload: "ui:delete:" + task.ID},
		})
	}
	buttons = append(buttons,
		[]maxapi.KeyboardButton{{Type: "callback", Text: "➕ Новое напоминание", Payload: "ui:add"}},
		[]maxapi.KeyboardButton{{Type: "callback", Text: "🏠 Главное меню", Payload: "ui:menu"}},
	)
	return buttons
}

func (a *Agent) handleCallback(ctx context.Context, update maxapi.Update) error {
	userID := updateUserID(update)
	a.logger.Info("received MAX callback", "user_id", userID, "payload", update.CallbackPayload(), "callback_id", update.CallbackID())
	if !a.allowed(userID) {
		return a.max.AnswerCallback(ctx, update.CallbackID(), "Бот доступен только владельцу")
	}
	payload := update.CallbackPayload()
	_ = a.max.AnswerCallback(ctx, update.CallbackID(), "")
	switch {
	case payload == "ui:tz:cancel":
		a.clearPending(userID)
		return a.sendTimezoneSetup(ctx, update, userID)
	case payload == "ui:tz:clock":
		a.setPending(userID, pendingInput{kind: "timezone_clock"})
		_, err := a.max.SendMessageWithKeyboard(ctx, updateChatID(update), userID, "Напишите, сколько сейчас времени у вас, в формате `15:40`. Я вычислю разницу с UTC.", [][]maxapi.KeyboardButton{{{Type: "callback", Text: "Отмена", Payload: "ui:tz:cancel"}}})
		return err
	case strings.HasPrefix(payload, "ui:tz:"):
		zone := strings.TrimPrefix(payload, "ui:tz:")
		if zone == "custom" {
			a.setPending(userID, pendingInput{kind: "timezone"})
			_, err := a.max.SendMessageWithKeyboard(ctx, updateChatID(update), userID, "Напишите часовой пояс в формате IANA, например `Europe/Moscow`, `Asia/Almaty` или `Europe/Amsterdam`.", [][]maxapi.KeyboardButton{{{Type: "callback", Text: "Отмена", Payload: "ui:tz:cancel"}}})
			return err
		}
		if _, err := time.LoadLocation(zone); err != nil {
			return a.sendTimezoneSetup(ctx, update, userID)
		}
		if err := a.store.SetTimezone(userID, zone); err != nil {
			return err
		}
		a.clearPending(userID)
		return a.sendMainMenu(ctx, update)
	case payload == "ui:menu":
		return a.sendMainMenu(ctx, update)
	case payload == "ui:tasks":
		return a.listTasks(ctx, update, userID)
	case payload == "ui:add":
		a.setPending(userID, pendingInput{kind: "create"})
		_, err := a.max.SendMessageWithKeyboard(ctx, updateChatID(update), userID, "Напишите одним сообщением, что и когда напомнить.\n\nНапример: `через 30 минут позвонить маме` или `2026-07-25 10:00 забрать заказ`", [][]maxapi.KeyboardButton{{{Type: "callback", Text: "Отмена", Payload: "ui:cancel"}}})
		return err
	case payload == "ui:search":
		return a.send(ctx, update, "Напишите, что найти — я сам выберу нужный поиск.")
	case payload == "ui:watch":
		return a.send(ctx, update, "Пришлите ссылку и интервал, например: `https://example.com 6h`.")
	case payload == "ui:usage":
		usage := a.store.Usage(userID)
		return a.send(ctx, update, fmt.Sprintf("Токены:\n\n- prompt: `%d`\n- completion: `%d`\n- total: `%d`", usage.PromptTokens, usage.CompletionTokens, usage.TotalTokens))
	case payload == "ui:timezone":
		return a.sendTimezoneSetup(ctx, update, userID)
	case payload == "ui:connections":
		if a.tools.HasGoogleAccount(userID) {
			return a.send(ctx, update, "Подключено: Google Gmail/Calendar.")
		}
		return a.send(ctx, update, "Подключений нет. Используйте `/connect_google`.")
	case payload == "ui:help":
		return a.send(ctx, update, "Я умею искать информацию, работать с документами, ставить напоминания, следить за страницами и подключать Google. Просто напишите задачу обычным текстом.")
	case payload == "ui:cancel":
		a.clearPending(userID)
		return a.listTasks(ctx, update, userID)
	case strings.HasPrefix(payload, "ui:delete:"):
		id := strings.TrimPrefix(payload, "ui:delete:")
		if !a.store.DeleteTask(userID, id) {
			return a.send(ctx, update, "Задача уже удалена или не найдена.")
		}
		return a.listTasks(ctx, update, userID)
	case strings.HasPrefix(payload, "ui:edit:"):
		id := strings.TrimPrefix(payload, "ui:edit:")
		a.setPending(userID, pendingInput{kind: "edit", taskID: id})
		_, err := a.max.SendMessageWithKeyboard(ctx, updateChatID(update), userID, "Напишите новое время и текст напоминания одним сообщением.\n\nНапример: `через 2 часа проверить почту`.", [][]maxapi.KeyboardButton{{{Type: "callback", Text: "Отмена", Payload: "ui:cancel"}}})
		return err
	default:
		return a.send(ctx, update, "Не понял действие. Откройте главное меню.")
	}
}

func updateUserID(update maxapi.Update) int64 {
	return update.EffectiveUserID()
}

func (a *Agent) setPending(userID int64, input pendingInput) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	a.pending[userID] = input
}

func (a *Agent) clearPending(userID int64) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	delete(a.pending, userID)
}

func (a *Agent) pendingFor(userID int64) (pendingInput, bool) {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	input, ok := a.pending[userID]
	return input, ok
}

func (a *Agent) handlePendingInput(ctx context.Context, update maxapi.Update, userID int64, text string) (bool, error) {
	input, ok := a.pendingFor(userID)
	if !ok {
		return false, nil
	}
	if strings.EqualFold(strings.TrimSpace(text), "/start") {
		a.clearPending(userID)
		return true, a.sendMainMenu(ctx, update)
	}
	if input.kind == "profile" {
		clockText := ""
		if match := profileClockPattern.FindStringSubmatch(text); len(match) == 3 {
			clockText = match[1] + ":" + match[2]
		}
		city := cleanProfileLocation(profileClockPattern.ReplaceAllString(text, ""))
		if city != "" {
			if err := a.store.SetHomeLocation(userID, city); err != nil {
				return true, err
			}
		}
		if clockText != "" {
			zone, err := inferFixedTimezone(clockText, time.Now().UTC())
			if err != nil {
				return true, a.send(ctx, update, "Не смог определить часовой пояс по этому времени. Напиши время в формате `15:40`.")
			}
			if err := a.store.SetTimezone(userID, zone); err != nil {
				return true, err
			}
		}
		if strings.TrimSpace(a.store.HomeLocation(userID)) == "" {
			return true, a.send(ctx, update, "В каком городе ты обычно находишься? Например: `Москва`.")
		}
		if !a.hasTimezone(userID) {
			return true, a.send(ctx, update, "Сколько сейчас времени у тебя? Например: `15:40`.")
		}
		a.clearPending(userID)
		return true, a.sendMainMenu(ctx, update)
	}
	if input.kind == "timezone" {
		zone := strings.TrimSpace(text)
		if _, err := time.LoadLocation(zone); err != nil {
			return true, a.send(ctx, update, "Не нашел такой часовой пояс. Пример: `Europe/Moscow` или `Europe/Amsterdam`.")
		}
		if err := a.store.SetTimezone(userID, zone); err != nil {
			return true, err
		}
		a.clearPending(userID)
		return true, a.sendMainMenu(ctx, update)
	}
	if input.kind == "timezone_clock" {
		zone, err := inferFixedTimezone(text, time.Now().UTC())
		if err != nil {
			return true, a.send(ctx, update, "Не понял время. Напишите его в формате `15:40`.")
		}
		if err := a.store.SetTimezone(userID, zone); err != nil {
			return true, err
		}
		a.clearPending(userID)
		return true, a.send(ctx, update, "Сохранил часовой пояс `"+zone+"`. Теперь можно пользоваться ботом.")
	}
	if strings.EqualFold(strings.TrimSpace(text), "отмена") || strings.EqualFold(strings.TrimSpace(text), "/cancel") {
		a.clearPending(userID)
		return true, a.listTasks(ctx, update, userID)
	}
	runAt, reminderText, err := parseReminderInput(text, a.userLocation(userID))
	if err != nil {
		return true, a.send(ctx, update, "Не понял формат. Пример: `через 30 минут позвонить` или `2026-07-25 10:00 забрать заказ`.")
	}
	if input.kind == "edit" {
		tasks := a.store.Tasks(userID)
		var task *store.Task
		for i := range tasks {
			if tasks[i].ID == input.taskID {
				copy := tasks[i]
				task = &copy
				break
			}
		}
		if task == nil {
			a.clearPending(userID)
			return true, a.send(ctx, update, "Напоминание уже удалено или не найдено.")
		}
		task.Text, task.NextRunAt = reminderText, runAt
		if err := a.store.SaveTask(*task); err != nil {
			return true, err
		}
	}
	if input.kind == "create" {
		task := store.Task{ID: fmt.Sprintf("reminder-%d", time.Now().UnixNano()), UserID: userID, ChatID: updateChatID(update), Kind: "reminder", Text: reminderText, NextRunAt: runAt, Active: true, CreatedAt: time.Now().UTC()}
		if err := a.store.SaveTask(task); err != nil {
			return true, err
		}
	}
	a.clearPending(userID)
	return true, a.listTasks(ctx, update, userID)
}

var profileClockPattern = regexp.MustCompile(`\b([01]?\d|2[0-3]):([0-5]\d)\b`)

func cleanProfileLocation(text string) string {
	value := strings.TrimSpace(text)
	for _, phrase := range []string{"сейчас", "текущее время", "мое время", "моё время", "сколько времени", "я в", "нахожусь в", "город"} {
		value = strings.ReplaceAll(strings.ToLower(value), phrase, "")
	}
	value = strings.TrimSpace(strings.Trim(value, " ,.;:-"))
	value = strings.TrimPrefix(value, "в ")
	return strings.TrimSpace(strings.Trim(value, " ,.;:-"))
}

func parseReminderInput(text string, location *time.Location) (time.Time, string, error) {
	parts := strings.Fields(strings.TrimSpace(text))
	if len(parts) < 2 {
		return time.Time{}, "", fmt.Errorf("missing reminder text")
	}
	if strings.EqualFold(parts[0], "через") {
		if len(parts) < 3 {
			return time.Time{}, "", fmt.Errorf("missing delay")
		}
		if delay, ok := parseRelativeDelay(strings.Join(parts[:3], " ")); ok && len(parts) >= 4 {
			return time.Now().UTC().Add(delay), strings.TrimSpace(strings.Join(parts[3:], " ")), nil
		}
	}
	if duration, err := time.ParseDuration(parts[0]); err == nil && len(parts) >= 2 {
		return time.Now().UTC().Add(duration), strings.TrimSpace(strings.Join(parts[1:], " ")), nil
	}
	if len(parts) >= 3 {
		parsed, err := time.ParseInLocation("2006-01-02 15:04", parts[0]+" "+parts[1], location)
		if err == nil {
			return parsed.UTC(), strings.TrimSpace(strings.Join(parts[2:], " ")), nil
		}
	}
	return time.Time{}, "", fmt.Errorf("invalid reminder input")
}

func inferFixedTimezone(text string, nowUTC time.Time) (string, error) {
	parsed, err := time.Parse("15:04", strings.TrimSpace(text))
	if err != nil {
		return "", err
	}
	currentMinutes := nowUTC.Hour()*60 + nowUTC.Minute()
	askedMinutes := parsed.Hour()*60 + parsed.Minute()
	diff := askedMinutes - currentMinutes
	if diff > 12*60 {
		diff -= 24 * 60
	}
	if diff < -12*60 {
		diff += 24 * 60
	}
	if diff < -12*60 || diff > 14*60 {
		return "", fmt.Errorf("timezone offset out of range")
	}
	sign := "+"
	absolute := diff
	if diff < 0 {
		sign = "-"
		absolute = -diff
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, absolute/60, absolute%60), nil
}

func (a *Agent) tasksText(userID int64) string {
	tasks := a.store.Tasks(userID)
	if len(tasks) == 0 {
		return "Активных задач нет."
	}
	var b strings.Builder
	b.WriteString("Активные задачи:")
	for _, task := range tasks {
		b.WriteString(fmt.Sprintf("\n- %s", taskDescription(task, a.userLocation(userID))))
	}
	return b.String()
}

func updateChatID(update maxapi.Update) int64 {
	if update.ChatID != nil {
		return *update.ChatID
	}
	if update.Message != nil {
		return update.Message.EffectiveChatID()
	}
	return 0
}

func formatInterval(seconds int64) string {
	if seconds%(24*60*60) == 0 {
		return strconv.FormatInt(seconds/(24*60*60), 10) + "d"
	}
	if seconds%(60*60) == 0 {
		return strconv.FormatInt(seconds/(60*60), 10) + "h"
	}
	return strconv.FormatInt(seconds/60, 10) + "m"
}

func taskDescription(task store.Task, location *time.Location) string {
	if task.Kind == "reminder" {
		return "напоминание на " + task.NextRunAt.In(location).Format("02.01 15:04") + ": " + task.Text
	}
	return "проверка " + task.URL + " каждые " + formatInterval(task.IntervalSeconds)
}

func (a *Agent) replyIfAllowed(ctx context.Context, update maxapi.Update, userID int64, text string) error {
	if !a.allowed(userID) {
		return a.send(ctx, update, "Бот пока доступен только владельцу.")
	}
	return a.send(ctx, update, text)
}

func (a *Agent) allowed(userID int64) bool {
	return a.accessMode == "all" || userID == a.ownerUserID
}

func (a *Agent) askMistral(ctx context.Context, userID int64, text string) (string, error) {
	history := a.store.History(userID)
	if document, ok := a.store.ActiveDocument(userID); ok && strings.TrimSpace(document.URL) != "" && !containsURL(text) {
		return a.askDocument(ctx, userID, text, document.URL)
	}
	instructions := "Тебя зовут Люми. Всегда называй себя только Люми и никогда не упоминай никакие другие имена для себя. " +
		"Ты персональный AI-ассистент в мессенджере MAX. Отвечай по-русски, кратко и по делу. " +
		"Актуальная информация уже передается в запросе после отдельного шага поиска, если он был нужен. " +
		"Не выдумывай ссылки и источники. " +
		"Если пользователь просит поставить напоминание без времени, не говори, что это невозможно, а уточни время. " +
		"Если вопрос не требует поиска, отвечай без лишнего обращения к вебу. " +
		"Не упоминай провайдера, модель или внутренние инструменты, если пользователь прямо не просит технические детали."
	// Keep the guidance transient and do not persist it in the user's history.
	messages := []mistral.Message{{
		Role:    "user",
		Content: instructions,
	}}
	messages = append(messages, history...)
	userMessage := mistral.Message{Role: "user", Content: text}
	messages = append(messages, userMessage)

	resp, err := a.mistral.Complete(ctx, messages, nil)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "Сервис вернул пустой ответ.", nil
	}
	content := strings.TrimSpace(resp.Choices[0].FirstMessage().Content)
	if content == "" {
		content = "Сервис вернул пустой ответ."
	}
	answer := content

	if err := a.store.AppendMessages(userID, userMessage, mistral.Message{Role: "assistant", Content: answer}); err != nil {
		return "", err
	}
	if err := a.store.AddUsage(userID, resp.Usage); err != nil {
		return "", err
	}
	return answer, nil
}

func containsURL(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "http://") || strings.Contains(lower, "https://")
}

func (a *Agent) askDocument(ctx context.Context, userID int64, question, documentURL string) (string, error) {
	history := a.store.History(userID)
	resp, err := a.mistral.CompleteWithDocument(ctx, history, question, documentURL)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "Сервис вернул пустой ответ.", nil
	}
	answer := strings.TrimSpace(resp.Choices[0].FirstMessage().Content)
	if answer == "" {
		answer = "Сервис вернул пустой ответ."
	}
	userMessage := mistral.Message{Role: "user", Content: question}
	if err := a.store.AppendMessages(userID, userMessage, mistral.Message{Role: "assistant", Content: answer}); err != nil {
		return "", err
	}
	if err := a.store.AddUsage(userID, resp.Usage); err != nil {
		return "", err
	}
	return answer, nil
}

func recentDocumentQuestion(history []mistral.Message) string {
	limit := len(history) - 8
	if limit < 0 {
		limit = 0
	}
	for i := len(history) - 1; i >= limit; i-- {
		if history[i].Role != "user" {
			continue
		}
		text := strings.TrimSpace(history[i].Content)
		lower := strings.ToLower(text)
		if containsAny(lower, "файл", "документ", "pdf", "про что", "о чем", "кратко", "сумм") {
			return text
		}
		return ""
	}
	return ""
}

func withSources(content string, sources []search.Result) string {
	if len(sources) == 0 {
		return content
	}
	var b strings.Builder
	b.WriteString("🔎 Использовал `web_search`.\n\n")
	b.WriteString(content)
	b.WriteString("\n\nИсточники:")
	for i, source := range sources {
		if i >= 5 {
			break
		}
		label := sourceLabel(source)
		b.WriteString(fmt.Sprintf("\n%d. %s — [ссылка](%s)", i+1, label, source.URL))
	}
	return b.String()
}

func sourceLabel(source search.Result) string {
	if parsed, err := url.Parse(strings.TrimSpace(source.URL)); err == nil && parsed.Hostname() != "" {
		host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
		return host
	}
	if title := strings.TrimSpace(source.Title); title != "" {
		return title
	}
	return "источник"
}

func (a *Agent) send(ctx context.Context, update maxapi.Update, text string) error {
	_, err := a.sendMessage(ctx, update, text)
	return err
}

func (a *Agent) sendProgress(ctx context.Context, update maxapi.Update, text string) (*maxapi.Message, error) {
	return a.sendMessage(ctx, update, text)
}

func (a *Agent) updateProgress(ctx context.Context, progress *maxapi.Message, text string) {
	if progress == nil || progress.ID == "" {
		return
	}
	if err := a.max.EditMessage(ctx, progress.ID, text); err != nil {
		a.logger.Warn("progress message update failed", "message_id", progress.ID, "error", err)
	}
}

func (a *Agent) sendMessage(ctx context.Context, update maxapi.Update, text string) (*maxapi.Message, error) {
	chatID := int64(0)
	userID := updateUserID(update)
	if update.ChatID != nil {
		chatID = *update.ChatID
	}
	if update.Message != nil {
		if msgChatID := update.Message.EffectiveChatID(); chatID == 0 && msgChatID != 0 {
			chatID = msgChatID
		}
	}
	return a.max.SendMessage(ctx, chatID, userID, text)
}

func progressText(text string) string {
	lower := strings.ToLower(text)
	switch {
	case containsAny(lower, "почт", "gmail", "письм"):
		return "✉️ Проверяю почту…"
	case containsAny(lower, "календар", "встреч", "событи", "расписан"):
		return "📅 Проверяю календарь…"
	case containsAny(lower, "найди", "поиск", "интернет", "актуальн", "новост", "исследуй", "изучи"):
		return "🔎 Ищу информацию в интернете…"
	default:
		return "🧠 Планирую действия…"
	}
}

func containsAny(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
