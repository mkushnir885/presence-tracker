package challenger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"presence-tracker/src/internal/config"
)

// asrTimeout bounds one ASR round-trip. Whisper-class models on local
// hardware take a few seconds per minute of audio; a generous cap covers
// cold starts without hanging the session.
const asrTimeout = 2 * time.Minute

// ASRClient calls an OpenAI-compatible /v1/audio/transcriptions endpoint.
type ASRClient struct {
	baseURL  string
	apiKey   string
	model    string
	language string
	http     *http.Client
}

// audioFilename picks a friendly filename for the multipart "file"
// part based on the audio MIME type. Some backends sniff the extension
// to pick a decoder.
func audioFilename(mime string) string {
	switch {
	case strings.Contains(mime, "wav"):
		return "audio.wav"
	case strings.Contains(mime, "ogg"):
		return "audio.ogg"
	case strings.Contains(mime, "mp3"), strings.Contains(mime, "mpeg"):
		return "audio.mp3"
	default:
		return "audio.webm"
	}
}

// NewASRClient builds an ASRClient. language is the BCP-47 / ISO 639-1
// hint sent on every request (Whisper's `language` parameter); pass ""
// to let the backend auto-detect, at the cost of accuracy on non-English
// speech. The returned client is safe for concurrent use.
func NewASRClient(cfg config.AIBackendConfig, language string) *ASRClient {
	return &ASRClient{
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:   cfg.APIKey,
		model:    cfg.Model,
		language: asrLanguage(language),
		http:     &http.Client{Timeout: asrTimeout},
	}
}

// asrLanguage reduces a BCP-47 tag to the primary subtag — Whisper's
// `language` parameter is defined in ISO 639-1, so `uk-UA` and `en-US`
// must shed their region before they hit the backend. The empty string
// and the "auto" sentinel both opt out of the hint so the backend
// detects the language itself.
func asrLanguage(tag string) string {
	primary, _, _ := strings.Cut(strings.TrimSpace(tag), "-")
	primary = strings.ToLower(primary)
	if primary == "auto" {
		return ""
	}
	return primary
}

// Transcribe posts audio to <base_url>/v1/audio/transcriptions and
// returns the transcript text. mime is the Content-Type of the audio
// payload (e.g. "audio/webm"); it is used as the multipart file part's
// Content-Type so the backend can pick the right decoder.
func (c *ASRClient) Transcribe(ctx context.Context, audio io.Reader, mime string) (string, error) {
	if c.baseURL == "" {
		return "", errors.New("challenger: ASR base_url not configured")
	}

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	filename := audioFilename(mime)

	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename=%q`, filename)}
	if mime != "" {
		hdr["Content-Type"] = []string{mime}
	}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		return "", fmt.Errorf("challenger: asr multipart: %w", err)
	}
	if _, err := io.Copy(part, audio); err != nil {
		return "", fmt.Errorf("challenger: asr read audio: %w", err)
	}
	if c.model != "" {
		if err := mw.WriteField("model", c.model); err != nil {
			return "", fmt.Errorf("challenger: asr write model: %w", err)
		}
	}
	if c.language != "" {
		if err := mw.WriteField("language", c.language); err != nil {
			return "", fmt.Errorf("challenger: asr write language: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("challenger: asr close multipart: %w", err)
	}

	url := c.baseURL + "/v1/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", fmt.Errorf("challenger: asr request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("challenger: asr post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("challenger: asr read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("challenger: asr HTTP %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}

	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", fmt.Errorf("challenger: asr decode: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}
