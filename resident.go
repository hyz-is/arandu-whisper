package whisper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"time"
)

// maxServerReply bounds what is read back from whisper-server: a transcript is
// text, so a megabyte is far more than five minutes of speech can make.
const maxServerReply = 4 << 20

// serverReply is the part of whisper-server's verbose_json answer this package
// reads: the language, as a name ("portuguese"), and the segments with their
// positions in seconds.
type serverReply struct {
	Language string `json:"language"`
	Text     string `json:"text"`
	Segments []struct {
		Start float64 `json:"start"`
		End   float64 `json:"end"`
		Text  string  `json:"text"`
	} `json:"segments"`
}

// languageCodes turns the names whisper-server reports into the codes the rest
// of this package uses. A name that is not here is returned as it came.
var languageCodes = map[string]string{
	"portuguese": "pt", "english": "en", "spanish": "es", "french": "fr",
	"german": "de", "italian": "it", "japanese": "ja", "chinese": "zh",
	"russian": "ru", "dutch": "nl", "arabic": "ar", "korean": "ko",
}

// recognizeOnServer posts the converted WAV to the resident whisper-server and
// reads the transcript from its answer. Nothing is written to disk: the audio
// leaves this process only for the server named by Config.ServerURL.
func (c *Client) recognizeOnServer(ctx context.Context, wavPath string) (Transcript, error) {
	wav, err := os.ReadFile(wavPath)
	if err != nil {
		return Transcript{}, ErrUnsupportedAudio
	}

	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="file"; filename="audio.wav"`)
	hdr.Set("Content-Type", "audio/wav")
	part, err := w.CreatePart(hdr)
	if err != nil {
		return Transcript{}, fmt.Errorf("whisper: building the request: %w", err)
	}
	if _, err := part.Write(wav); err != nil {
		return Transcript{}, fmt.Errorf("whisper: building the request: %w", err)
	}
	fields := [][2]string{{"response_format", "verbose_json"}, {"temperature", "0.0"}, {"language", c.language}}
	if c.prompt != "" {
		fields = append(fields, [2]string{"prompt", c.prompt})
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return Transcript{}, fmt.Errorf("whisper: building the request: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return Transcript{}, fmt.Errorf("whisper: building the request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.server+"/inference", &body)
	if err != nil {
		return Transcript{}, fmt.Errorf("whisper: building the request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := serverClient.Do(req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return Transcript{}, fmt.Errorf("%w: %w", ErrTimeout, ctxErr)
			}
			return Transcript{}, ctxErr
		}
		return Transcript{}, &ProcessError{Program: "whisper-server", ExitCode: -1, Stderr: summarize(urlFreeError(err), "<none>"), kind: ErrFailed}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxServerReply))
	if err != nil {
		return Transcript{}, ErrBadOutput
	}
	if resp.StatusCode != http.StatusOK {
		return Transcript{}, &ProcessError{Program: "whisper-server", ExitCode: resp.StatusCode, Stderr: "http status " + resp.Status, kind: ErrFailed}
	}
	var reply serverReply
	if json.Unmarshal(raw, &reply) != nil {
		return Transcript{}, ErrBadOutput
	}
	return fromServer(reply, c.language), nil
}

func fromServer(reply serverReply, language string) Transcript {
	t := Transcript{Language: language}
	if language == "auto" {
		name := strings.ToLower(strings.TrimSpace(reply.Language))
		if code, ok := languageCodes[name]; ok {
			name = code
		}
		t.Language = name
	}
	var parts []string
	for _, s := range reply.Segments {
		text := strings.TrimSpace(s.Text)
		if text == "" || strings.EqualFold(text, "[BLANK_AUDIO]") {
			continue
		}
		parts = append(parts, text)
		t.Segments = append(t.Segments, Segment{
			Start: time.Duration(s.Start * float64(time.Second)),
			End:   time.Duration(s.End * float64(time.Second)),
			Text:  text,
		})
	}
	if len(parts) == 0 && len(reply.Segments) == 0 {
		// A server that answers no segments still has the whole text.
		if text := strings.TrimSpace(reply.Text); text != "" && !strings.EqualFold(text, "[BLANK_AUDIO]") {
			parts = append(parts, text)
		}
	}
	t.Text = strings.Join(parts, " ")
	return t
}

// urlFreeError keeps an error's text without the address it carries.
func urlFreeError(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap().Error()
	}
	return err.Error()
}

// serverClient has no timeout of its own: the call's context ends the request.
var serverClient = &http.Client{}
