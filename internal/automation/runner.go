package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"AI-agent/internal/maxapi"
	"AI-agent/internal/mistral"
	"AI-agent/internal/search"
	"AI-agent/internal/store"
)

type Runner struct {
	store      *store.Store
	max        *maxapi.Client
	mistral    *mistral.Client
	search     *search.Client
	logger     *slog.Logger
	httpClient *http.Client
}

func NewRunner(st *store.Store, max *maxapi.Client, mistralClient *mistral.Client, searchClient *search.Client, logger *slog.Logger) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		store:      st,
		max:        max,
		mistral:    mistralClient,
		search:     searchClient,
		logger:     logger,
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}
}

func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	r.logger.Info("automation runner started")
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.runDue(ctx, now.UTC())
		}
	}
}

func (r *Runner) runDue(ctx context.Context, now time.Time) {
	for _, task := range r.store.DueTasks(now) {
		switch task.Kind {
		case "reminder":
			r.runReminder(ctx, task)
		case "watch_url":
			r.runURLWatch(ctx, task, now)
		case "daily_digest":
			r.runDailyDigest(ctx, task, now)
		case "scheduled_research":
			r.runScheduledResearch(ctx, task)
		default:
			r.logger.Warn("unknown automation task", "task_id", task.ID, "kind", task.Kind)
		}
	}
}

func (r *Runner) runDailyDigest(ctx context.Context, task store.Task, now time.Time) {
	location := r.userLocation(task.UserID)
	localNow := now.In(location)
	tasks := r.store.TasksForDate(task.UserID, localNow)
	var b strings.Builder
	b.WriteString("📋 Задачи на сегодня:")
	if len(tasks) == 0 {
		b.WriteString("\n\nНа сегодня запланированных задач нет.")
	} else {
		for _, item := range tasks {
			b.WriteString("\n- " + r.taskDescription(item))
		}
	}
	if _, err := r.max.SendMessage(ctx, task.ChatID, task.UserID, b.String()); err != nil {
		r.logger.Warn("daily digest delivery failed", "task_id", task.ID, "error", err)
		return
	}
	nextLocal := task.NextRunAt.In(location)
	nextLocal = time.Date(nextLocal.Year(), nextLocal.Month(), nextLocal.Day()+1, nextLocal.Hour(), nextLocal.Minute(), 0, 0, location)
	task.NextRunAt = nextLocal.UTC()
	if err := r.store.SaveTask(task); err != nil {
		r.logger.Warn("daily digest state save failed", "task_id", task.ID, "error", err)
	}
}

func (r *Runner) taskDescription(task store.Task) string {
	if task.Kind == "reminder" {
		return "напоминание на " + task.NextRunAt.In(r.userLocation(task.UserID)).Format("15:04") + ": " + task.Text
	}
	return "проверка страницы: " + task.URL
}

func (r *Runner) userLocation(userID int64) *time.Location {
	location, err := loadLocation(r.store.Timezone(userID))
	if err != nil {
		return time.UTC
	}
	return location
}

func loadLocation(zone string) (*time.Location, error) {
	if strings.HasPrefix(zone, "UTC+") || strings.HasPrefix(zone, "UTC-") {
		if len(zone) != len("UTC+00:00") {
			return nil, fmt.Errorf("invalid fixed timezone")
		}
		hour, hourErr := strconv.Atoi(zone[4:6])
		minute, minuteErr := strconv.Atoi(zone[7:9])
		if hourErr != nil || minuteErr != nil || hour > 14 || minute > 59 {
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

func (r *Runner) runReminder(ctx context.Context, task store.Task) {
	text := fmt.Sprintf("🔔 Напоминание:\n\n%s", task.Text)
	if _, err := r.max.SendMessage(ctx, task.ChatID, task.UserID, text); err != nil {
		r.logger.Warn("reminder delivery failed", "task_id", task.ID, "error", err)
		return
	}
	task.Active = false
	if err := r.store.SaveTask(task); err != nil {
		r.logger.Warn("reminder state save failed", "task_id", task.ID, "error", err)
	}
}

func (r *Runner) runScheduledResearch(ctx context.Context, task store.Task) {
	progress, _ := r.max.SendMessage(ctx, task.ChatID, task.UserID, "🔎 Выполняю отложенную проверку…")
	message := "Не смог выполнить отложенную проверку."
	if r.search != nil && r.mistral != nil {
		resp, err := r.search.Search(ctx, task.Text)
		if err == nil {
			answer, answerErr := r.mistral.Complete(ctx, []mistral.Message{
				{Role: "system", Content: "Тебя зовут Люми. Называй себя только Люми и никогда не используй другие имена для себя. Ответь по-русски кратко по материалам поиска. Не выдумывай факты."},
				{Role: "user", Content: "Выполни отложенную задачу и сообщи результат:\n" + task.Text + "\n\nМатериалы:\n" + resp.Content},
			}, nil)
			if answerErr == nil && len(answer.Choices) > 0 {
				_ = r.store.AddUsage(task.UserID, answer.Usage)
				message = strings.TrimSpace(answer.Choices[0].FirstMessage().Content)
				if message == "" {
					message = "По отложенной проверке не удалось получить содержательный ответ."
				}
				message = "🔎 Результат отложенной проверки:\n\n" + message + formatSources(resp.Results)
			}
		}
	}
	if progress != nil && progress.ID != "" {
		_ = r.max.DeleteMessage(ctx, progress.ID)
	}
	if _, err := r.max.SendMessage(ctx, task.ChatID, task.UserID, message); err != nil {
		r.logger.Warn("scheduled research delivery failed", "task_id", task.ID, "error", err)
		return
	}
	task.Active = false
	if err := r.store.SaveTask(task); err != nil {
		r.logger.Warn("scheduled research state save failed", "task_id", task.ID, "error", err)
	}
}

func formatSources(sources []search.Result) string {
	if len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nИсточники:")
	for i, source := range sources {
		if i >= 5 {
			break
		}
		b.WriteString(fmt.Sprintf("\n%d. [ссылка](%s)", i+1, source.URL))
	}
	return b.String()
}

func (r *Runner) runURLWatch(ctx context.Context, task store.Task, now time.Time) {
	fingerprint, snapshot, err := r.pageFingerprint(ctx, task.URL)
	if err != nil {
		r.logger.Warn("url watch failed", "task_id", task.ID, "url", task.URL, "error", err)
		task.NextRunAt = now.Add(interval(task.IntervalSeconds))
		_ = r.store.SaveTask(task)
		return
	}
	if task.LastFingerprint == "" {
		task.LastFingerprint = fingerprint
		task.LastSnapshot = snapshot
	} else if task.LastFingerprint != fingerprint {
		progress, _ := r.max.SendMessage(ctx, task.ChatID, task.UserID, "🔎 Анализирую изменения страницы…")
		message := r.summarizeChange(ctx, task.UserID, task.URL, task.LastSnapshot, snapshot)
		if progress != nil && progress.ID != "" {
			_ = r.max.DeleteMessage(ctx, progress.ID)
		}
		if _, err := r.max.SendMessage(ctx, task.ChatID, task.UserID, message); err != nil {
			r.logger.Warn("url watch notification failed", "task_id", task.ID, "error", err)
		} else {
			task.LastFingerprint = fingerprint
			task.LastSnapshot = snapshot
		}
	}
	task.NextRunAt = now.Add(interval(task.IntervalSeconds))
	if err := r.store.SaveTask(task); err != nil {
		r.logger.Warn("url watch state save failed", "task_id", task.ID, "error", err)
	}
}

func (r *Runner) pageFingerprint(ctx context.Context, pageURL string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "AI-agent page monitor/1.0")
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("page returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", "", err
	}
	content := strings.Join(strings.Fields(string(body)), " ")
	if len(content) > 50000 {
		content = content[:50000]
	}
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:]), content, nil
}

func (r *Runner) summarizeChange(ctx context.Context, userID int64, pageURL, previous, current string) string {
	fallback := fmt.Sprintf("🌐 Страница изменилась:\n\n%s\n\n[Открыть страницу](%s)", pageURL, pageURL)
	if r.mistral == nil || strings.TrimSpace(previous) == "" {
		return fallback
	}
	messages := []mistral.Message{
		{Role: "system", Content: "Тебя зовут Люми. Называй себя только Люми и никогда не используй другие имена для себя. Ты анализируешь изменения на веб-странице. Отвечай по-русски кратко: что изменилось, без выдумок. Если данных недостаточно, так и скажи."},
		{Role: "user", Content: fmt.Sprintf("URL: %s\n\nПредыдущая версия:\n%s\n\nНовая версия:\n%s", pageURL, previous, current)},
	}
	resp, err := r.mistral.Complete(ctx, messages, nil)
	if err != nil || len(resp.Choices) == 0 {
		return fallback
	}
	_ = r.store.AddUsage(userID, resp.Usage)
	content := strings.TrimSpace(resp.Choices[0].FirstMessage().Content)
	if content == "" {
		return fallback
	}
	return "🌐 Страница изменилась:\n\n" + content + fmt.Sprintf("\n\n[Открыть страницу](%s)", pageURL)
}

func interval(seconds int64) time.Duration {
	if seconds < 60 {
		return time.Minute
	}
	return time.Duration(seconds) * time.Second
}
