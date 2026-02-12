package storage

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/coder/coder/v2/codersdk"
	"github.com/google/uuid"
)

const (
	maxTemplateSourceZipBytes               = 20 << 20 // 20 MiB compressed
	maxTemplateSourceTotalUncompressedBytes = 40 << 20 // 40 MiB total extracted
	maxTemplateSourceFiles                  = 2000
	maxTemplateSourceFileBytes              = 2 << 20 // 2 MiB per file
)

// fetchTemplateSourceFiles downloads the source archive for a template version and
// returns a map of relative file paths to UTF-8 file contents.
func fetchTemplateSourceFiles(ctx context.Context, sdk *codersdk.Client, versionID uuid.UUID) (map[string]string, error) {
	if ctx == nil {
		return nil, fmt.Errorf("assertion failed: context must not be nil")
	}
	if sdk == nil {
		return nil, fmt.Errorf("assertion failed: codersdk client must not be nil")
	}
	if versionID == uuid.Nil {
		return nil, fmt.Errorf("assertion failed: template version ID must not be nil")
	}

	version, err := sdk.TemplateVersion(ctx, versionID)
	if err != nil {
		return nil, fmt.Errorf("fetch template version %q: %w", versionID, err)
	}
	if version.Job.FileID == uuid.Nil {
		return nil, fmt.Errorf("assertion failed: template version %q job.fileID must not be nil", versionID)
	}

	sourceZip, _, err := sdk.DownloadWithFormat(ctx, version.Job.FileID, codersdk.FormatZip)
	if err != nil {
		return nil, fmt.Errorf("download template source zip for file %q: %w", version.Job.FileID, err)
	}
	if len(sourceZip) > maxTemplateSourceZipBytes {
		return nil, fmt.Errorf("template source zip exceeds max size: %d > %d", len(sourceZip), maxTemplateSourceZipBytes)
	}

	archiveReader, err := zip.NewReader(bytes.NewReader(sourceZip), int64(len(sourceZip)))
	if err != nil {
		return nil, fmt.Errorf("open template source zip: %w", err)
	}
	if len(archiveReader.File) > maxTemplateSourceFiles {
		return nil, fmt.Errorf("template source zip contains too many entries: %d > %d", len(archiveReader.File), maxTemplateSourceFiles)
	}

	files := make(map[string]string, len(archiveReader.File))
	totalUncompressedBytes := int64(0)

	for _, archiveFile := range archiveReader.File {
		if archiveFile == nil {
			return nil, fmt.Errorf("assertion failed: template source zip entry must not be nil")
		}
		if archiveFile.FileInfo().IsDir() {
			continue
		}

		relativePath, err := validateTemplateSourcePath(archiveFile.Name)
		if err != nil {
			return nil, fmt.Errorf("validate template source path %q: %w", archiveFile.Name, err)
		}
		if archiveFile.UncompressedSize64 > uint64(maxTemplateSourceFileBytes) {
			return nil, fmt.Errorf(
				"template source file %q exceeds max file size: %d > %d",
				relativePath,
				archiveFile.UncompressedSize64,
				maxTemplateSourceFileBytes,
			)
		}

		entryReader, err := archiveFile.Open()
		if err != nil {
			return nil, fmt.Errorf("open template source file %q: %w", relativePath, err)
		}

		contents, readErr := io.ReadAll(io.LimitReader(entryReader, maxTemplateSourceFileBytes+1))
		closeErr := entryReader.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read template source file %q: %w", relativePath, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close template source file %q: %w", relativePath, closeErr)
		}
		if len(contents) > maxTemplateSourceFileBytes {
			return nil, fmt.Errorf("template source file %q exceeds max file size: %d > %d", relativePath, len(contents), maxTemplateSourceFileBytes)
		}

		totalUncompressedBytes += int64(len(contents))
		if totalUncompressedBytes > maxTemplateSourceTotalUncompressedBytes {
			return nil, fmt.Errorf(
				"template source files exceed max total size: %d > %d",
				totalUncompressedBytes,
				maxTemplateSourceTotalUncompressedBytes,
			)
		}

		if !utf8.Valid(contents) {
			continue
		}
		files[relativePath] = string(contents)
	}

	return files, nil
}

// buildSourceZip creates an in-memory zip archive from a file map.
func buildSourceZip(files map[string]string) ([]byte, error) {
	if files == nil {
		return nil, fmt.Errorf("assertion failed: files map must not be nil")
	}
	if len(files) > maxTemplateSourceFiles {
		return nil, fmt.Errorf("template source file count exceeds limit: %d > %d", len(files), maxTemplateSourceFiles)
	}

	normalizedFiles := make(map[string]string, len(files))
	paths := make([]string, 0, len(files))
	totalUncompressedBytes := int64(0)

	for requestedPath, content := range files {
		normalizedPath, err := validateTemplateSourcePath(requestedPath)
		if err != nil {
			return nil, fmt.Errorf("validate template source path %q: %w", requestedPath, err)
		}
		if _, exists := normalizedFiles[normalizedPath]; exists {
			return nil, fmt.Errorf("duplicate normalized template source path %q", normalizedPath)
		}
		if !utf8.ValidString(content) {
			return nil, fmt.Errorf("template source file %q contains invalid UTF-8", normalizedPath)
		}
		if len(content) > maxTemplateSourceFileBytes {
			return nil, fmt.Errorf("template source file %q exceeds max file size: %d > %d", normalizedPath, len(content), maxTemplateSourceFileBytes)
		}

		totalUncompressedBytes += int64(len(content))
		if totalUncompressedBytes > maxTemplateSourceTotalUncompressedBytes {
			return nil, fmt.Errorf(
				"template source files exceed max total size: %d > %d",
				totalUncompressedBytes,
				maxTemplateSourceTotalUncompressedBytes,
			)
		}

		normalizedFiles[normalizedPath] = content
		paths = append(paths, normalizedPath)
	}

	sort.Strings(paths)

	var buffer bytes.Buffer
	zipWriter := zip.NewWriter(&buffer)

	for _, sourcePath := range paths {
		fileWriter, err := zipWriter.Create(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("create zip entry %q: %w", sourcePath, err)
		}
		if _, err := fileWriter.Write([]byte(normalizedFiles[sourcePath])); err != nil {
			return nil, fmt.Errorf("write zip entry %q: %w", sourcePath, err)
		}
	}

	if err := zipWriter.Close(); err != nil {
		return nil, fmt.Errorf("close source zip writer: %w", err)
	}

	result := buffer.Bytes()
	if len(result) > maxTemplateSourceZipBytes {
		return nil, fmt.Errorf("template source zip exceeds max size: %d > %d", len(result), maxTemplateSourceZipBytes)
	}

	return result, nil
}

func validateTemplateSourcePath(templatePath string) (string, error) {
	if templatePath == "" {
		return "", fmt.Errorf("path must not be empty")
	}

	cleanedPath := path.Clean(templatePath)
	if cleanedPath == "." || cleanedPath == "/" {
		return "", fmt.Errorf("path must not resolve to root")
	}
	if strings.HasPrefix(cleanedPath, "/") {
		return "", fmt.Errorf("path must be relative")
	}
	if cleanedPath == ".." || strings.HasPrefix(cleanedPath, "../") {
		return "", fmt.Errorf("path must not escape root")
	}
	for _, component := range strings.Split(cleanedPath, "/") {
		if component == ".." {
			return "", fmt.Errorf("path must not contain parent directory components")
		}
	}

	return cleanedPath, nil
}
