package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/relayconvert"
	"github.com/QuantumNous/new-api/relaykit/relayconvert/convmeta"
	"github.com/QuantumNous/new-api/relaykit/types"
)

// FormatFor maps a provider protocol string to a relaykit RelayFormat.
func FormatFor(protocol string) (types.RelayFormat, error) {
	switch protocol {
	case "openai":
		return types.RelayFormatOpenAI, nil
	case "claude":
		return types.RelayFormatClaude, nil
	case "gemini":
		return types.RelayFormatGemini, nil
	}
	return "", fmt.Errorf("unknown protocol %q", protocol)
}

// ParseClientRequest deserializes a client request body into the relaykit DTO
// for the given protocol. Relaykit infers source format from the DTO type.
func ParseClientRequest(format types.RelayFormat, body []byte) (any, error) {
	switch format {
	case types.RelayFormatOpenAI:
		var r dto.GeneralOpenAIRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case types.RelayFormatClaude:
		var r dto.ClaudeRequest
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		return &r, nil
	}
	return nil, fmt.Errorf("unsupported client format %q", format)
}

func defaultMaxTokens(model string) int { return 4096 }

func newMeta(originModel, upstreamModel string, isStream bool) *convmeta.Values {
	return &convmeta.Values{
		OriginModelName:     originModel,
		UpstreamModelName:   upstreamModel,
		ChannelMetaAttached: true,
		IsStream:            isStream,
		Options: &convmeta.Options{
			Claude: convmeta.ClaudeOptions{DefaultMaxTokens: defaultMaxTokens},
		},
	}
}

// ConvertRequestBody rewrites body for the upstream protocol and native model
// name. Same-protocol is a cheap passthrough with only the model field swapped.
type ConversionLogger func(msg string, kv ...any)

func ConvertRequestBody(ctx context.Context, clientFmt, upstreamFmt types.RelayFormat,
	canonical, nativeModel string, body []byte, isStream bool, log ConversionLogger) ([]byte, error) {

	if clientFmt == upstreamFmt {
		// rewrite model field only
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, err
		}
		mb, _ := json.Marshal(nativeModel)
		m["model"] = mb
		return json.Marshal(m)
	}

	req, err := ParseClientRequest(clientFmt, body)
	if err != nil {
		return nil, fmt.Errorf("parse client request: %w", err)
	}
	res, err := relayconvert.ConvertRequest(ctx, newMeta(canonical, nativeModel, isStream), upstreamFmt, req)
	if err != nil {
		return nil, fmt.Errorf("convert request %s->%s: %w", clientFmt, upstreamFmt, err)
	}
	if log != nil && len(res.Diagnostics) > 0 {
		log("conversion diagnostics", "from", string(res.From), "to", string(res.To), "quality", string(res.Quality), "diagnostics", fmt.Sprint(res.Diagnostics))
	}
	out, err := json.Marshal(res.Value)
	if err != nil {
		return nil, err
	}
	// relaykit keeps the source model name; swap to the upstream-native name
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	mb, _ := json.Marshal(nativeModel)
	m["model"] = mb
	return json.Marshal(m)
}

// parseResponseBody unmarshals a complete upstream response into its DTO.
func parseResponseBody(format types.RelayFormat, body []byte) (any, error) {
	switch format {
	case types.RelayFormatOpenAI:
		var r dto.OpenAITextResponse
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case types.RelayFormatClaude:
		var r dto.ClaudeResponse
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, err
		}
		return &r, nil
	}
	return nil, fmt.Errorf("unsupported response format %q", format)
}

// ConvertResponseBody converts a complete (non-stream) upstream response body
// to the client protocol. Same-format bodies pass through untouched.
func ConvertResponseBody(ctx context.Context, upstreamFmt, clientFmt types.RelayFormat,
	model string, body []byte, log ConversionLogger) ([]byte, error) {

	if upstreamFmt == clientFmt {
		return body, nil
	}
	resp, err := parseResponseBody(upstreamFmt, body)
	if err != nil {
		return nil, fmt.Errorf("parse upstream response: %w", err)
	}
	res, err := relayconvert.ConvertResponse(ctx, newMeta(model, model, false), clientFmt, resp)
	if err != nil {
		return nil, fmt.Errorf("convert response %s->%s: %w", upstreamFmt, clientFmt, err)
	}
	if log != nil && len(res.Diagnostics) > 0 {
		log("conversion diagnostics", "from", string(res.From), "to", string(res.To), "quality", string(res.Quality), "diagnostics", fmt.Sprint(res.Diagnostics))
	}
	out, err := json.Marshal(res.Value)
	if err != nil {
		return nil, err
	}
	// relaykit keeps the upstream payload's model; swap to the canonical name
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	mb, _ := json.Marshal(model)
	m["model"] = mb
	return json.Marshal(m)
}

// parseStreamChunk unmarshals one SSE data payload into the upstream's stream DTO.
func parseStreamChunk(format types.RelayFormat, data []byte) (any, error) {
	switch format {
	case types.RelayFormatOpenAI:
		var r dto.ChatCompletionsStreamResponse
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return &r, nil
	case types.RelayFormatClaude:
		var r dto.ClaudeResponse
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, err
		}
		return &r, nil
	}
	return nil, fmt.Errorf("unsupported stream format %q", format)
}

// StreamConvert copies an upstream SSE stream to w, converting frames when the
// protocols differ. Same-protocol streams pass through with per-chunk flush.
func StreamConvert(ctx context.Context, upstreamFmt, clientFmt types.RelayFormat,
	model string, src io.Reader, w http.ResponseWriter, log ConversionLogger) error {

	if upstreamFmt == clientFmt {
		return streamPassthrough(src, w)
	}

	state, err := relayconvert.NewResponseStreamState(upstreamFmt, clientFmt,
		relayconvert.ResponseStreamOptions{Model: model})
	if err != nil {
		return err
	}
	meta := newMeta(model, model, true)

	fl, _ := w.(http.Flusher)
	writeResult := func(r relayconvert.ResponseResult) error {
		b, err := json.Marshal(r.Value)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	sc := newSSEScanner(src)
	for sc.Next() {
		data := sc.Data()
		if bytes.Equal(data, []byte("[DONE]")) {
			break
		}
		chunk, err := parseStreamChunk(upstreamFmt, data)
		if err != nil {
			continue // unparseable keep-alive / unknown event; skip
		}
		results, err := relayconvert.ConvertStreamResponseChunk(ctx, meta, state, chunk)
		if err != nil {
			continue
		}
		for _, r := range results {
			if err := writeResult(r); err != nil {
				return err
			}
		}
	}
	finals, err := relayconvert.FinalizeStreamResponse(ctx, meta, state)
	if err == nil {
		for _, r := range finals {
			if err := writeResult(r); err != nil {
				return err
			}
		}
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
	return nil
}

// streamPassthrough copies with per-read flush; never io.Copy (it buffers).
func streamPassthrough(src io.Reader, w http.ResponseWriter) error {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return nil
		}
	}
}

// sseScanner yields SSE "data:" payloads line by line.
type sseScanner struct {
	r   io.Reader
	buf []byte
	err error
}

func newSSEScanner(r io.Reader) *sseScanner {
	return &sseScanner{r: r}
}

func (s *sseScanner) Next() bool {
	if s.err != nil {
		return false
	}
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := s.r.Read(b)
		if n > 0 {
			if b[0] == '\n' {
				break
			}
			line = append(line, b[0])
		}
		if err != nil {
			s.err = err
			if len(line) == 0 {
				return false
			}
			break
		}
	}
	// strip trailing \r and trailing blank-line handling
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		// event:/id:/comment lines: skip, try next
		return s.Next()
	}
	data := bytes.TrimPrefix(line, []byte("data:"))
	data = bytes.TrimSpace(data)
	s.buf = data
	return true
}

func (s *sseScanner) Data() []byte { return s.buf }
