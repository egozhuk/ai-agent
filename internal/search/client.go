package search

import (
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	htmlx "golang.org/x/net/html"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type response struct {
	Results []Result `json:"results"`
}

type Response struct {
	Content string
	Results []Result
}

func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), httpClient: httpClient}
}

func (c *Client) Search(ctx context.Context, query string) (Response, error) {
	if strings.TrimSpace(c.baseURL) == "" {
		return Response{}, fmt.Errorf("search API URL is not configured")
	}
	endpoint, err := url.Parse(c.baseURL + "/search")
	if err != nil {
		return Response{}, fmt.Errorf("parse search API URL: %w", err)
	}
	values := endpoint.Query()
	values.Set("q", strings.TrimSpace(query))
	values.Set("format", "json")
	values.Set("language", "ru-RU")
	values.Set("safesearch", "1")
	values.Set("pageno", "1")
	endpoint.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "MAX-AI-Agent/1.0")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("search request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, fmt.Errorf("search API returned HTTP %d", resp.StatusCode)
	}
	var payload response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&payload); err != nil {
		return Response{}, fmt.Errorf("decode search response: %w", err)
	}
	if len(payload.Results) == 0 {
		return Response{}, fmt.Errorf("search returned no results")
	}
	if len(payload.Results) > 3 {
		payload.Results = payload.Results[:3]
	}

	var b strings.Builder
	for i := range payload.Results {
		result := &payload.Results[i]
		text := strings.TrimSpace(result.Content)
		if page, fetchErr := c.fetchPage(ctx, result.URL); fetchErr == nil && len(page) > len(text) {
			text = page
		}
		if len(text) > 7000 {
			text = text[:7000]
		}
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "\n\nИСТОЧНИК %d: %s\nURL: %s\nМАТЕРИАЛ:\n%s", i+1, result.Title, result.URL, text)
	}
	if b.Len() == 0 {
		return Response{}, fmt.Errorf("search results contain no readable text")
	}
	return Response{Content: b.String(), Results: payload.Results}, nil
}

func (c *Client) fetchPage(ctx context.Context, rawURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid result URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("User-Agent", "MAX-AI-Agent/1.0")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("page returned HTTP %d", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); contentType != "" && !strings.Contains(contentType, "text/html") {
		return "", fmt.Errorf("unsupported content type")
	}
	doc, err := htmlx.Parse(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(*htmlx.Node, bool)
	walk = func(node *htmlx.Node, skip bool) {
		if node.Type == htmlx.ElementNode {
			switch node.Data {
			case "script", "style", "noscript", "svg", "nav", "footer", "header":
				skip = true
			}
		}
		if !skip && node.Type == htmlx.TextNode {
			text := strings.TrimSpace(stdhtml.UnescapeString(node.Data))
			if text != "" {
				b.WriteString(text)
				b.WriteByte(' ')
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, skip)
		}
	}
	walk(doc, false)
	return strings.Join(strings.Fields(b.String()), " "), nil
}
