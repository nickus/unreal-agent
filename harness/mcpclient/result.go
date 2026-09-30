package mcpclient

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/unreallabsai/unreal-agent/harness/operation"
)

const (
	completeResultFilename = "result.txt"
	// contentExcerptLength bounds content blocks of types the client does not
	// know, which are shown as JSON.
	contentExcerptLength = 2_000
)

// saveFunc stores content a result refers to and returns its path.
type saveFunc func(name string, data []byte) (string, error)

type contentBlock struct {
	Type        string            `json:"type"`
	Text        string            `json:"text"`
	Data        string            `json:"data"`
	MIMEType    string            `json:"mimeType"`
	URI         string            `json:"uri"`
	Name        string            `json:"name"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Resource    *embeddedResource `json:"resource"`
}

type embeddedResource struct {
	URI      string  `json:"uri"`
	MIMEType string  `json:"mimeType"`
	Text     *string `json:"text"`
	Blob     *string `json:"blob"`
}

// renderResult turns a tools/call result into text for the model. Text blocks
// are kept as they are; binary content is saved through save, when set, and
// described by its path. Structured content is added as JSON unless a text
// block already carries the same value.
func renderResult(result CallToolResult, save saveFunc) string {
	var parts []string
	var texts []string
	for index, raw := range result.Content {
		var block contentBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			parts = append(parts, "[unreadable content block: "+truncateRunes(compactJSON(raw), contentExcerptLength)+"]")
			continue
		}
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
			texts = append(texts, block.Text)
		case "image", "audio":
			parts = append(parts, describeBinary(block.Type+" content", block.MIMEType, block.Data, fmt.Sprintf("content-%d", index+1), save))
		case "resource_link":
			parts = append(parts, describeLink(block))
		case "resource":
			parts = append(parts, describeResource(block.Resource, fmt.Sprintf("content-%d", index+1), save))
		default:
			parts = append(parts, fmt.Sprintf("[%s content: %s]", block.Type, truncateRunes(compactJSON(raw), contentExcerptLength)))
		}
	}
	if structured := compactJSON(result.StructuredContent); structured != "" && structured != "null" &&
		!sameJSON(result.StructuredContent, texts) {
		if len(parts) == 0 {
			parts = append(parts, structured)
		} else {
			parts = append(parts, "Structured content: "+structured)
		}
	}
	if len(parts) == 0 {
		return "The tool returned no content."
	}
	return strings.Join(parts, "\n")
}

func describeBinary(kind, mimeType, encoded, name string, save saveFunc) string {
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	data, err := decodeBase64(encoded)
	if err != nil {
		return fmt.Sprintf("[%s (%s) with invalid base64 data]", kind, mimeType)
	}
	description := fmt.Sprintf("%s (%s, %d bytes)", kind, mimeType, len(data))
	if save == nil {
		return "[" + description + " not shown]"
	}
	path, err := save(name+extension(mimeType), data)
	if err != nil {
		return fmt.Sprintf("[%s could not be saved: %v]", description, err)
	}
	return fmt.Sprintf("[%s saved to %s]", description, path)
}

func describeLink(block contentBlock) string {
	label := block.Title
	if label == "" {
		label = block.Name
	}
	text := "[resource link"
	if label != "" {
		text += ": " + label
	}
	text += " <" + block.URI + ">"
	if block.MIMEType != "" {
		text += " (" + block.MIMEType + ")"
	}
	if block.Description != "" {
		text += " " + block.Description
	}
	return text + "]"
}

func describeResource(resource *embeddedResource, name string, save saveFunc) string {
	if resource == nil {
		return "[resource content without a resource]"
	}
	if resource.Text != nil {
		return "[resource " + resource.URI + "]\n" + *resource.Text
	}
	if resource.Blob != nil {
		return describeBinary("resource "+resource.URI, resource.MIMEType, *resource.Blob, name, save)
	}
	return "[resource " + resource.URI + " without content]"
}

// sameJSON reports whether one of texts is JSON equal to value.
func sameJSON(value jsontext.Value, texts []string) bool {
	canonical := value.Clone()
	if canonical.Canonicalize() != nil {
		return false
	}
	for _, text := range texts {
		candidate := jsontext.Value(strings.TrimSpace(text))
		if candidate.Canonicalize() == nil && bytes.Equal(candidate, canonical) {
			return true
		}
	}
	return false
}

func decodeBase64(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if data, err := base64.StdEncoding.DecodeString(encoded); err == nil {
		return data, nil
	}
	return base64.RawStdEncoding.DecodeString(encoded)
}

var extensions = map[string]string{
	"application/json": ".json",
	"application/pdf":  ".pdf",
	"audio/mpeg":       ".mp3",
	"audio/ogg":        ".ogg",
	"audio/wav":        ".wav",
	"audio/webm":       ".webm",
	"image/gif":        ".gif",
	"image/jpeg":       ".jpg",
	"image/png":        ".png",
	"image/svg+xml":    ".svg",
	"image/webp":       ".webp",
	"text/csv":         ".csv",
	"text/html":        ".html",
	"text/markdown":    ".md",
	"text/plain":       ".txt",
}

func extension(mimeType string) string {
	base, _, _ := strings.Cut(strings.ToLower(mimeType), ";")
	if extension, ok := extensions[strings.TrimSpace(base)]; ok {
		return extension
	}
	return ".bin"
}

// saver stores files for operation id under the jobs directory.
func (jobs *Jobs) saver(id operation.ID) saveFunc {
	if jobs.directory == "" {
		return nil
	}
	return func(name string, data []byte) (string, error) {
		component := string(id)
		if component == "" || component == "." || component == ".." || filepath.Base(component) != component {
			return "", fmt.Errorf("operation ID %q is not a path component", id)
		}
		directory := filepath.Join(jobs.directory, component)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return "", err
		}
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", err
		}
		return path, nil
	}
}

// keepComplete saves a result longer than the operation's output limit, which
// the operation keeps only the head and tail of, and says where it is.
func (jobs *Jobs) keepComplete(current operation.Operation, text string) string {
	if utf8.RuneCountInString(text) <= current.MaxOutputLength {
		return text
	}
	save := jobs.saver(current.ID)
	if save == nil {
		return text
	}
	path, err := save(completeResultFilename, []byte(text))
	if err != nil {
		return fmt.Sprintf("The complete result (%d bytes) could not be saved: %v\n%s", len(text), err, text)
	}
	return fmt.Sprintf("The complete result (%d bytes) is in %s.\n%s", len(text), path, text)
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}
