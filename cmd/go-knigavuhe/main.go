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

// Person represents author/reader
type Person struct {
	Name string `json:"name"`
}

// Book represents book metadata
type Book struct {
	Name    string      `json:"name"`
	Authors interface{} `json:"authors"` // Flexible type: map or slice
	Readers interface{} `json:"readers"` // Flexible type: map or slice
	Cover   string      `json:"cover"`
	URL     string      `json:"url"`
}

// Audio represents an audio track
type Audio struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// BookData contains all book information
type BookData struct {
	Book           Book    `json:"book"`
	Playlist       []Audio `json:"playlist"`
	MergedPlaylist []Audio `json:"merged_playlist"`
	CoverAlt       string  `json:"-"`
	Description    string  `json:"-"`
}

const (
	userAgent    = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36 Edg/135.0.0.0"
	cookie       = "new_design=1"
	maxRetries   = 20
	retryDelay   = 2 * time.Second
	maxIdleConns = 10
)

var (
	jsonRegex      = regexp.MustCompile(`BookController\.enter\((.*?)\);`)
	descRegex      = regexp.MustCompile(`bookDescription\">(.+?)</div>`)
	forbiddenChars = regexp.MustCompile(`[<>:"/\\|?*]`)
	httpClient     = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:    maxIdleConns,
			IdleConnTimeout: 60 * time.Second,
		},
	}
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("Usage: audiobook-downloader <output-dir> <url-file>")
		fmt.Println("  <output-dir>   : Directory to save audiobooks")
		fmt.Println("  <url-file>     : File containing URLs (one per line)")
		os.Exit(1)
	}

	// Create output directory
	outputDir := os.Args[1]
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Error creating output directory: %v", err)
	}

	// Read URLs from file
	urls, err := readURLsFromFile(os.Args[2])
	if err != nil {
		log.Fatalf("Error reading URLs: %v", err)
	}

	// Process downloads
	results := processDownloads(outputDir, urls)

	// Print summary
	fmt.Println("\nDownload Summary:")
	for _, res := range results {
		if len(res.Errors) == 0 {
			fmt.Printf("✅ %s\n   Path: %s\n", res.BookName, res.Path)
		} else {
			fmt.Printf("❌ %s\n   Errors:\n", res.BookName)
			for _, e := range res.Errors {
				fmt.Printf("    - %s\n", e)
			}
		}
	}
}

func readURLsFromFile(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var urls []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if url := strings.TrimSpace(scanner.Text()); url != "" {
			urls = append(urls, url)
		}
	}
	return urls, scanner.Err()
}

func processDownloads(outputDir string, urls []string) []DownloadResult {
	var wg sync.WaitGroup
	bookChan := make(chan string, len(urls))
	resultChan := make(chan DownloadResult, len(urls))
	results := make([]DownloadResult, 0, len(urls))

	// Create worker pool
	workers := runtime.NumCPU() * 2
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for url := range bookChan {
				result := downloadBook(url, outputDir)
				resultChan <- result
			}
		}()
	}

	// Feed URLs to workers
	for _, url := range urls {
		bookChan <- url
	}
	close(bookChan)

	// Collect results
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	for res := range resultChan {
		results = append(results, res)
	}
	return results
}

func downloadBook(bookURL, outputDir string) DownloadResult {
	result := DownloadResult{URL: bookURL}

	// Normalize URL
	normalizedURL, err := normalizeURL(bookURL)
	if err != nil {
		result.addError("URL normalization", err)
		return result
	}

	bookData, err := fetchBookData(normalizedURL)
	if err != nil {
		result.addError("fetch book data", err)
		return result
	}

	// Create book directory
	bookDir := filepath.Join(outputDir, sanitizePath(bookData.Book.Name))
	if err := os.MkdirAll(bookDir, 0755); err != nil {
		result.addError("create directory", err)
		return result
	}

	result.BookName = bookData.Book.Name
	result.Path = bookDir

	// Download assets
	downloadAssets(bookDir, bookData, &result)

	return result
}

func fetchBookData(bookURL string) (*BookData, error) {
	// Fetch HTML page
	html, err := downloadPage(bookURL)
	if err != nil {
		return nil, fmt.Errorf("page download failed: %w", err)
	}

	// Extract JSON data
	jsonData, err := extractJSON(html)
	if err != nil {
		return nil, fmt.Errorf("JSON extraction failed: %w", err)
	}

	// Parse JSON
	bookData, err := parseBookJSON(jsonData)
	if err != nil {
		return nil, fmt.Errorf("JSON parsing failed: %w", err)
	}

	// Set alternative cover URL
	bookData.CoverAlt = strings.Split(bookData.Book.Cover, "-")[0] + ".jpg"

	// Extract description
	if desc, err := extractDescription(html); err == nil {
		bookData.Description = desc
	}

	return bookData, nil
}

func downloadPage(url string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Cookie", cookie)

	resp, err := httpClient.Do(req)
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

func extractJSON(html string) (string, error) {
	matches := jsonRegex.FindStringSubmatch(html)
	if len(matches) < 2 {
		return "", errors.New("JSON data not found in HTML")
	}
	return matches[1], nil
}

func parseBookJSON(jsonStr string) (*BookData, error) {
	var data BookData
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func extractDescription(html string) (string, error) {
	matches := descRegex.FindStringSubmatch(html)
	if len(matches) < 2 {
		return "", errors.New("description not found")
	}

	// Remove HTML tags by extracting text
	text := regexp.MustCompile(`<[^>]*>`).ReplaceAllString(matches[1], "")
	return text, nil
}

func sanitizePath(path string) string {
	// Remove forbidden characters
	safe := forbiddenChars.ReplaceAllString(path, "_")

	// Trim spaces and dots
	safe = strings.Trim(safe, " .")

	// Replace reserved names
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

func downloadAssets(bookDir string, bookData *BookData, result *DownloadResult) {
	// Download cover
	if err := downloadWithFallback(
		[]string{bookData.CoverAlt, bookData.Book.Cover},
		filepath.Join(bookDir, "cover.jpg"),
	); err != nil {
		result.addError("cover download", err)
	}

	// Save description
	if err := saveDescription(bookDir, bookData); err != nil {
		result.addError("save description", err)
	}

	// Download playlist
	if err := downloadPlaylist(bookDir, bookData, result); err != nil {
		result.addError("playlist download", err)
	}
}

func downloadWithFallback(urls []string, filePath string) error {
	for _, url := range urls {
		if url == "" {
			continue
		}
		if err := downloadFile(url, filePath); err == nil {
			return nil
		}
	}
	return errors.New("all download attempts failed")
}

func saveDescription(bookDir string, bookData *BookData) error {
	// Extract names from authors/readers
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

	desc := fmt.Sprintf(
		"Название: %s\nАвтор(ы): %s\nЧтец(ы): %s\n\nОписание:\n%s\n\nURL: %s",
		bookData.Book.Name,
		getNames(bookData.Book.Authors),
		getNames(bookData.Book.Readers),
		bookData.Description,
		bookData.Book.URL,
	)

	return os.WriteFile(filepath.Join(bookDir, "Description.txt"), []byte(desc), 0644)
}

func downloadPlaylist(bookDir string, bookData *BookData, result *DownloadResult) error {
	// Try main playlist first
	if err := downloadTracks(bookDir, bookData.Playlist); err == nil {
		return nil
	}

	// If main playlist fails, try merged playlist
	result.addError("main playlist", errors.New("falling back to merged playlist"))
	return downloadTracks(bookDir, bookData.MergedPlaylist)
}

func downloadTracks(bookDir string, tracks []Audio) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	errChan := make(chan error, 1)
	sem := make(chan struct{}, 5) // Limit concurrent downloads

	for _, track := range tracks {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		wg.Add(1)
		go func(t Audio) {
			defer wg.Done()

			// Acquire semaphore
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			// Prepare file path
			ext := filepath.Ext(t.URL)
			if ext == "" {
				ext = ".mp3"
			}
			filePath := filepath.Join(bookDir, sanitizePath(t.Title)+ext)

			// Skip existing files
			if _, err := os.Stat(filePath); err == nil {
				return
			}

			// Download with retries
			for attempt := 1; attempt <= maxRetries; attempt++ {
				if err := downloadFile(t.URL, filePath); err == nil {
					return
				}
				time.Sleep(retryDelay)
			}

			// Report error
			select {
			case errChan <- fmt.Errorf("track %s: download failed after %d attempts", t.Title, maxRetries):
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

func downloadFile(url, filePath string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}

	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	return err
}

func normalizeURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}

	// Normalize to https and remove mobile subdomain
	if u.Host == "m.knigavuhe.org" {
		u.Host = "knigavuhe.org"
	}
	u.Scheme = "https"

	return u.String(), nil
}

// DownloadResult contains download status
type DownloadResult struct {
	URL      string
	BookName string
	Path     string
	Errors   []string
}

func (r *DownloadResult) addError(context string, err error) {
	r.Errors = append(r.Errors, fmt.Sprintf("%s: %v", context, err))
}
