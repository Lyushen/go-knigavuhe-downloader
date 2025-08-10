# Go Knigavuhe Downloader

A fast, concurrent audiobook downloader for knigavuhe.org written in Go. Downloads entire audiobooks including cover art, descriptions, and all audio tracks with retry logic and parallel processing.

**RU:** 
- Без графического интерфейса, зато есть скорость, параллельность и минимализм
- Не нужно устанавливать дополнительные программы, исполняемый файл для Windows/Linux содержит всё необходимое
- Собирается из актуального кода с помощью GitHub Actions при каждом релизе, что обеспечивает безопасность
- Использует всю доступную пропускную способность интернет-канала во время скачивания
- Позволяет скачивать десятки книг одновременно

## Features

- **Concurrent Downloads**: Multi-threaded downloading with worker pools
- **Retry Logic**: Automatic retries for failed downloads (up to 20 attempts)
- **Complete Package**: Downloads cover art, descriptions, and all audio tracks
- **Fallback Support**: Uses alternative sources when primary downloads fail
- **Batch Processing**: Process multiple books from a URL file
- **Cross-Platform**: Pre-built binaries for Linux and Windows
- **Resume Downloading**: Scanned all downloaded files and resume from interrupted state

## Quick Start

### Download Pre-built Binaries

Get the latest release from the [releases page](https://github.com/Lyushen/go-knigavuhe-downloader/releases#latest).

### Usage

```bash
./go-knigavuhe <output-directory> <url-file>
```

**Example:**
```bash
./go-knigavuhe ./audiobooks urls.txt
```

Where `urls.txt` contains:
```
https://knigavuhe.org/book/some-audiobook/
https://knigavuhe.org/book/another-book/
```

## Build from Source

### Prerequisites
- Go 1.19 or higher
- Precompiled on 1.24.3

### Build Structure
```
cmd/
  go-knigavuhe/
    main.go
```

### Build Commands

**Linux/macOS:**
```bash
go build -o go-knigavuhe ./cmd/go-knigavuhe
```

**Windows:**
```bash
go build -o go-knigavuhe.exe ./cmd/go-knigavuhe
```

**Cross-compile for all platforms:**
```bash
# Linux
GOOS=linux GOARCH=amd64 go build -o go-knigavuhe-linux ./cmd/go-knigavuhe

# Windows
GOOS=windows GOARCH=amd64 go build -o go-knigavuhe-windows.exe ./cmd/go-knigavuhe
```

## Output Structure

Each audiobook is downloaded to its own directory:
```
output-directory/
├── Book Title 1/
│   ├── cover.jpg
│   ├── Description.txt
│   ├── Chapter 01.mp3
│   ├── Chapter 02.mp3
│   └── ...
└── Book Title 2/
    ├── cover.jpg
    ├── Description.txt
    └── ...
```

## Technical Details

- **Concurrent Workers**: Uses CPU count × 2 workers for optimal performance
- **Download Limits**: Maximum 5 simultaneous file downloads per book
- **Timeout**: 30-second HTTP timeout with connection pooling
- **Sanitization**: Automatically handles forbidden filesystem characters
- **Error Handling**: Comprehensive error reporting with context

## GitHub Actions

Pre-built binaries are automatically generated for Linux and Windows using GitHub Actions on every release.

## License

MIT License - see LICENSE file for details.