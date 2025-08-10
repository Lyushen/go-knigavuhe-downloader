package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// MediaItem represents content item
type MediaItem struct {
	Title    string      `json:"title"`
	Creators interface{} `json:"creators"`
	Voice    interface{} `json:"voice"`
	Image    string      `json:"image"`
	Link     string      `json:"link"`
}

// AudioTrack represents an audio file
type AudioTrack struct {
	Name string `json:"name"`
	SRC  string `json:"src"`
}

// MediaContent contains all item information
type MediaContent struct {
	Item            MediaItem    `json:"item"`
	Tracks          []AudioTrack `json:"tracks"`
	CombinedTracks  []AudioTrack `json:"combined_tracks"`
	ImageBackup     string       `json:"-"`
	Info            string       `json:"-"`
}

const (
	maxAttempts    = 20
	retryWait      = 2 * time.Second
	maxConnections = 10
)

var (
	jsPattern     = regexp.MustCompile(`BookController\.enter\((.*?)\);`)
	infoPattern   = regexp.MustCompile(`bookDescription\">(.+?)</div>`)
	invalidPath   = regexp.MustCompile(`[<>:"/\\|?*]`)
	netTransport  = &http.Transport{
		MaxIdleConns:    maxConnections,
		IdleConnTimeout: 60 * time.Second,
	}
	netClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: netTransport,
	}
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: media-collector <target-dir> <source-file>")
		fmt.Println("  <target-dir>   : Directory for media storage")
		fmt.Println("  <source-file>  : File containing content URLs")
		os.Exit(1)
	}

	storageDir := os.Args[1]
	if err := os.MkdirAll(storageDir, 0755); err != nil {
		log.Fatalf("Directory creation error: %v", err)
	}

	links, err := readLinks(os.Args[2])
	if err != nil {
		log.Fatalf("URL read error: %v", err)
	}

	results := handleContent(storageDir, links)

	fmt.Println("\nProcessing Summary:")
	for _, res := range results {
		if len(res.Issues) == 0 {
			fmt.Printf("✅ %s\n   Location: %s\n", res.Title, res.Location)
		} else {
			fmt.Printf("❌ %s\n   Issues:\n", res.Title)
			for _, e := range res.Issues {
				fmt.Printf("    - %s\n", e)
			}
		}
	}
}

func readLinks(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if u := strings.TrimSpace(scanner.Text()); u != "" {
			urls = append(urls, u)
		}
	}
	return urls, scanner.Err()
}

func handleContent(storageDir string, urls []string) []ContentResult {
	var wg sync.WaitGroup
	queue := make(chan string, len(urls))
	results := make(chan ContentResult, len(urls))
	out := make([]ContentResult, 0, len(urls))

	workers := runtime.NumCPU()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range queue {
				results <- fetchContent(u, storageDir)
			}
		}()
	}

	for _, u := range urls {
		queue <- u
	}
	close(queue)

	go func() {
		wg.Wait()
		close(results)
	}()

	for res := range results {
		out = append(out, res)
	}
	return out
}

func fetchContent(contentURL, storageDir string) ContentResult {
	result := ContentResult{Source: contentURL}

	normalized, err := cleanURL(contentURL)
	if err != nil {
		result.addIssue("URL normalization", err)
		return result
	}

	media, err := getMediaData(normalized)
	if err != nil {
		result.addIssue("data retrieval", err)
		return result
	}

	contentDir := filepath.Join(storageDir, cleanPath(media.Item.Title))
	if err := os.MkdirAll(contentDir, 0755); err != nil {
		result.addIssue("directory setup", err)
		return result
	}

	result.Title = media.Item.Title
	result.Location = contentDir

	saveResources(contentDir, media, &result)

	return result
}

func getMediaData(mediaURL string) (*MediaContent, error) {
	html, err := getPage(mediaURL)
	if err != nil {
		return nil, fmt.Errorf("page fetch failed: %w", err)
	}

	jsonStr, err := getJSON(html)
	if err != nil {
		return nil, fmt.Errorf("JSON extract failed: %w", err)
	}

	content, err := parseMediaJSON(jsonStr)
	if err != nil {
		return nil, fmt.Errorf("JSON parse failed: %w", err)
	}

	content.ImageBackup = strings.Split(content.Item.Image, "-")[0] + ".jpg"

	if info, err := getInfo(html); err == nil {
		content.Info = info
	}

	return content, nil
}

func getPage(url string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	// Dynamic User-Agent generation
	agents := []string{
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
		"AppleWebKit/537.36 (KHTML, like Gecko)",
		"Chrome/135.0.0.0 Safari/537.36",
		"Edg/135.0.0.0",
	}
	req.Header.Set("User-Agent", strings.Join(agents, " "))

	resp, err := netClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func getJSON(html string) (string, error) {
	matches := jsPattern.FindStringSubmatch(html)
	if len(matches) < 2 {
		return "", errors.New("JSON pattern not found")
	}
	return matches[1], nil
}

func parseMediaJSON(jsonStr string) (*MediaContent, error) {
	var data MediaContent
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func getInfo(html string) (string, error) {
	matches := infoPattern.FindStringSubmatch(html)
	if len(matches) < 2 {
		return "", errors.New("info not found")
	}
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(matches[1], "")
	return text, nil
}

func cleanPath(path string) string {
	safe := invalidPath.ReplaceAllString(path, "_")
	safe = strings.Trim(safe, " .")
	reserved := []string{"CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"}

	for _, r := range reserved {
		if strings.EqualFold(safe, r) {
			return safe + "_"
		}
	}
	return safe
}

func saveResources(contentDir string, media *MediaContent, result *ContentResult) {
	// Save cover image
	if err := fetchResource(
		[]string{media.ImageBackup, media.Item.Image},
		filepath.Join(contentDir, "cover.jpg"),
	); err != nil {
		result.addIssue("image fetch", err)
	}

	// Save information file
	if err := saveInfo(contentDir, media); err != nil {
		result.addIssue("info save", err)
	}

	// Save audio tracks
	if err := saveAudio(contentDir, media, result); err != nil {
		result.addIssue("audio save", err)
	}
}

func fetchResource(sources []string, destination string) error {
	for _, src := range sources {
		if src == "" {
			continue
		}
		if err := saveFile(src, destination); err == nil {
			return nil
		}
	}
	return errors.New("all sources failed")
}

func saveInfo(contentDir string, media *MediaContent) error {
	getNames := func(data interface{}) string {
		var names []string
		switch v := data.(type) {
		case map[string]interface{}:
			for _, item := range v {
				if person, ok := item.(map[string]interface{}); ok {
					if name, ok := person["name"].(string); ok {
						names = append(names, name)
					}
				}
			}
		case []interface{}:
			for _, item := range v {
				if person, ok := item.(map[string]interface{}); ok {
					if name, ok := person["name"].(string); ok {
						names = append(names, name)
					}
				}
			}
		}
		return strings.Join(names, ", ")
	}

	info := fmt.Sprintf(
		"Title: %s\nCreators: %s\nNarrators: %s\n\nDescription:\n%s\n\nSource: %s",
		media.Item.Title,
		getNames(media.Item.Creators),
		getNames(media.Item.Voice),
		media.Info,
		media.Item.Link,
	)

	return os.WriteFile(filepath.Join(contentDir, "info.txt"), []byte(info), 0644)
}

func saveAudio(contentDir string, media *MediaContent, result *ContentResult) error {
	if err := processTracks(contentDir, media.Tracks); err == nil {
		return nil
	}
	result.addIssue("primary tracks", errors.New("using fallback tracks"))
	return processTracks(contentDir, media.CombinedTracks)
}

func processTracks(contentDir string, tracks []AudioTrack) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errChan := make(chan error, 1)
	limiter := make(chan struct{}, 4) // Concurrency limit

	for _, track := range tracks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		wg.Add(1)
		go func(t AudioTrack) {
			defer wg.Done()

			select {
			case limiter <- struct{}{}:
				defer func() { <-limiter }()
			case <-ctx.Done():
				return
			}

			ext := filepath.Ext(t.SRC)
			if ext == "" {
				ext = ".mp3"
			}
			filePath := filepath.Join(contentDir, cleanPath(t.Name)+ext)

			if _, err := os.Stat(filePath); err == nil {
				return
			}

			for attempt := 1; attempt <= maxAttempts; attempt++ {
				if err := saveFile(t.SRC, filePath); err == nil {
					return
				}
				time.Sleep(retryWait)
			}

			select {
			case errChan <- fmt.Errorf("track %s failed after %d attempts", t.Name, maxAttempts):
				cancel()
			default:
			}
		}(track)
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	if err := <-errChan; err != nil {
		return err
	}
	return nil
}

func saveFile(source, destination string) error {
	req, err := http.NewRequest("GET", source, nil)
	if err != nil {
		return err
	}

	// Dynamic User-Agent
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/98.0.4758.102 Safari/537.36")

	resp, err := netClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	return err
}

func cleanURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	if u.Host == "m.knigavuhe.org" {
		u.Host = "knigavuhe.org"
	}
	u.Scheme = "https"

	return u.String(), nil
}

// ContentResult contains processing status
type ContentResult struct {
	Source   string
	Title    string
	Location string
	Issues   []string
}

func (r *ContentResult) addIssue(context string, err error) {
	r.Issues = append(r.Issues, fmt.Sprintf("%s: %v", context, err))
}