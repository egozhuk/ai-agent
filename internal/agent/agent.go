package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
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
	}
}

func (a *Agent) HandleUpdate(ctx context.Context, update maxapi.Update) error {
	a.logger.Debug("received MAX update", "type", update.UpdateType, "has_message", update.Message != nil, "raw_bytes", len(update.Raw))
	if update.UpdateType == "bot_started" {
		userID := int64(0)
		if update.User != nil {
			userID = update.User.EffectiveID()
		}
		return a.replyIfAllowed(ctx, update, userID, "Бот запущен. Напишите задачу обычным текстом или `/usage`, чтобы посмотреть расход токенов.")
	}
	if update.UpdateType != "message_created" || update.Message == nil {
		if update.UpdateType != "message_created" {
			a.logger.Debug("ignored MAX update type", "type", update.UpdateType)
		} else {
			a.logger.Warn("message_created update has no message payload", "top_level_fields", maxapi.TopLevelFields(update.Raw))
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
		return a.send(ctx, update, "Готов. Я могу отвечать на вопросы, искать актуальную информацию в интернете и выполнять отложенные задачи.\n\nКоманды: `/remind`, `/watch`, `/reminders`, `/cancel`, `/clear_context`, `/usage`.")
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
	case "/reminders":
		return a.listTasks(ctx, update, userID)
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
	call, err := a.mistral.PlanAutomation(ctx, inputs)
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
				parsed, parseErr = time.ParseInLocation("2006-01-02 15:04", args.RunAt, time.Local)
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
		answer = fmt.Sprintf("Поставил напоминание на `%s`.", runAt.In(time.Local).Format("02.01.2006 15:04"))
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
				parsed, parseErr = time.ParseInLocation("2006-01-02 15:04", args.RunAt, time.Local)
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
		answer = fmt.Sprintf("Хорошо, проверю это через заданное время и напишу результат около `%s`.", runAt.In(time.Local).Format("02.01.2006 15:04"))
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
		localNow := time.Now().In(time.Local)
		nextLocal := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, time.Local)
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
		parsed, parseErr := time.ParseInLocation("2006-01-02 15:04", parts[1]+" "+parts[2], time.Local)
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
			targetTime, _ = time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(runAt), time.Local)
		}
	}
	var matches []store.Task
	for _, task := range a.store.Tasks(userID) {
		candidate := normalizeTaskText(task.Text)
		if candidate == "" || (candidate != target && !strings.Contains(candidate, target) && !strings.Contains(target, candidate)) {
			continue
		}
		if !targetTime.IsZero() && !task.NextRunAt.In(time.Local).Truncate(time.Minute).Equal(targetTime.In(time.Local).Truncate(time.Minute)) {
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
	return a.send(ctx, update, a.tasksText(userID))
}

func (a *Agent) tasksText(userID int64) string {
	tasks := a.store.Tasks(userID)
	if len(tasks) == 0 {
		return "Активных задач нет."
	}
	var b strings.Builder
	b.WriteString("Активные задачи:")
	for _, task := range tasks {
		b.WriteString(fmt.Sprintf("\n- %s", taskDescription(task)))
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

func taskDescription(task store.Task) string {
	if task.Kind == "reminder" {
		return "напоминание на " + task.NextRunAt.Local().Format("02.01 15:04") + ": " + task.Text
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
	instructions := "Ты персональный AI-ассистент в мессенджере MAX. Отвечай по-русски, кратко и по делу. " +
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
	userID := int64(0)
	if update.ChatID != nil {
		chatID = *update.ChatID
	}
	if update.Message != nil {
		if msgChatID := update.Message.EffectiveChatID(); chatID == 0 && msgChatID != 0 {
			chatID = msgChatID
		}
		userID = update.Message.EffectiveSenderID()
	}
	if userID == 0 && update.User != nil {
		userID = update.User.EffectiveID()
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
